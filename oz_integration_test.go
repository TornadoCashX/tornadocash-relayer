package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

var (
	testPool          = common.HexToAddress("0x12D66f87A04A9E220743712cE6d9bB1B5616B8Fc")
	testRewardAccount = common.HexToAddress("0x1111111111111111111111111111111111111111")
	testOZSender      = common.HexToAddress("0x2222222222222222222222222222222222222222")
	testChainID       = big.NewInt(1)
)

// fakeChain is a minimal JSON-RPC node: receipts and transactions by hash plus
// the pool's isSpent answer.
type fakeChain struct {
	mu       sync.Mutex
	spent    bool
	txs      map[common.Hash]*types.Transaction
	receipts map[common.Hash]uint64 // hash -> receipt status
	pending  map[common.Hash]bool   // in mempool, no receipt yet
	key      *ecdsa.PrivateKey
	// finalizedNonce answers eth_getTransactionCount at the "finalized" tag;
	// any other tag is rejected so tests prove finality is requested.
	finalizedNonce uint64
	// finalizedBlock answers eth_getBlockByNumber("finalized"); mined
	// receipts live in block 16, so they are final once this is >= 16.
	finalizedBlock uint64
	// spendNotFinal makes isSpent read at the "finalized" tag return false
	// while latest reports spent (a spend that a reorg may still undo).
	spendNotFinal bool
	// rootUnknown makes isKnownRoot return false (root left the history).
	rootUnknown bool
}

// isKnownRootSelector tells isKnownRoot apart from isSpent in eth_call.
var isKnownRootSelector = func() []byte {
	parsed, err := abi.JSON(strings.NewReader(tornadoPoolABI))
	if err != nil {
		panic(err)
	}
	return parsed.Methods["isKnownRoot"].ID
}()

// Headers and receipts share the same block hash; finality tests must not
// accidentally accept an unrelated hash just because its height matches.
func fakeCanonicalHeader(number uint64) *types.Header {
	return &types.Header{Number: new(big.Int).SetUint64(number), Difficulty: new(big.Int)}
}

func newFakeChain(t *testing.T) *fakeChain {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return &fakeChain{txs: map[common.Hash]*types.Transaction{}, receipts: map[common.Hash]uint64{}, pending: map[common.Hash]bool{}, key: key}
}

// mine records a signed transaction to `to` with `data` and receipt status.
func (c *fakeChain) mine(t *testing.T, nonce uint64, to common.Address, data []byte, status uint64) common.Hash {
	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID: testChainID, Nonce: nonce, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(2),
		Gas: 500_000, To: &to, Value: new(big.Int), Data: data,
	}), types.LatestSignerForChainID(testChainID), c.key)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.txs[tx.Hash()] = tx
	c.receipts[tx.Hash()] = status
	if status == types.ReceiptStatusSuccessful {
		c.spent = true
	}
	return tx.Hash()
}

// broadcast records a signed transaction that sits in the mempool.
func (c *fakeChain) broadcast(t *testing.T, nonce uint64, to common.Address, data []byte) common.Hash {
	hash := c.mine(t, nonce, to, data, types.ReceiptStatusFailed)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.receipts, hash)
	c.pending[hash] = true
	return hash
}

// finalTag reports a read of the finalized view: the tag or its block number.
func (c *fakeChain) finalTag(tag string) bool {
	if tag == "finalized" {
		return true
	}
	number, err := hexutil.DecodeUint64(tag)
	return err == nil && number == c.finalizedBlock
}

