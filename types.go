package main

import (
	"bytes"
	"encoding/json"
	"math/big"
	"net"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

type Config struct {
	Host                    string
	Port                    string
	RPCURLs                 []string // HTTP_RPC_URL: 1-2 read-only RPCs (round robin + failover)
	SQLitePath              string
	OracleRPCURL            string
	OZRelayerURL            string
	OZRelayerID             string
	OZAPIKey                string
	OZTxValidity            time.Duration
	ChainID                 *big.Int
	RewardAccount           common.Address
	OffchainOracle          common.Address
	Instances               InstancesConfig
	InstanceByAddress       map[common.Address]InstanceMeta
	OraclePriceTokens       map[string]oraclePriceToken
	MaxGasLimit             uint64
	GasLimitBufferBPS       uint64
	GasFeeToleranceBPS      uint64
	MaxGasPriceWei          *big.Int
	ServiceFeeBPS           uint64
	PollPeriod              time.Duration
	JobTimeout              time.Duration
	MaxActiveJobs           uint64
	MaxProtectedJobs        uint64
	MaxPreflightConcurrency uint64
	IdempotencyTTL          time.Duration
	PriceUpdatePeriod       time.Duration
	CORSOrigins             []string
	TrustedProxyCIDRs       []*net.IPNet
	ClientIPHeader          string
	ClientIPMode            string
	HealthToken             string
	RelayRatePerMinute      uint64
	RelayRateBurst          uint64
	RelayRateMaxClients     uint64
	Version                 string
}

type ChainInstances map[string]InstancesConfig

type InstancesConfig map[string]InstanceConfig

type InstanceConfig struct {
	InstanceAddress    map[string]string `json:"instanceAddress"`
	TokenAddress       string            `json:"tokenAddress,omitempty"`
	OracleTokenAddress string            `json:"oracleTokenAddress,omitempty"`
	Symbol             string            `json:"symbol,omitempty"`
	Decimals           uint8             `json:"decimals,omitempty"`
}

// tokenOrder is the preferred token order in /status, price refreshes and
// logs; other tokens follow sorted.
var tokenOrder = []string{"eth", "dai", "cdai", "usdc", "usdt", "torn", "wbtc"}

// MarshalJSON writes the tokens in tokenOrder, then the rest sorted.
func (instances InstancesConfig) MarshalJSON() ([]byte, error) {
	return marshalOrdered(instances, tokenOrder, marshalInstanceConfig)
}

func marshalInstanceConfig(token string, cfg InstanceConfig) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"instanceAddress":`)
	addresses, err := marshalOrderedStringMap(cfg.InstanceAddress, denominationOrder(token))
	if err != nil {
		return nil, err
	}
	buf.Write(addresses)
	if cfg.TokenAddress != "" {
		tokenAddress, _ := json.Marshal(cfg.TokenAddress)
		buf.WriteString(`,"tokenAddress":`)
		buf.Write(tokenAddress)
	}
	if cfg.Symbol != "" {
		symbol, _ := json.Marshal(cfg.Symbol)
		buf.WriteString(`,"symbol":`)
		buf.Write(symbol)
	}
	if cfg.Decimals != 0 {
		decimals, _ := json.Marshal(cfg.Decimals)
		buf.WriteString(`,"decimals":`)
		buf.Write(decimals)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

type InstanceMeta struct {
	Currency     string
	Amount       string
	Symbol       string
	Decimals     uint8
	IsERC20      bool
	TokenAddress common.Address
	// Derived by withUnits from Amount and Decimals at config load, so the
	// request path never re-parses configuration.
	Denomination *big.Int // Amount in base units
	TokenUnit    *big.Int // 10^Decimals
}

// withUnits fills Denomination and TokenUnit from Amount and Decimals.
func (m InstanceMeta) withUnits() (InstanceMeta, error) {
	denomination, err := decimalToUnits(m.Amount, m.Decimals)
	if err != nil {
		return InstanceMeta{}, err
	}
	m.Denomination = denomination
	m.TokenUnit = tokenUnit(m.Decimals)
	return m, nil
}

// WithdrawRequest is the client payload of POST /v1/tornadoWithdraw.
type WithdrawRequest struct {
	Proof    string   `json:"proof"`
	Contract string   `json:"contract"`
	Args     []string `json:"args"`
	MethodID string   `json:"methodId,omitempty"`
}

// Positions in WithdrawRequest.Args, matching the Tornado pool signature
// withdraw(bytes proof, bytes32 root, bytes32 nullifierHash, address recipient,
// address relayer, uint256 fee, uint256 refund).
const (
	argRoot = iota
	argNullifierHash
	argRecipient
	argRelayer
	argFee
	argRefund
	withdrawArgCount
)

type JobStatus string

const (
	StatusQueued     JobStatus = "QUEUED"
	StatusPreparing  JobStatus = "PREPARING"
	StatusSubmitting JobStatus = "SUBMITTING" // OZ submit in flight or outcome unknown
	StatusSubmitted  JobStatus = "SUBMITTED"  // OZ owns the transaction
	StatusMined      JobStatus = "MINED"
	StatusConfirmed  JobStatus = "CONFIRMED"
	StatusFailed     JobStatus = "FAILED"
	StatusSent       JobStatus = "SENT" // Public API compatibility only.
)

type Job struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Status       JobStatus       `json:"status"`
	Request      WithdrawRequest `json:"request"`
	RequestHash  string          `json:"requestHash,omitempty"`
	OZTxID       string          `json:"ozTxId,omitempty"`
	TxHash       string          `json:"txHash,omitempty"`
	TxHashes     []string        `json:"txHashes,omitempty"`
	FailedReason string          `json:"failedReason,omitempty"`
	UpdatedAt    time.Time       `json:"updatedAt"`
	// CreatedAt is the admission time; it bounds preflight retries.
	CreatedAt time.Time `json:"-"`
	// SubmitStartedAt is set before the OZ submit call and bounds transaction
	// list searches used to reconcile an unknown POST outcome.
	SubmitStartedAt time.Time `json:"-"`
	NullifierKey    string    `json:"-"`
	// Retryable is set when a FAILED job may be retried with the identical request.
	Retryable bool `json:"-"`
}

type StoreStats struct {
	CurrentQueue  int `json:"currentQueue"`
	ProtectedJobs int `json:"protectedJobs"`
	TotalJobs     int `json:"totalJobs"`
	// UnknownSubmits are SUBMITTING jobs without an OZ transaction id for
	// longer than unknownSubmitWarnAfter: they hold their nullifier until an
	// operator resolves them (README "Unknown submit outcome"). At most 100.
	UnknownSubmits []unknownSubmit `json:"unknownSubmits"`
}

type unknownSubmit struct {
	ID          string    `json:"id"`
	SubmittedAt time.Time `json:"submittedAt"`
}

func isTerminalStatus(status JobStatus) bool {
	return status == StatusConfirmed || status == StatusFailed
}

// transitionJob only moves forward. MINED may fall back to SUBMITTED when OZ
// reports the mined transaction was reorganized out. SUBMITTED->SUBMITTED and
// MINED->MINED self-transitions are allowed (and report true) so callers can
// persist updated fields such as a replacement tx hash without a status
// change; QUEUED, PREPARING and SUBMITTING do not transition to themselves.
func transitionJob(job *Job, next JobStatus) bool {
	if job == nil || isTerminalStatus(job.Status) {
		return false
	}
	allowed := false
	switch job.Status {
	case StatusQueued:
		allowed = next == StatusPreparing || next == StatusFailed
	case StatusPreparing:
		// Back to QUEUED only when preflight is deferred (nothing was sent).
		allowed = next == StatusSubmitting || next == StatusQueued || next == StatusFailed
	case StatusSubmitting, StatusSubmitted, StatusMined:
		allowed = next == StatusSubmitted || next == StatusMined || next == StatusConfirmed || next == StatusFailed
	}
	if !allowed {
		return false
	}
	job.Status = next
	return true
}

// activeStatuses count against MAX_ACTIVE_JOBS and are resumed on restart.
func activeStatuses() []JobStatus {
	return []JobStatus{StatusQueued, StatusPreparing, StatusSubmitting, StatusSubmitted, StatusMined}
}

// validTxHash reports a well-formed, non-zero transaction hash.
func validTxHash(txHash string) bool {
	return common.IsHexHash(txHash) && common.HexToHash(txHash) != (common.Hash{})
}

// recordTxHash makes hash current and keeps every hash OZ ever reported, so
// replaced attempts remain auditable after OZ prunes its own history.
// Malformed or zero hashes are ignored.
func recordTxHash(job *Job, txHash string) {
	if !validTxHash(txHash) || job.TxHash == txHash {
		return
	}
	job.TxHash = txHash
	if !slices.Contains(job.TxHashes, txHash) {
		job.TxHashes = append(job.TxHashes, txHash)
	}
}

func marshalOrderedStringMap(values map[string]string, preferred []string) ([]byte, error) {
	return marshalOrdered(values, preferred, func(_ string, value string) ([]byte, error) {
		return json.Marshal(value)
	})
}

// marshalOrdered writes values as a JSON object whose keys follow preferred,
// then the remaining keys sorted (orderedKeys). encode renders one value. A
// nil map is written as null, like encoding/json.
func marshalOrdered[V any](values map[string]V, preferred []string, encode func(key string, value V) ([]byte, error)) ([]byte, error) {
	if values == nil {
		return []byte("null"), nil
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, key := range orderedKeys(values, preferred) {
		if i > 0 {
			buf.WriteByte(',')
		}
		encodedKey, _ := json.Marshal(key)
		encodedValue, err := encode(key, values[key])
		if err != nil {
			return nil, err
		}
		buf.Write(encodedKey)
		buf.WriteByte(':')
		buf.Write(encodedValue)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func orderedKeys[V any](values map[string]V, preferred []string) []string {
	seen := make(map[string]bool, len(values))
	keys := make([]string, 0, len(values))
	for _, key := range preferred {
		if _, ok := values[key]; ok {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	extras := make([]string, 0)
	for key := range values {
		if !seen[key] {
			extras = append(extras, key)
		}
	}
	slices.Sort(extras)
	return append(keys, extras...)
}

func denominationOrder(token string) []string {
	switch token {
	case "eth":
		return []string{"1", "10", "100", "0.1"}
	case "dai":
		return []string{"100", "1000", "10000", "100000"}
	case "cdai":
		return []string{"5000", "50000", "500000", "5000000"}
	case "usdc":
		return []string{"100", "1000", "10000"}
	case "usdt":
		return []string{"100", "1000", "10000", "100000"}
	case "wbtc":
		return []string{"1", "10", "0.1"}
	default:
		return nil
	}
}
