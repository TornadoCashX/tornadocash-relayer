package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// ozClient is a minimal client for the OpenZeppelin Relayer v1 HTTP API.
// OZ owns the signing key, nonce, broadcast, fee bumping and receipt tracking;
// this service only submits a fully preflighted transaction and polls it.
type ozClient struct {
	baseURL    string
	relayerID  string
	apiKey     string
	httpClient *http.Client
}

const (
	ozRequestTimeout = 30 * time.Second
	// maxOZResponseBytes bounds how much of an OZ response body is read.
	maxOZResponseBytes = 4 << 20
	// ozScanPageSize and ozMaxScanPages bound FindTransaction's scan.
	ozScanPageSize = 100
	ozMaxScanPages = 50
)

// errOZNotFound is a definitive 404 from OZ (unknown id, or a terminal
// transaction that OZ already pruned).
var errOZNotFound = errors.New("OpenZeppelin Relayer: not found")

// OZ transaction statuses (lowercase on the wire).
const (
	ozStatusPending   = "pending"
	ozStatusSent      = "sent"
	ozStatusSubmitted = "submitted"
	ozStatusMined     = "mined"
	ozStatusConfirmed = "confirmed"
	ozStatusFailed    = "failed"
	ozStatusCanceled  = "canceled"
	ozStatusExpired   = "expired"
)

type ozTransaction struct {
	ID           string `json:"id"`
	Hash         string `json:"hash"`
	Status       string `json:"status"`
	StatusReason string `json:"status_reason"`
	CreatedAt    string `json:"created_at"`
	From         string `json:"from"`
	To           string `json:"to"`
	Data         string `json:"data"`
	// Nonce is shared by the original transaction, every fee-bump replacement
	// and any NOOP OZ sends in its place. Empty until OZ assigns it.
	Nonce json.Number `json:"nonce"`
}

// ozFinalStatus reports whether OZ will never touch the transaction again.
func ozFinalStatus(status string) bool {
	switch status {
	case ozStatusConfirmed, ozStatusFailed, ozStatusCanceled, ozStatusExpired:
		return true
	}
	return false
}

type ozEVMPolicy struct {
	GasPriceCap        json.Number `json:"gas_price_cap"`
	WhitelistReceivers []string    `json:"whitelist_receivers"`
	EIP1559Pricing     *bool       `json:"eip1559_pricing"`
	GasLimitEstimation *bool       `json:"gas_limit_estimation"`
}

type ozRelayer struct {
	ID             string      `json:"id"`
	NetworkType    string      `json:"network_type"`
	Address        string      `json:"address"`
	Paused         bool        `json:"paused"`
	SystemDisabled bool        `json:"system_disabled"`
	Policies       ozEVMPolicy `json:"policies"`
}

type ozRelayerStatus struct {
	Balance                 string `json:"balance"`
	PendingTransactionCount uint64 `json:"pending_transactions_count"`
	SystemDisabled          bool   `json:"system_disabled"`
	Paused                  bool   `json:"paused"`
	Nonce                   string `json:"nonce"`
}

// ozSubmitRequest is the EVM transaction request. Fee fields are u128 JSON
// numbers on the wire, so they are encoded as json.Number.
type ozSubmitRequest struct {
	To                   string      `json:"to"`
	Value                json.Number `json:"value"`
	Data                 string      `json:"data"`
	GasLimit             uint64      `json:"gas_limit"`
	MaxFeePerGas         json.Number `json:"max_fee_per_gas"`
	MaxPriorityFeePerGas json.Number `json:"max_priority_fee_per_gas"`
	ValidUntil           string      `json:"valid_until"`
}

type ozEnvelope struct {
	Success    bool            `json:"success"`
	Data       json.RawMessage `json:"data"`
	Error      json.RawMessage `json:"error"`
	Pagination *struct {
		TotalItems uint64 `json:"total_items"`
	} `json:"pagination"`
}

func newOZClient(cfg Config) *ozClient {
	return &ozClient{
		baseURL:    cfg.OZRelayerURL,
		relayerID:  cfg.OZRelayerID,
		apiKey:     cfg.OZAPIKey,
		httpClient: &http.Client{Timeout: ozRequestTimeout},
	}
}

func (c *ozClient) relayerPath(suffix string) string {
	return "/api/v1/relayers/" + url.PathEscape(c.relayerID) + suffix
}

// do performs one request. A non-2xx answer is an error; 404 maps to
// errOZNotFound. The body of a 2xx answer is returned raw.
func (c *ozClient) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OpenZeppelin Relayer %s %s: %w", method, path, redactURLError(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOZResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("OpenZeppelin Relayer %s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errOZNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &ozHTTPError{status: resp.StatusCode, message: ozErrorMessage(raw)}
	}
	return raw, nil
}

type ozHTTPError struct {
	status  int
	message string
}

func (e *ozHTTPError) Error() string {
	return fmt.Sprintf("OpenZeppelin Relayer HTTP %d: %s", e.status, e.message)
}

// definitiveRejection reports a 4xx answer: OZ refused the request and did not
// create a transaction (429 is a rate limit and therefore not definitive).
func (e *ozHTTPError) definitiveRejection() bool {
	return e.status >= 400 && e.status < 500 && e.status != http.StatusTooManyRequests && e.status != http.StatusRequestTimeout
}

// targets reports whether tx calls to with exactly data. It is how an OZ
// transaction is matched to a withdraw job, both when recovering an unknown
// submit and when checking a confirmed answer.
func (tx *ozTransaction) targets(to common.Address, data string) bool {
	return common.IsHexAddress(tx.To) && common.HexToAddress(tx.To) == to && strings.EqualFold(tx.Data, data)
}