func (c *fakeChain) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var call struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	body, _ := io.ReadAll(req.Body)
	if err := json.Unmarshal(body, &call); err != nil {
		http.Error(w, "batch not supported", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var result any
	hashParam := func() common.Hash {
		var hash common.Hash
		_ = json.Unmarshal(call.Params[0], &hash)
		return hash
	}
	switch call.Method {
	case "eth_chainId":
		result = hexutil.EncodeBig(testChainID)
	case "eth_getTransactionCount":
		var tag string
		if len(call.Params) < 2 || json.Unmarshal(call.Params[1], &tag) != nil || !c.finalTag(tag) {
			http.Error(w, "nonce must be read at the finalized block", http.StatusBadRequest)
			return
		}
		result = hexutil.EncodeUint64(c.finalizedNonce)
	case "eth_call":
		var tag string
		if len(call.Params) > 1 {
			_ = json.Unmarshal(call.Params[1], &tag)
		}
		var msg struct {
			Input hexutil.Bytes `json:"input"`
			Data  hexutil.Bytes `json:"data"`
		}
		_ = json.Unmarshal(call.Params[0], &msg)
		input := append(msg.Input, msg.Data...)
		word := make([]byte, 32)
		if len(input) >= 4 && bytes.Equal(input[:4], isKnownRootSelector) {
			if !c.rootUnknown {
				word[31] = 1
			}
		} else if c.spent && !(c.finalTag(tag) && c.spendNotFinal) {
			word[31] = 1
		}
		result = hexutil.Encode(word)
	case "eth_getBlockByNumber":
		var tag string
		if json.Unmarshal(call.Params[0], &tag) != nil {
			http.Error(w, "invalid block tag", http.StatusBadRequest)
			return
		}
		number := c.finalizedBlock
		if tag != "finalized" {
			var err error
			number, err = hexutil.DecodeUint64(tag)
			if err != nil {
				http.Error(w, "invalid block number", http.StatusBadRequest)
				return
			}
		}
		result = fakeCanonicalHeader(number)
	case "eth_getTransactionByHash":
		tx, ok := c.txs[hashParam()]
		if !ok {
			result = nil
			break
		}
		encoded, _ := tx.MarshalJSON()
		var fields map[string]any
		_ = json.Unmarshal(encoded, &fields)
		if c.pending[tx.Hash()] {
			fields["blockHash"], fields["blockNumber"], fields["transactionIndex"] = nil, nil, nil
		} else {
			fields["blockHash"] = fakeCanonicalHeader(16).Hash().Hex()
			fields["blockNumber"] = "0x10"
			fields["transactionIndex"] = "0x0"
		}
		fields["from"] = crypto.PubkeyToAddress(c.key.PublicKey).Hex()
		result = fields
	case "eth_getTransactionReceipt":
		hash := hashParam()
		status, ok := c.receipts[hash]
		if !ok {
			result = nil
			break
		}
		receipt := &types.Receipt{
			Type: types.DynamicFeeTxType, Status: status, CumulativeGasUsed: 21000, GasUsed: 21000,
			TxHash: hash, BlockHash: fakeCanonicalHeader(16).Hash(), BlockNumber: big.NewInt(16), Logs: []*types.Log{},
		}
		result = receipt
	default:
		http.Error(w, "unsupported "+call.Method, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
}

// fakeOZ is a mock OpenZeppelin Relayer API.
type fakeOZ struct {
	mu          sync.Mutex
	txs         map[string]*ozTransaction
	submitted   []ozSubmitRequest
	submitCode  int // 0 = success
	submitError string
	policy      ozEVMPolicy
	paused      bool
	down        bool   // every API call answers 503
	relayerID   string // served relayer id; "" means "oz-1"
	txGets      int    // GET .../transactions/{id} calls
	failingTx   string // this transaction id answers 500
}

func (f *fakeOZ) failTx(id string) {
	f.mu.Lock()
	f.failingTx = id
	f.mu.Unlock()
}

func (f *fakeOZ) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Header.Get("Authorization") != "Bearer "+strings.Repeat("k", 32) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"error":"Unauthorized"}`))
		return
	}
	write := func(status int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": status < 300, "data": data})
	}
	if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/transactions/") {
		f.txGets++
	}
	if f.down {
		write(http.StatusServiceUnavailable, nil)
		return
	}
	id := f.relayerID
	if id == "" {
		id = "oz-1"
	}
	base := "/api/v1/relayers/" + id
	path := req.URL.Path
	switch {
	case req.Method == http.MethodGet && path == "/api/v1/ready":
		write(200, map[string]any{"ready": true})
	case req.Method == http.MethodGet && path == base:
		write(200, map[string]any{"id": "oz-1", "network": "mainnet", "network_type": "evm",
			"address": testOZSender.Hex(), "paused": f.paused, "system_disabled": false, "policies": f.policy})
	case req.Method == http.MethodGet && path == base+"/status":
		write(200, map[string]any{"network_type": "evm", "balance": "1000", "pending_transactions_count": 0,
			"system_disabled": false, "paused": f.paused, "nonce": "7"})
	case req.Method == http.MethodPost && path == base+"/rpc":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	case req.Method == http.MethodPost && path == base+"/transactions":
		var request ozSubmitRequest
		raw, _ := io.ReadAll(req.Body)
		if err := json.Unmarshal(raw, &request); err != nil {
			write(400, nil)
			return
		}
		f.submitted = append(f.submitted, request)
		if f.submitCode != 0 {
			w.WriteHeader(f.submitCode)
			_, _ = w.Write([]byte(`{"success":false,"error":"` + f.submitError + `"}`))
			return
		}
		tx := &ozTransaction{ID: "tx-" + string(rune('a'+len(f.txs))), Status: ozStatusPending, To: request.To, Data: request.Data,
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		f.txs[tx.ID] = tx
		write(200, tx)
	case req.Method == http.MethodGet && path == base+"/transactions":
		out := make([]*ozTransaction, 0, len(f.txs))
		for _, tx := range f.txs {
			out = append(out, tx)
		}
		write(200, out)
	case req.Method == http.MethodGet && f.failingTx != "" && path == base+"/transactions/"+f.failingTx:
		write(http.StatusInternalServerError, nil)
	case req.Method == http.MethodGet && strings.HasPrefix(path, base+"/transactions/"):
		tx, ok := f.txs[strings.TrimPrefix(path, base+"/transactions/")]
		if !ok {
			write(404, nil)
			return
		}
		write(200, tx)
	default:
		write(404, nil)
	}
}

func (f *fakeOZ) set(id string, fn func(*ozTransaction)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.txs[id])
}

type ozHarness struct {
	r     *Relayer
	chain *fakeChain
	oz    *fakeOZ
}

func newOZHarness(t *testing.T) *ozHarness {
	t.Helper()
	chain := newFakeChain(t)
	chainServer := httptest.NewServer(chain)
	t.Cleanup(chainServer.Close)
	eip1559, estimateGas := true, false
	oz := &fakeOZ{txs: map[string]*ozTransaction{}, policy: ozEVMPolicy{
		GasPriceCap: "100000000000", WhitelistReceivers: []string{testPool.Hex()},
		EIP1559Pricing: &eip1559, GasLimitEstimation: &estimateGas,
	}}
	ozServer := httptest.NewServer(oz)
	t.Cleanup(ozServer.Close)

	client, err := dialRPC("HTTP_RPC_URL", chainServer.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	poolABI, err := abi.JSON(strings.NewReader(tornadoPoolABI))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ChainID:           testChainID,
		RewardAccount:     testRewardAccount,
		InstanceByAddress: map[common.Address]InstanceMeta{testPool: units(InstanceMeta{Currency: "eth", Amount: "0.1", Decimals: 18})},
		MaxGasPriceWei:    big.NewInt(100_000_000_000),
		OZRelayerURL:      ozServer.URL,
		OZRelayerID:       "oz-1",
		OZAPIKey:          strings.Repeat("k", 32),
		OZTxValidity:      time.Hour,
		PollPeriod:        time.Second,
	}
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "relayer.db"), "1", time.Hour, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &Relayer{cfg: cfg, chain: singleRPC(client), oz: newOZClient(cfg), poolABI: poolABI, store: store,
		ctx: ctx, cancel: cancel}
	return &ozHarness{r: r, chain: chain, oz: oz}
}

func testWithdrawRequest() WithdrawRequest {
	return WithdrawRequest{
		Proof:    "0x" + strings.Repeat("ab", 256),
		Contract: testPool.Hex(),
		Args: []string{
			"0x" + strings.Repeat("01", 32),
			"0x" + strings.Repeat("02", 32),
			"0x3333333333333333333333333333333333333333",
			testRewardAccount.Hex(),
			"0x" + strings.Repeat("00", 31) + "05",
			"0x" + strings.Repeat("00", 32),
		},
	}
}

// createSubmittedJob creates a job and submits a valid plan to the fake OZ.
func (h *ozHarness) createSubmittedJob(t *testing.T) (*Job, []byte) {
	t.Helper()
	req := testWithdrawRequest()
	job, _, err := h.r.store.CreateOrGetPending(context.Background(), req, requestHash("1", req), h.r.nullifierKey(req))
	if err != nil {
		t.Fatal(err)
	}
	_, data, err := h.r.jobCall(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.r.updateJobCritical(job.ID, func(job *Job) { transitionJob(job, StatusPreparing) }); err != nil {
		t.Fatal(err)
	}
	h.r.submitToOZ(job.ID, &withdrawTxPlan{to: testPool, data: data, gasLimit: 400_000, tip: big.NewInt(1), maxFee: big.NewInt(3)})
	return h.job(t, job.ID), data
}

func (h *ozHarness) job(t *testing.T, id string) *Job {
	t.Helper()
	job, ok, err := h.r.store.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("load job %s: %v", id, err)
	}
	return job
}

func (h *ozHarness) reconcile(t *testing.T, id string) *Job {
	t.Helper()
	h.r.reconcileJob(context.Background(), id)
	return h.job(t, id)
}

func nullifierHeld(t *testing.T, h *ozHarness) bool {
	t.Helper()
	stats, err := h.r.store.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return stats.ProtectedJobs > 0
}

func TestSubmitSendsExactPlanAndStoresOZID(t *testing.T) {
	h := newOZHarness(t)
	job, data := h.createSubmittedJob(t)
	if job.Status != StatusSubmitted || job.OZTxID == "" {
		t.Fatalf("job = %s id=%q, want SUBMITTED with OZ id", job.Status, job.OZTxID)
	}
	sent := h.oz.submitted[0]
	if sent.To != testPool.Hex() || sent.Data != hexutil.Encode(data) || sent.GasLimit != 400_000 ||
		sent.MaxFeePerGas != "3" || sent.MaxPriorityFeePerGas != "1" || sent.Value != "0" || sent.ValidUntil == "" {
		t.Fatalf("unexpected OZ request %+v", sent)
	}
}

func TestSubmitWireFormatUsesJSONNumbers(t *testing.T) {
	encoded, err := json.Marshal(ozSubmitRequest{To: "0x1", Value: "0", Data: "0x", GasLimit: 1, MaxFeePerGas: "300000000000000000000", MaxPriorityFeePerGas: "2", ValidUntil: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"max_fee_per_gas":300000000000000000000`) || !strings.Contains(string(encoded), `"value":0`) {
		t.Fatalf("fee fields must be JSON numbers: %s", encoded)
	}
}

