package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

// units fills the derived Denomination/TokenUnit of a test InstanceMeta.
func units(meta InstanceMeta) InstanceMeta {
	meta, err := meta.withUnits()
	if err != nil {
		panic(err)
	}
	return meta
}

// pricesAt builds the oracle price cache with every price read at updatedAt.
func pricesAt(updatedAt time.Time, prices map[string]*big.Int) map[string]oraclePrice {
	out := make(map[string]oraclePrice, len(prices))
	for currency, price := range prices {
		out[currency] = oraclePrice{wei: price, updatedAt: updatedAt}
	}
	return out
}

// freshPrices builds an oracle price cache read just now.
func freshPrices(prices map[string]*big.Int) map[string]oraclePrice {
	return pricesAt(time.Now(), prices)
}

func requestWithNullifier(b string) WithdrawRequest {
	req := testWithdrawRequest()
	req.Args = append([]string(nil), req.Args...)
	req.Args[1] = "0x" + strings.Repeat(b, 32)
	return req
}

func submitRequest(t *testing.T, h *ozHarness, req WithdrawRequest) (*Job, []byte) {
	t.Helper()
	job, _, err := h.r.store.CreateOrGetPending(context.Background(), req, requestHash("1", req), h.r.nullifierKey(req))
	if err != nil {
		t.Fatal(err)
	}
	_, data, err := h.r.jobCall(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.r.updateJobCritical(job.ID, func(j *Job) { transitionJob(j, StatusPreparing) }); err != nil {
		t.Fatal(err)
	}
	h.r.submitToOZ(job.ID, &withdrawTxPlan{to: testPool, data: data, gasLimit: 400_000, tip: big.NewInt(1), maxFee: big.NewInt(3)})
	return h.job(t, job.ID), data
}

// recoveryChain is a JSON-RPC node that lets the full preflight of the test
// pool succeed. While gated, every request blocks until the gate opens or the
// caller gives up, simulating an RPC call in flight during shutdown.
type recoveryChain struct {
	poolABI  map[string][]byte // selector by method name
	mu       sync.Mutex
	gate     chan struct{} // nil = open
	requests atomic.Int64
}

func newRecoveryChain(t *testing.T, h *ozHarness) *recoveryChain {
	t.Helper()
	c := &recoveryChain{poolABI: map[string][]byte{}}
	for _, name := range []string{"denomination", "isKnownRoot", "isSpent", "token", "withdraw"} {
		c.poolABI[name] = h.r.poolABI.Methods[name].ID
	}
	server := httptest.NewServer(c)
	t.Cleanup(server.Close)
	client, err := dialRPC("HTTP_RPC_URL", server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	h.r.chain = singleRPC(client)
	return c
}

func (c *recoveryChain) close() {
	c.mu.Lock()
	c.gate = make(chan struct{})
	c.mu.Unlock()
}

func (c *recoveryChain) open() {
	c.mu.Lock()
	if c.gate != nil {
		close(c.gate)
		c.gate = nil
	}
	c.mu.Unlock()
}

type recoveryRPCCall struct {
	ID     json.RawMessage   `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func (c *recoveryChain) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	c.requests.Add(1)
	c.mu.Lock()
	gate := c.gate
	c.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-req.Context().Done():
			return
		}
	}
	body, _ := io.ReadAll(req.Body)
	w.Header().Set("Content-Type", "application/json")
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
		var calls []recoveryRPCCall
		_ = json.Unmarshal(body, &calls)
		out := make([]map[string]any, 0, len(calls))
		for _, call := range calls {
			out = append(out, c.answer(call))
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	var call recoveryRPCCall
	_ = json.Unmarshal(body, &call)
	_ = json.NewEncoder(w).Encode(c.answer(call))
}

func (c *recoveryChain) answer(call recoveryRPCCall) map[string]any {
	reply := map[string]any{"jsonrpc": "2.0", "id": call.ID}
	word := func(v *big.Int) string { return hexutil.Encode(common.LeftPadBytes(v.Bytes(), 32)) }
	switch call.Method {
	case "eth_chainId":
		reply["result"] = "0x1"
	case "eth_getCode":
		reply["result"] = "0x"
	case "eth_estimateGas":
		reply["result"] = "0x30d40" // 200000
	case "eth_maxPriorityFeePerGas":
		reply["result"] = "0x3b9aca00" // 1 gwei
	case "eth_getBlockByNumber":
		reply["result"] = &types.Header{Number: big.NewInt(100), Difficulty: new(big.Int), BaseFee: big.NewInt(1_000_000_000)}
	case "eth_call":
		var msg struct {
			Input hexutil.Bytes `json:"input"`
			Data  hexutil.Bytes `json:"data"`
		}
		_ = json.Unmarshal(call.Params[0], &msg)
		input := msg.Input
		if len(input) == 0 {
			input = msg.Data
		}
		switch {
		case len(input) >= 4 && bytes.Equal(input[:4], c.poolABI["denomination"]):
			reply["result"] = word(big.NewInt(100_000_000_000_000_000))
		case len(input) >= 4 && bytes.Equal(input[:4], c.poolABI["isKnownRoot"]):
			reply["result"] = word(big.NewInt(1))
		case len(input) >= 4 && bytes.Equal(input[:4], c.poolABI["isSpent"]):
			reply["result"] = word(new(big.Int))
		case len(input) >= 4 && bytes.Equal(input[:4], c.poolABI["withdraw"]):
			reply["result"] = "0x"
		default:
			reply["error"] = map[string]any{"code": 3, "message": "execution reverted"}
		}
	default:
		reply["error"] = map[string]any{"code": -32601, "message": "unsupported " + call.Method}
	}
	return reply
}

// preflightableRequest pays a fee that covers gas on recoveryChain.
func preflightableRequest(nullifierByte string) WithdrawRequest {
	req := requestWithNullifier(nullifierByte)
	req.Args[4] = hexutil.Encode(common.LeftPadBytes(big.NewInt(10_000_000_000_000_000).Bytes(), 32)) // 0.01 ETH
	return req
}

func configureWorkers(t *testing.T, h *ozHarness) {
	t.Helper()
	h.r.preflightSlots = make(chan struct{}, 2)
	h.r.cfg.JobTimeout = 10 * time.Second
	h.r.cfg.MaxGasLimit = 1_000_000
	h.r.cfg.GasLimitBufferBPS = 11_500
	if err := h.r.verifyOZRelayer(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.r.refreshOZStatus(context.Background())
}

func waitWorkers(t *testing.T, h *ozHarness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.r.Wait(ctx); err != nil {
		t.Fatalf("workers did not finish: %v", err)
	}
}

func (h *ozHarness) submitCount() int {
	h.oz.mu.Lock()
	defer h.oz.mu.Unlock()
	return len(h.oz.submitted)
}

// restart reopens the same SQLite file and drops all in-memory claims, as a
// process restart does.
func restartStore(t *testing.T, h *ozHarness, path string) {
	t.Helper()
	if err := h.r.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := NewSQLiteStore(path, "1", time.Hour, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h.r.store = store
	h.r.claimed.Clear()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func sampleWithdrawRequest() WithdrawRequest {
	return WithdrawRequest{
		Proof:    "0x" + strings.Repeat("11", 256),
		Contract: "0x0000000000000000000000000000000000000011",
		Args: []string{
			"0x" + strings.Repeat("22", 32),
			"0x" + strings.Repeat("33", 32),
			"0x0000000000000000000000000000000000000044",
			"0x0000000000000000000000000000000000000055",
			"0x" + strings.Repeat("00", 32),
			"0x" + strings.Repeat("00", 32),
		},
	}
}
