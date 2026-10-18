package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

// An unknown submit whose OZ scan can never complete is retained, but must
// not stop reconciliation of the jobs behind it.
func TestIncompleteOZScanDoesNotStallOtherJobs(t *testing.T) {
	h := newOZHarness(t)
	h.oz.submitCode, h.oz.submitError = http.StatusBadGateway, "gateway"
	stuck, _ := submitRequest(t, h, requestWithNullifier("0a"))
	if stuck.Status != StatusSubmitting {
		t.Fatalf("setup: %+v", stuck)
	}
	h.oz.mu.Lock()
	h.oz.submitCode = 0
	h.oz.txs["bad-record"] = &ozTransaction{ID: "bad-record", Status: ozStatusConfirmed, To: testOZSender.Hex(), Data: "0x", CreatedAt: "not-a-time"}
	h.oz.mu.Unlock()

	healthy, data := submitRequest(t, h, requestWithNullifier("0b"))
	mined := h.chain.mine(t, 7, testPool, data, types.ReceiptStatusSuccessful)
	h.oz.set(healthy.OZTxID, func(tx *ozTransaction) { tx.Status, tx.Hash = ozStatusConfirmed, mined.Hex() })

	h.r.refreshOZStatus(context.Background())
	h.r.reconcileActiveJobs(context.Background())
	if got := h.job(t, healthy.ID); got.Status != StatusConfirmed {
		t.Fatalf("job behind an incomplete scan must still reconcile, got %s", got.Status)
	}
	if got := h.job(t, stuck.ID); got.Status != StatusSubmitting {
		t.Fatalf("the unknown submit must stay retained, got %s", got.Status)
	}
}

// The nonce proof must read isSpent at the same finalized block: a backend
// whose latest view lags must not release an already spent nullifier.
func TestNonceProofReadsIsSpentAtFinalizedBlock(t *testing.T) {
	h := newOZHarness(t)
	job, _ := h.createSubmittedJob(t)
	h.chain.mu.Lock()
	h.chain.spent = true
	h.chain.finalizedNonce = 8
	h.chain.mu.Unlock()
	h.oz.set(job.OZTxID, func(tx *ozTransaction) {
		tx.Status, tx.Hash, tx.From, tx.Nonce = ozStatusExpired, common.HexToHash("0x99").Hex(), testOZSender.Hex(), "7"
	})
	overrideChainRPC(t, h, func(method string, params []json.RawMessage) (any, bool, bool) {
		if method == "eth_call" && len(params) > 1 && string(params[1]) == `"latest"` {
			return hexutil.Encode(make([]byte, 32)), true, false // lagging latest view
		}
		return nil, false, false
	})
	if got := h.reconcile(t, job.ID); got.Status == StatusFailed && got.Retryable {
		t.Fatalf("spent at finalized must not be released as 'did not execute': %+v", got)
	}
}

// OZ "confirmed" with a matching record but a different transaction on chain
// must be resolved by chain rather than waiting forever.
func TestConfirmedMismatchOnChainIsResolvedByChain(t *testing.T) {
	h := newOZHarness(t)
	job, _ := h.createSubmittedJob(t)
	other := h.chain.mine(t, 7, testPool, []byte{0x01}, types.ReceiptStatusSuccessful)
	h.chain.mu.Lock()
	h.chain.spent = false
	h.chain.finalizedNonce = 8
	h.chain.mu.Unlock()
	_, data, _ := h.r.jobCall(job)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) {
		tx.Status, tx.Hash, tx.To, tx.Data = ozStatusConfirmed, other.Hex(), testPool.Hex(), hexutil.Encode(data)
		tx.From, tx.Nonce = testOZSender.Hex(), "7"
	})
	if got := h.reconcile(t, job.ID); got.Status != StatusFailed || nullifierHeld(t, h) {
		t.Fatalf("mismatch must be resolved by chain (nonce consumed, unspent), got %+v", got)
	}
}