func TestBumpedHashIsTrackedAndConfirmedAfterChainCheck(t *testing.T) {
	h := newOZHarness(t)
	job, data := h.createSubmittedJob(t)

	h.oz.set(job.OZTxID, func(tx *ozTransaction) { tx.Status, tx.Hash = ozStatusSubmitted, common.HexToHash("0xaa").Hex() })
	if job = h.reconcile(t, job.ID); job.TxHash != common.HexToHash("0xaa").Hex() {
		t.Fatalf("first hash not stored: %+v", job)
	}
	// OZ bumped the fee: new hash, then confirmed.
	minedHash := h.chain.mine(t, 7, testPool, data, types.ReceiptStatusSuccessful)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) { tx.Status, tx.Hash = ozStatusConfirmed, minedHash.Hex() })
	job = h.reconcile(t, job.ID)
	if job.Status != StatusConfirmed || job.TxHash != minedHash.Hex() || len(job.TxHashes) != 2 {
		t.Fatalf("job = %+v, want CONFIRMED with latest hash and both hashes", job)
	}
	if nullifierHeld(t, h) {
		t.Fatal("nullifier must be released after confirmation")
	}
}

func TestOZConfirmedWithoutLocalReceiptStaysMined(t *testing.T) {
	h := newOZHarness(t)
	job, _ := h.createSubmittedJob(t)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) { tx.Status, tx.Hash = ozStatusConfirmed, common.HexToHash("0xcc").Hex() })
	job = h.reconcile(t, job.ID)
	if job.Status == StatusConfirmed || isTerminalStatus(job.Status) {
		t.Fatalf("job = %s; OZ confirmation must be verified on HTTP_RPC_URL first", job.Status)
	}
	if !nullifierHeld(t, h) {
		t.Fatal("nullifier must stay owned while unverified")
	}
}