func ozErrorMessage(raw []byte) string {
	var envelope ozEnvelope
	if json.Unmarshal(raw, &envelope) == nil && len(envelope.Error) > 0 {
		var text string
		if json.Unmarshal(envelope.Error, &text) == nil {
			return redactSensitive(text)
		}
		return redactSensitive(string(envelope.Error))
	}
	return redactSensitive(strings.TrimSpace(string(raw)))
}

func redactURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return errors.New(redactSensitive(urlErr.Err.Error()))
	}
	return err
}

func (c *ozClient) envelope(ctx context.Context, method, path string, body, out any) (*ozEnvelope, error) {
	raw, err := c.do(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	var envelope ozEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("OpenZeppelin Relayer %s %s: invalid response: %w", method, path, err)
	}
	if !envelope.Success {
		return nil, fmt.Errorf("OpenZeppelin Relayer %s %s: %s", method, path, ozErrorMessage(raw))
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return nil, fmt.Errorf("OpenZeppelin Relayer %s %s: invalid data: %w", method, path, err)
		}
	}
	return &envelope, nil
}

// Submit creates a transaction. OZ has no idempotency key: if this call fails
// without a definitive rejection the caller must reconcile with FindTransaction.
func (c *ozClient) Submit(ctx context.Context, req ozSubmitRequest) (*ozTransaction, error) {
	var tx ozTransaction
	if _, err := c.envelope(ctx, http.MethodPost, c.relayerPath("/transactions"), req, &tx); err != nil {
		return nil, err
	}
	if tx.ID == "" {
		return nil, errors.New("OpenZeppelin Relayer returned a transaction without id")
	}
	return &tx, nil
}

func (c *ozClient) GetTransaction(ctx context.Context, id string) (*ozTransaction, error) {
	return ozGet[ozTransaction](ctx, c, c.relayerPath("/transactions/"+url.PathEscape(id)))
}

// FindTransaction scans the relayer's transactions (newest first) for one with
// the given target and calldata, stopping at transactions created before
// notBefore. Withdraw calldata embeds the unique nullifier, so a match is the
// transaction an unconfirmed submit created.
func (c *ozClient) FindTransaction(ctx context.Context, to common.Address, data string, notBefore time.Time) (*ozTransaction, error) {
	const perPage = ozScanPageSize
	for page := 1; page <= ozMaxScanPages; page++ {
		var txs []ozTransaction
		path := c.relayerPath("/transactions") + "?page=" + strconv.Itoa(page) + "&per_page=" + strconv.Itoa(perPage)
		envelope, err := c.envelope(ctx, http.MethodGet, path, nil, &txs)
		if err != nil {
			return nil, err
		}
		reachedOlder := false
		for i := range txs {
			tx := &txs[i]
			created, err := time.Parse(time.RFC3339Nano, tx.CreatedAt)
			if err != nil {
				return nil, fmt.Errorf("%w: transaction %s has invalid created_at: %v", errOZScanIncomplete, tx.ID, err)
			}
			if created.Before(notBefore) {
				reachedOlder = true
				continue
			}
			if tx.targets(to, data) {
				return tx, nil
			}
		}
		if reachedOlder || len(txs) < perPage {
			return nil, nil
		}
		if envelope.Pagination != nil && uint64(page)*perPage >= envelope.Pagination.TotalItems {
			return nil, nil
		}
	}
	return nil, fmt.Errorf("%w: page limit exceeded", errOZScanIncomplete)
}

// errOZScanIncomplete: OZ answered, but FindTransaction could not finish the
// scan. The result is unknown (not negative) and OZ itself is available.
var errOZScanIncomplete = errors.New("OpenZeppelin Relayer transaction scan incomplete")

func (c *ozClient) GetRelayer(ctx context.Context) (*ozRelayer, error) {
	return ozGet[ozRelayer](ctx, c, c.relayerPath(""))
}

func (c *ozClient) GetStatus(ctx context.Context) (*ozRelayerStatus, error) {
	return ozGet[ozRelayerStatus](ctx, c, c.relayerPath("/status"))
}

// ozGet GETs path and decodes the envelope data into a new T.
func ozGet[T any](ctx context.Context, c *ozClient, path string) (*T, error) {
	var out T
	if _, err := c.envelope(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ChainID asks OZ's own provider for eth_chainId via the relayer RPC
// pass-through, proving OZ signs for the same chain as NET_ID.
func (c *ozClient) ChainID(ctx context.Context) (*big.Int, error) {
	raw, err := c.do(ctx, http.MethodPost, c.relayerPath("/rpc"), map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "eth_chainId", "params": []any{},
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("OpenZeppelin Relayer eth_chainId: invalid response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("OpenZeppelin Relayer eth_chainId: %s", redactSensitive(resp.Error.Message))
	}
	var hexValue string
	if err := json.Unmarshal(resp.Result, &hexValue); err != nil {
		return nil, fmt.Errorf("OpenZeppelin Relayer eth_chainId: unexpected result %s", string(resp.Result))
	}
	value, ok := new(big.Int).SetString(strings.TrimPrefix(strings.ToLower(hexValue), "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("OpenZeppelin Relayer eth_chainId: invalid value %q", hexValue)
	}
	return value, nil
}

// Ready checks OZ's own readiness probe (Redis, queue, workers).
func (c *ozClient) Ready(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/api/v1/ready", nil)
	return err
}