// A transaction the node knows as mined, but without a receipt yet (lagging
// or load-balanced RPC), must block abandonment like a pending one.
func TestMinedWithoutReceiptIsNotAbandoned(t *testing.T) {
	h := newOZHarness(t)
	job, data := h.createSubmittedJob(t)
	hash := h.chain.mine(t, 7, testPool, data, types.ReceiptStatusSuccessful)
	h.chain.mu.Lock()
	delete(h.chain.receipts, hash)
	h.chain.spent = false
	h.chain.mu.Unlock()
	h.oz.set(job.OZTxID, func(tx *ozTransaction) { tx.Status, tx.Hash = ozStatusSubmitted, hash.Hex() })
	h.reconcile(t, job.ID)
	h.oz.mu.Lock()
	delete(h.oz.txs, job.OZTxID)
	h.oz.mu.Unlock()
	if err := h.r.updateJobCritical(job.ID, func(j *Job) { j.SubmitStartedAt = time.Now().Add(-24 * time.Hour) }); err != nil {
		t.Fatal(err)
	}
	if got := h.reconcile(t, job.ID); isTerminalStatus(got.Status) || !nullifierHeld(t, h) {
		t.Fatalf("a mined transaction without a receipt must keep the job waiting: %+v", got)
	}
}

// A withdrawal that passed simulation but reverted on chain (e.g. an EIP-7702
// delegation added to the recipient after preflight) fails permanently, and
// every later request for the same note is refused: each revert costs gas.
func TestRevertedNullifierIsRefused(t *testing.T) {
	h := newOZHarness(t)
	job, data := h.createSubmittedJob(t)
	reverted := h.chain.mine(t, 7, testPool, data, types.ReceiptStatusFailed)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) {
		tx.Status, tx.Hash, tx.StatusReason = ozStatusFailed, reverted.Hex(), "reverted"
	})
	h.chain.setFinalizedBlock(16)
	if job = h.reconcile(t, job.ID); job.Status != StatusFailed || job.Retryable {
		t.Fatalf("setup: job = %s retryable=%v", job.Status, job.Retryable)
	}

	ctx := context.Background()
	// The identical request returns the failed job.
	if same, duplicate, err := h.r.store.CreateOrGetPending(ctx, job.Request, job.RequestHash, job.NullifierKey); err != nil || !duplicate || same.ID != job.ID {
		t.Fatalf("identical request: dup=%v err=%v", duplicate, err)
	}
	// A new proof for the same note (new recipient) is refused, also after Cleanup.
	if err := h.r.store.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	req := testWithdrawRequest()
	req.Args = append([]string(nil), req.Args...)
	req.Args[2] = "0x4444444444444444444444444444444444444444"
	req.Proof = "0x" + strings.Repeat("cd", 256)
	if _, _, err := h.r.store.CreateOrGetPending(ctx, req, requestHash("1", req), h.r.nullifierKey(req)); !errors.Is(err, ErrNullifierReverted) {
		t.Fatalf("reverted note re-admitted: err=%v", err)
	}
}

// A revert whose proof root has left the pool's root history is not the
// user's doing: the job fails retryably and the note is not refused.
func TestRevertWithExpiredRootDoesNotRefuseNote(t *testing.T) {
	h := newOZHarness(t)
	job, data := h.createSubmittedJob(t)
	reverted := h.chain.mine(t, 7, testPool, data, types.ReceiptStatusFailed)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) {
		tx.Status, tx.Hash, tx.StatusReason = ozStatusFailed, reverted.Hex(), "reverted"
	})
	h.chain.mu.Lock()
	h.chain.rootUnknown = true
	h.chain.mu.Unlock()
	h.chain.setFinalizedBlock(16)
	if job = h.reconcile(t, job.ID); job.Status != StatusFailed || !job.Retryable || !strings.Contains(job.FailedReason, "merkle root") {
		t.Fatalf("job = %s retryable=%v reason=%q", job.Status, job.Retryable, job.FailedReason)
	}
	if nullifierHeld(t, h) {
		t.Fatal("nullifier must be released")
	}
	req := testWithdrawRequest()
	req.Args = append([]string(nil), req.Args...)
	req.Args[0] = "0x" + strings.Repeat("ab", 32) // new proof with a current root
	req.Proof = "0x" + strings.Repeat("cd", 256)
	if _, duplicate, err := h.r.store.CreateOrGetPending(context.Background(), req, requestHash("1", req), h.r.nullifierKey(req)); err != nil || duplicate {
		t.Fatalf("note with an expired-root revert must stay usable: dup=%v err=%v", duplicate, err)
	}
}