func TestOZFailedWithMinedRevertFailsPermanently(t *testing.T) {
	h := newOZHarness(t)
	job, data := h.createSubmittedJob(t)
	reverted := h.chain.mine(t, 7, testPool, data, types.ReceiptStatusFailed)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) {
		tx.Status, tx.Hash, tx.StatusReason = ozStatusFailed, reverted.Hex(), "reverted"
	})
	// Mined but not finalized: a reorg could still drop the revert and let the
	// withdrawal execute, so the nullifier stays held.
	if job = h.reconcile(t, job.ID); isTerminalStatus(job.Status) || !nullifierHeld(t, h) {
		t.Fatalf("job = %+v; an unfinalized revert must not release", job)
	}
	h.chain.setFinalizedBlock(16)
	job = h.reconcile(t, job.ID)
	if job.Status != StatusFailed || job.Retryable {
		t.Fatalf("job = %+v, want permanent FAILED", job)
	}
	if nullifierHeld(t, h) {
		t.Fatal("the job lock must be released (new requests are refused via reverted_nullifiers)")
	}
}

func TestExpiryNoopWithSpentNullifierIsPermanent(t *testing.T) {
	h := newOZHarness(t)
	job, _ := h.createSubmittedJob(t)
	// Someone else withdrew; OZ replaced ours with a NOOP to itself.
	h.chain.mine(t, 1, testPool, []byte{1}, types.ReceiptStatusSuccessful)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) {
		tx.Status, tx.Hash, tx.To, tx.Data = ozStatusConfirmed, common.HexToHash("0xdd").Hex(), testOZSender.Hex(), "0x"
	})
	job = h.reconcile(t, job.ID)
	if job.Status != StatusFailed || job.Retryable || !strings.Contains(job.FailedReason, "already spent") {
		t.Fatalf("job = %+v, want permanent spent failure", job)
	}
}

func TestPrunedOZRecordResolvedByEarlierMinedHash(t *testing.T) {
	h := newOZHarness(t)
	job, data := h.createSubmittedJob(t)
	minedHash := h.chain.mine(t, 7, testPool, data, types.ReceiptStatusSuccessful)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) { tx.Status, tx.Hash = ozStatusMined, minedHash.Hex() })
	if job = h.reconcile(t, job.ID); job.Status != StatusMined {
		t.Fatalf("job = %s, want MINED", job.Status)
	}
	h.oz.mu.Lock()
	delete(h.oz.txs, job.OZTxID)
	h.oz.mu.Unlock()
	// Without OZ, only a finalized receipt may make the job terminal.
	if job = h.reconcile(t, job.ID); job.Status != StatusMined || !nullifierHeld(t, h) {
		t.Fatalf("job = %+v; an unfinalized receipt must keep the job MINED", job)
	}
	h.chain.setFinalizedBlock(16)
	if job = h.reconcile(t, job.ID); job.Status != StatusConfirmed {
		t.Fatalf("job = %+v, want CONFIRMED from chain", job)
	}
}

func TestSubmitDefinitiveRejectionFailsRetryable(t *testing.T) {
	h := newOZHarness(t)
	h.oz.submitCode, h.oz.submitError = http.StatusBadRequest, "gas_limit is too low"
	job, _ := h.createSubmittedJob(t)
	if job.Status != StatusFailed || !job.Retryable || nullifierHeld(t, h) {
		t.Fatalf("job = %+v, want retryable FAILED and released nullifier", job)
	}
}