// Gas spikes and missing prices are retried server-side for a few minutes
// instead of failing at once (the fee is fixed in the proof).
func TestDeferrablePreflightFailureRequeues(t *testing.T) {
	store := newTestSQLiteStore(t)
	r := &Relayer{store: store}
	ctx := context.Background()
	job, _, err := store.CreateOrGetPending(ctx, sampleWithdrawRequest(), "defer", "defer-nullifier")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(ctx, job.ID, func(j *Job) { transitionJob(j, StatusPreparing) }); err != nil {
		t.Fatal(err)
	}
	if r.deferPreflight(job.ID, permanentFailure(errors.New("simulation reverted"))) {
		t.Fatal("a permanent failure must not be deferred")
	}
	if r.deferPreflight(job.ID, retryableInternalFailure(errors.New("rpc down"))) {
		t.Fatal("only market conditions are deferred")
	}
	if !r.deferPreflight(job.ID, deferrableFailure(errors.New("gas price above limit"))) {
		t.Fatal("gas spike within the window must be deferred")
	}
	got, _, _ := store.Get(ctx, job.ID)
	if got.Status != StatusQueued {
		t.Fatalf("status = %s, want QUEUED", got.Status)
	}
	if r.preflightDue(job.ID) {
		t.Fatal("a deferred job must wait preflightRetryInterval")
	}
	if _, dup, err := store.CreateOrGetPending(ctx, sampleWithdrawRequest(), "defer", "defer-nullifier"); err != nil || !dup {
		t.Fatalf("nullifier must stay locked while deferred: dup=%v err=%v", dup, err)
	}
	// Window over: the failure becomes final.
	if err := store.Update(ctx, job.ID, func(j *Job) { transitionJob(j, StatusPreparing) }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET created_at=? WHERE id=?`,
		time.Now().Add(-preflightRetryWindow).UnixMilli(), job.ID); err != nil {
		t.Fatal(err)
	}
	if r.deferPreflight(job.ID, deferrableFailure(errors.New("gas price above limit"))) {
		t.Fatal("no deferral after the retry window")
	}
}

// ---------------------------------------------------------------------------
// 1. PREPARING recovery
// ---------------------------------------------------------------------------

func TestPreparingJobIsPreflightedAgainAfterShutdownAndSubmittedOnce(t *testing.T) {
	h := newOZHarness(t)
	dbPath := filepath.Join(t.TempDir(), "restart.db")
	restartStore(t, h, dbPath)
	configureWorkers(t, h)
	chain := newRecoveryChain(t, h)

	// Shutdown while the preflight RPC is in flight: the worker is cancelled
	// and the job is left in PREPARING.
	chain.close()
	req := preflightableRequest("a1")
	job, _, err := h.r.store.CreateOrGetPending(context.Background(), req, requestHash("1", req), h.r.nullifierKey(req))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.r.processWithdraw(ctx, job.ID, req); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for chain.requests.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if got := h.job(t, job.ID); got.Status != StatusPreparing {
		t.Fatalf("status after shutdown = %s, want PREPARING", got.Status)
	}
	chain.open()

	restartStore(t, h, dbPath)
	for round := 0; round < 3; round++ {
		h.r.reconcileActiveJobs(context.Background())
		waitWorkers(t, h)
	}
	got := h.job(t, job.ID)
	if got.Status != StatusSubmitted || got.OZTxID == "" || !nullifierHeld(t, h) {
		t.Fatalf("job = %s ozTx=%q failed=%q; want SUBMITTED to OZ after recovery", got.Status, got.OZTxID, got.FailedReason)
	}
	if n := h.submitCount(); n != 1 {
		t.Fatalf("OZ submits = %d, want exactly 1", n)
	}
}

func TestCrashedPreparingJobResumesWithSingleWorker(t *testing.T) {
	h := newOZHarness(t)
	dbPath := filepath.Join(t.TempDir(), "crash.db")
	restartStore(t, h, dbPath)
	configureWorkers(t, h)
	chain := newRecoveryChain(t, h)

	req := preflightableRequest("b2")
	job, _, err := h.r.store.CreateOrGetPending(context.Background(), req, requestHash("1", req), h.r.nullifierKey(req))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.r.updateJobCritical(job.ID, func(j *Job) { transitionJob(j, StatusPreparing) }); err != nil {
		t.Fatal(err)
	}
	restartStore(t, h, dbPath) // kill -9 during preflight

	// Several reconcile rounds fire while the recovered preflight is still
	// running: the job claim must keep it to one worker and one submit.
	chain.close()
	for round := 0; round < 3; round++ {
		h.r.reconcileActiveJobs(context.Background())
	}
	chain.open()
	waitWorkers(t, h)
	h.r.reconcileActiveJobs(context.Background())
	waitWorkers(t, h)

	if got := h.job(t, job.ID); got.Status != StatusSubmitted || got.OZTxID == "" {
		t.Fatalf("job = %s failed=%q; want SUBMITTED", got.Status, got.FailedReason)
	}
	if n := h.submitCount(); n != 1 {
		t.Fatalf("OZ submits = %d, want exactly 1", n)
	}
}

func TestRecoveredPreparingJobFailureReleasesNullifier(t *testing.T) {
	h := newOZHarness(t)
	configureWorkers(t, h)
	h.r.chain = nil // replaced below by an RPC that rejects everything
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)
	client, err := dialRPC("HTTP_RPC_URL", server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	h.r.chain = singleRPC(client)

	req := preflightableRequest("c3")
	job, _, err := h.r.store.CreateOrGetPending(context.Background(), req, requestHash("1", req), h.r.nullifierKey(req))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.r.updateJobCritical(job.ID, func(j *Job) { transitionJob(j, StatusPreparing) }); err != nil {
		t.Fatal(err)
	}
	h.r.reconcileActiveJobs(context.Background())
	waitWorkers(t, h)
	got := h.job(t, job.ID)
	if got.Status != StatusFailed || !got.Retryable || nullifierHeld(t, h) {
		t.Fatalf("job = %+v; a recovered job must reach a definite state, not stay PREPARING", got)
	}
	if n := h.submitCount(); n != 0 {
		t.Fatalf("OZ submits = %d, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// 2. Unknown submit after a configuration change
// ---------------------------------------------------------------------------

func lostSubmit(t *testing.T) (*ozHarness, *Job) {
	t.Helper()
	h := newOZHarness(t)
	h.oz.submitCode, h.oz.submitError = http.StatusBadGateway, "gateway"
	job, _ := h.createSubmittedJob(t)
	if job.Status != StatusSubmitting || job.OZTxID != "" {
		t.Fatalf("setup: job = %+v", job)
	}
	h.oz.mu.Lock()
	sent := h.oz.submitted[0]
	h.oz.submitCode = 0
	h.oz.txs["tx-lost"] = &ozTransaction{ID: "tx-lost", Status: ozStatusSubmitted, To: sent.To, Data: sent.Data,
		Hash: common.HexToHash("0xab").Hex(), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	h.oz.mu.Unlock()
	return h, job
}

func TestLostSubmitSurvivesConfigurationChange(t *testing.T) {
	newReward := common.HexToAddress("0x9999999999999999999999999999999999999999")
	for name, change := range map[string]func(*Relayer){
		"reward account changed": func(r *Relayer) { r.cfg.RewardAccount = newReward },
		"pool removed":           func(r *Relayer) { r.cfg.InstanceByAddress = map[common.Address]InstanceMeta{} },
	} {
		t.Run(name, func(t *testing.T) {
			h, job := lostSubmit(t)
			change(h.r) // restart with a new configuration, same database
			job = h.reconcile(t, job.ID)
			if job.OZTxID != "tx-lost" || job.Status != StatusSubmitted || !nullifierHeld(t, h) {
				t.Fatalf("job = %s ozTx=%q failed=%q; want the original OZ transaction matched and the nullifier held",
					job.Status, job.OZTxID, job.FailedReason)
			}
			// A new proof for the same nullifier must not get a second job.
			other := testWithdrawRequest()
			other.Args = append([]string(nil), other.Args...)
			other.Args[3] = newReward.Hex()
			owner, dup, err := h.r.store.CreateOrGetPending(context.Background(), other, requestHash("1", other), h.r.nullifierKey(other))
			if err != nil || !dup || owner.ID != job.ID {
				t.Fatalf("second request: owner=%v dup=%v err=%v; want the live job to keep the nullifier", owner, dup, err)
			}
		})
	}
}

func TestLostSubmitWithUnrebuildableRequestStaysQuarantined(t *testing.T) {
	h, job := lostSubmit(t)
	// Corrupt the persisted request: it can no longer be turned into calldata.
	if _, err := h.r.store.(*SQLiteStore).db.Exec(`UPDATE jobs SET request_json=? WHERE id=?`,
		`{"proof":"0x","contract":"0x12D66f87A04A9E220743712cE6d9bB1B5616B8Fc","args":[]}`, job.ID); err != nil {
		t.Fatal(err)
	}
	job = h.reconcile(t, job.ID)
	if job.Status != StatusSubmitting || !nullifierHeld(t, h) {
		t.Fatalf("job = %+v; an unrebuildable unknown submit must stay quarantined", job)
	}
}

// ---------------------------------------------------------------------------
// 3. Chain fallback finality
// ---------------------------------------------------------------------------

func TestUnfinalizedRevertKeepsNullifierAcrossReorg(t *testing.T) {
	h := newOZHarness(t)
	job, data := h.createSubmittedJob(t)
	reverted := h.chain.mine(t, 7, testPool, data, types.ReceiptStatusFailed)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) {
		tx.Status, tx.Hash, tx.From, tx.Nonce, tx.StatusReason = ozStatusFailed, reverted.Hex(), testOZSender.Hex(), "7", "reverted"
	})
	if job = h.reconcile(t, job.ID); isTerminalStatus(job.Status) || !nullifierHeld(t, h) {
		t.Fatalf("job = %+v; an unfinalized revert must not release", job)
	}
	// Short reorg: the revert drops back into the mempool.
	h.chain.mu.Lock()
	delete(h.chain.receipts, reverted)
	h.chain.pending[reverted] = true
	h.chain.mu.Unlock()
	if job = h.reconcile(t, job.ID); isTerminalStatus(job.Status) || !nullifierHeld(t, h) {
		t.Fatalf("job = %+v; a pending replacement must keep the nullifier", job)
	}
	other := testWithdrawRequest()
	other.Args = append([]string(nil), other.Args...)
	other.Args[2] = "0x4444444444444444444444444444444444444444"
	if owner, dup, err := h.r.store.CreateOrGetPending(context.Background(), other, requestHash("1", other), h.r.nullifierKey(other)); err != nil || !dup || owner.ID != job.ID {
		t.Fatalf("second request: owner=%v dup=%v err=%v; want the live job to keep the nullifier", owner, dup, err)
	}
}

func TestReceiptReorganizedOutBeforeFinalityIsNotFinal(t *testing.T) {
	h := newOZHarness(t)
	job, data := h.createSubmittedJob(t)
	mined := h.chain.mine(t, 7, testPool, data, types.ReceiptStatusSuccessful)
	h.chain.setFinalizedBlock(16)
	if final, ok, err := h.r.receiptFinalized(context.Background(), mined); err != nil || !final || !ok {
		t.Fatalf("finalized receipt: final=%v ok=%v err=%v", final, ok, err)
	}
	h.chain.setFinalizedBlock(15)
	if final, _, err := h.r.receiptFinalized(context.Background(), mined); err != nil || final {
		t.Fatalf("receipt above finalized block: final=%v err=%v", final, err)
	}
	h.chain.mu.Lock()
	delete(h.chain.receipts, mined)
	h.chain.mu.Unlock()
	h.chain.setFinalizedBlock(16)
	if final, _, err := h.r.receiptFinalized(context.Background(), mined); err != nil || final {
		t.Fatalf("reorganized receipt: final=%v err=%v", final, err)
	}
	_ = job
}

func TestSpendSeenOnlyAtLatestIsNotPermanent(t *testing.T) {
	h := newOZHarness(t)
	job, _ := h.createSubmittedJob(t)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) {
		tx.Status, tx.Hash, tx.From, tx.Nonce = ozStatusExpired, common.HexToHash("0x99").Hex(), testOZSender.Hex(), "7"
	})
	h.chain.mu.Lock()
	h.chain.spent, h.chain.spendNotFinal = true, true
	h.chain.mu.Unlock()
	if job = h.reconcile(t, job.ID); isTerminalStatus(job.Status) || !nullifierHeld(t, h) {
		t.Fatalf("job = %+v; a spend that is not finalized must not fail the job permanently", job)
	}
	h.chain.mu.Lock()
	h.chain.spendNotFinal = false
	h.chain.mu.Unlock()
	if job = h.reconcile(t, job.ID); job.Status != StatusFailed || job.Retryable || !strings.Contains(job.FailedReason, "already spent") {
		t.Fatalf("job = %+v, want permanent spent failure once finalized", job)
	}
}

func TestFinalityUnavailableKeepsJobWaiting(t *testing.T) {
	h := newOZHarness(t)
	job, data := h.createSubmittedJob(t)
	mined := h.chain.mine(t, 7, testPool, data, types.ReceiptStatusSuccessful)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) { tx.Status, tx.Hash = ozStatusMined, mined.Hex() })
	h.reconcile(t, job.ID)
	h.oz.mu.Lock()
	delete(h.oz.txs, job.OZTxID)
	h.oz.mu.Unlock()
	// An RPC without the finalized tag: the job must wait, never guess.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "eth_getBlockByNumber") {
			http.Error(w, "finalized not supported", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.chain.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := dialRPC("HTTP_RPC_URL", server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	h.r.chain = singleRPC(client)
	if job = h.reconcile(t, job.ID); isTerminalStatus(job.Status) || !nullifierHeld(t, h) {
		t.Fatalf("job = %+v; without a finalized view the job must keep waiting", job)
	}
}