func TestUnknownSubmitOutcomeIsReconciledByCalldata(t *testing.T) {
	h := newOZHarness(t)
	h.oz.submitCode = http.StatusBadGateway
	job, _ := h.createSubmittedJob(t)
	if job.Status != StatusSubmitting || !nullifierHeld(t, h) {
		t.Fatalf("job = %s, want SUBMITTING with nullifier held", job.Status)
	}
	// The POST actually created the transaction on OZ.
	h.oz.mu.Lock()
	sent := h.oz.submitted[0]
	h.oz.txs["tx-late"] = &ozTransaction{ID: "tx-late", Status: ozStatusSubmitted, To: strings.ToLower(sent.To), Data: sent.Data,
		Hash: common.HexToHash("0xee").Hex(), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	h.oz.mu.Unlock()
	job = h.reconcile(t, job.ID)
	if job.OZTxID != "tx-late" || job.Status != StatusSubmitted || job.TxHash != common.HexToHash("0xee").Hex() {
		t.Fatalf("job = %+v, want reconciled OZ tx", job)
	}
}

func TestUnknownSubmitWithoutOZTransactionRetainsNullifierAfterGrace(t *testing.T) {
	h := newOZHarness(t)
	h.oz.submitCode = http.StatusBadGateway
	job, _ := h.createSubmittedJob(t)
	if job = h.reconcile(t, job.ID); job.Status != StatusSubmitting {
		t.Fatalf("unknown submission must stay SUBMITTING, got %s", job.Status)
	}
	if err := h.r.updateJobCritical(job.ID, func(job *Job) { job.SubmitStartedAt = time.Now().Add(-2 * time.Minute) }); err != nil {
		t.Fatal(err)
	}
	if job = h.reconcile(t, job.ID); job.Status != StatusSubmitting {
		t.Fatalf("job = %+v, want SUBMITTING quarantine", job)
	}
	if !nullifierHeld(t, h) {
		t.Fatal("unknown submission must retain its nullifier")
	}
}

func TestVerifyOZRelayerChecksPolicyAndRecordsSender(t *testing.T) {
	h := newOZHarness(t)
	if err := h.r.verifyOZRelayer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.r.senderAddress() != testOZSender {
		t.Fatalf("sender = %s", h.r.senderAddress().Hex())
	}
	h.oz.policy.WhitelistReceivers = []string{"0x3333333333333333333333333333333333333333"}
	if err := h.r.verifyOZRelayer(context.Background()); err == nil || !strings.Contains(err.Error(), "missing pool") {
		t.Fatalf("missing whitelist must fail, got %v", err)
	}
	h.oz.policy.WhitelistReceivers = []string{testPool.Hex(), "0x3333333333333333333333333333333333333333"}
	if err := h.r.verifyOZRelayer(context.Background()); err == nil || !strings.Contains(err.Error(), "exactly match") {
		t.Fatalf("extra whitelist receiver must fail, got %v", err)
	}
	h.oz.policy.WhitelistReceivers = []string{testPool.Hex()}
	h.oz.policy.EIP1559Pricing = nil
	if err := h.r.verifyOZRelayer(context.Background()); err == nil || !strings.Contains(err.Error(), "eip1559_pricing") {
		t.Fatalf("missing eip1559 policy must fail, got %v", err)
	}
	eip1559 := true
	h.oz.policy.EIP1559Pricing = &eip1559
	h.oz.policy.GasPriceCap = "100000000001"
	if err := h.r.verifyOZRelayer(context.Background()); err == nil || !strings.Contains(err.Error(), "gas_price_cap") {
		t.Fatalf("gas cap above maxGasPriceGwei must fail, got %v", err)
	}
}

func TestOZClientMapsNotFoundAndAuth(t *testing.T) {
	h := newOZHarness(t)
	if _, err := h.r.oz.GetTransaction(context.Background(), "missing"); err != errOZNotFound {
		t.Fatalf("err = %v, want errOZNotFound", err)
	}
	bad := *h.r.oz
	bad.apiKey = "wrong"
	if _, err := bad.GetRelayer(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want HTTP 401", err)
	}
}

func TestStoreRefusesPreviousSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	store, err := NewSQLiteStore(path, "1", time.Hour, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	if _, err := NewSQLiteStore(path, "1", time.Hour, 1, 1); err == nil || !strings.Contains(err.Error(), "previous self-signing relayer") {
		t.Fatalf("err = %v, want old-schema refusal", err)
	}
}

func TestDuplicateNullifierReturnsOwningJob(t *testing.T) {
	h := newOZHarness(t)
	req := testWithdrawRequest()
	first, _, err := h.r.store.CreateOrGetPending(context.Background(), req, requestHash("1", req), h.r.nullifierKey(req))
	if err != nil {
		t.Fatal(err)
	}
	other := req
	other.Args = append([]string(nil), req.Args...)
	other.Args[2] = "0x4444444444444444444444444444444444444444"
	second, duplicate, err := h.r.store.CreateOrGetPending(context.Background(), other, requestHash("1", other), h.r.nullifierKey(other))
	if err != nil || !duplicate || second.ID != first.ID {
		t.Fatalf("same nullifier must return owning job, got %v dup=%v err=%v", second, duplicate, err)
	}
}

func TestConfigRejectsLocalSigningSettings(t *testing.T) {
	t.Setenv("PRIVATE_KEY", "0x01")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "relayer.config.json")
	original, err := os.ReadFile("relayer.config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NET_ID", "1")
	t.Setenv("REWARD_ACCOUNT", testRewardAccount.Hex())
	if _, err := LoadConfig(cfgPath); err == nil || !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("err = %v, want PRIVATE_KEY rejection", err)
	}
}

func TestSubmitToUnknownRelayerIsDefinitive(t *testing.T) {
	h := newOZHarness(t)
	h.oz.relayerID = "other"
	job, _ := h.createSubmittedJob(t)
	if job.Status != StatusFailed || !job.Retryable || nullifierHeld(t, h) {
		t.Fatalf("job = %+v, want retryable FAILED (relayer 404 creates nothing)", job)
	}
}

func TestOZFailedWhileStillInMempoolKeepsNullifier(t *testing.T) {
	h := newOZHarness(t)
	job, data := h.createSubmittedJob(t)
	pendingHash := h.chain.broadcast(t, 7, testPool, data)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) { tx.Status, tx.Hash = ozStatusSubmitted, pendingHash.Hex() })
	h.reconcile(t, job.ID)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) { tx.Status, tx.Hash = ozStatusFailed, common.HexToHash("0x99").Hex() })
	job = h.reconcile(t, job.ID)
	if isTerminalStatus(job.Status) || !nullifierHeld(t, h) {
		t.Fatalf("job = %s; a still-pending hash must keep the nullifier", job.Status)
	}
}

func TestAdmissionRefusedWhilePausedOrMisconfigured(t *testing.T) {
	h := newOZHarness(t)
	if err := h.r.verifyOZRelayer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reason := h.r.ozAdmissionError(); reason != "" {
		t.Fatalf("verified relayer must admit, got %q", reason)
	}
	h.oz.paused = true
	h.r.refreshOZStatus(context.Background())
	if reason := h.r.ozAdmissionError(); !strings.Contains(reason, "paused") {
		t.Fatalf("paused OZ must refuse admission, got %q", reason)
	}
	h.oz.paused = false
	h.oz.setDown(true)
	h.r.refreshOZStatus(context.Background())
	if reason := h.r.ozAdmissionError(); !strings.Contains(reason, "unavailable") {
		t.Fatalf("unreachable OZ must refuse admission, got %q", reason)
	}
	h.oz.setDown(false)
	h.r.refreshOZStatus(context.Background())
	h.oz.policy.WhitelistReceivers = []string{"0x3333333333333333333333333333333333333333"}
	h.r.ozMu.Lock()
	h.r.ozVerifiedAt = time.Time{}
	h.r.ozMu.Unlock()
	h.r.reverifyOZRelayer(context.Background())
	if reason := h.r.ozAdmissionError(); !strings.Contains(reason, "misconfigured") {
		t.Fatalf("failed re-verification must refuse admission, got %q", reason)
	}
	// Fixed on the OZ side: the very next round reopens admission.
	h.oz.policy.WhitelistReceivers = []string{testPool.Hex()}
	h.r.reverifyOZRelayer(context.Background())
	if reason := h.r.ozAdmissionError(); reason != "" {
		t.Fatalf("a failed re-verification must be retried next round, still refused: %q", reason)
	}
}

func TestStatusIsUnhealthyUntilOZVerified(t *testing.T) {
	r := &Relayer{}
	r.startupPending.Store(true)
	if errs := r.cachedReadinessErrors(); len(errs) == 0 {
		t.Fatal("/status must not report healthy before OZ is verified")
	}
	r.startupPending.Store(false)
	if errs := r.cachedReadinessErrors(); len(errs) != 0 {
		t.Fatalf("unexpected errors after verification: %v", errs)
	}
}

func TestLowerOZGasPriceCapLimitsSubmissions(t *testing.T) {
	h := newOZHarness(t)
	h.oz.policy.GasPriceCap = "20000000000"
	if err := h.r.verifyOZRelayer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.r.maxGasPrice(); got.Cmp(big.NewInt(20_000_000_000)) != 0 {
		t.Fatalf("maxGasPrice = %s, want OZ cap", got)
	}
	budget := new(big.Int).Mul(big.NewInt(1_000_000_000_000), big.NewInt(400_000))
	if err := h.r.checkFeeBudget(budget, 400_000, big.NewInt(10_000_000_000), "test"); err != nil {
		t.Fatalf("market below OZ cap must pass: %v", err)
	}
	if got := h.r.marketMaxFee(big.NewInt(10_000_000_000), big.NewInt(1)); got.Cmp(big.NewInt(20_000_000_000)) != 0 {
		t.Fatalf("maxFee = %s, want OZ cap 20 gwei", got)
	}
	if err := h.r.checkFeeBudget(budget, 400_000, big.NewInt(30_000_000_000), "test"); err == nil {
		t.Fatal("market above OZ gas_price_cap must be refused")
	}
}

func (f *fakeOZ) setDown(down bool) {
	f.mu.Lock()
	f.down = down
	f.mu.Unlock()
}

func TestOZUnavailableJobWaitsThenResolvesByTransactionID(t *testing.T) {
	h := newOZHarness(t)
	job, data := h.createSubmittedJob(t)
	h.oz.setDown(true)
	minedHash := h.chain.mine(t, 7, testPool, data, types.ReceiptStatusSuccessful)
	for i := 0; i < 3; i++ {
		if job = h.reconcile(t, job.ID); isTerminalStatus(job.Status) || !nullifierHeld(t, h) {
			t.Fatalf("job = %+v; while OZ is down it must wait and keep the nullifier", job)
		}
	}
	h.oz.setDown(false)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) { tx.Status, tx.Hash = ozStatusConfirmed, minedHash.Hex() })
	job = h.reconcile(t, job.ID)
	if job.Status != StatusConfirmed || job.TxHash != minedHash.Hex() || nullifierHeld(t, h) {
		t.Fatalf("job = %+v, want CONFIRMED via OZ transaction id", job)
	}
}

func TestUnknownSubmitWaitsForOZThenRecoversTransactionID(t *testing.T) {
	h := newOZHarness(t)
	h.oz.submitCode, h.oz.submitError = http.StatusBadGateway, "gateway"
	job, data := h.createSubmittedJob(t)
	if job.Status != StatusSubmitting || job.OZTxID != "" {
		t.Fatalf("job = %+v, want SUBMITTING without OZ id", job)
	}
	// OZ did create the transaction although the response was lost.
	h.oz.mu.Lock()
	h.oz.txs["tx-lost"] = &ozTransaction{ID: "tx-lost", Status: ozStatusSubmitted, To: testPool.Hex(), Data: hexutil.Encode(data),
		Hash: common.HexToHash("0xab").Hex(), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	h.oz.mu.Unlock()
	h.oz.setDown(true)
	// Even long after submission, an unreachable OZ never releases.
	if err := h.r.updateJobCritical(job.ID, func(j *Job) { j.SubmitStartedAt = time.Now().Add(-time.Hour) }); err != nil {
		t.Fatal(err)
	}
	if job = h.reconcile(t, job.ID); job.Status != StatusSubmitting || !nullifierHeld(t, h) {
		t.Fatalf("job = %+v; unknown outcome with OZ down must wait", job)
	}
	h.oz.setDown(false)
	if job = h.reconcile(t, job.ID); job.OZTxID != "tx-lost" || job.Status != StatusSubmitted {
		t.Fatalf("job = %+v, want recovered OZ transaction id", job)
	}
}
