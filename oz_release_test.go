package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"

	"github.com/ethereum/go-ethereum/common"
)

// expireWithNonce makes the job's OZ transaction final with a recorded hash
// that the chain never saw, sent from testOZSender at nonce 7.
func expireWithNonce(h *ozHarness, job *Job, status string) {
	h.oz.set(job.OZTxID, func(tx *ozTransaction) {
		tx.Status, tx.Hash, tx.From, tx.Nonce = status, common.HexToHash("0x99").Hex(), testOZSender.Hex(), "7"
	})
}

func TestFinalOZTransactionNeverBroadcastReleases(t *testing.T) {
	h := newOZHarness(t)
	job, _ := h.createSubmittedJob(t)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) {
		tx.Status, tx.StatusReason = ozStatusFailed, "insufficient balance"
	})
	job = h.reconcile(t, job.ID)
	if job.Status != StatusFailed || !job.Retryable || nullifierHeld(t, h) {
		t.Fatalf("job = %+v; an unsigned final transaction must release for retry", job)
	}
	if !strings.Contains(job.FailedReason, "never broadcast") {
		t.Fatalf("failed reason = %q", job.FailedReason)
	}
}

func TestFinalizedNonceReleasesOnlyOnceConsumed(t *testing.T) {
	for _, status := range []string{ozStatusExpired, ozStatusCanceled, ozStatusFailed} {
		t.Run(status, func(t *testing.T) {
			h := newOZHarness(t)
			job, _ := h.createSubmittedJob(t)
			expireWithNonce(h, job, status)

			h.chain.setFinalizedNonce(7) // slot 7 not yet finalized: a replacement could still land
			if job = h.reconcile(t, job.ID); isTerminalStatus(job.Status) || !nullifierHeld(t, h) {
				t.Fatalf("job = %+v; unconsumed nonce must keep the nullifier", job)
			}

			h.chain.setFinalizedNonce(8) // slot 7 finalized and isSpent=false: withdrawal cannot execute
			job = h.reconcile(t, job.ID)
			if job.Status != StatusFailed || !job.Retryable || nullifierHeld(t, h) {
				t.Fatalf("job = %+v; consumed nonce must release for retry", job)
			}
			if !strings.Contains(job.FailedReason, "nonce 7 finalized") {
				t.Fatalf("failed reason = %q", job.FailedReason)
			}
		})
	}
}

func TestConsumedNonceWithSpentNullifierIsPermanent(t *testing.T) {
	h := newOZHarness(t)
	job, _ := h.createSubmittedJob(t)
	expireWithNonce(h, job, ozStatusExpired)
	h.chain.setFinalizedNonce(8)
	h.chain.mu.Lock()
	h.chain.spent = true // someone else withdrew this note
	h.chain.mu.Unlock()
	if job = h.reconcile(t, job.ID); job.Status != StatusFailed || job.Retryable {
		t.Fatalf("job = %+v, want permanent FAILED", job)
	}
}

func TestNonceProofRequiresFinalOZRecord(t *testing.T) {
	h := newOZHarness(t)
	job, _ := h.createSubmittedJob(t)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) { tx.Status, tx.Hash = ozStatusSubmitted, common.HexToHash("0x99").Hex() })
	h.reconcile(t, job.ID)
	h.oz.mu.Lock()
	delete(h.oz.txs, job.OZTxID) // OZ pruned the record: nonce is unknown
	h.oz.mu.Unlock()
	h.chain.setFinalizedNonce(100)
	if job = h.reconcile(t, job.ID); isTerminalStatus(job.Status) || !nullifierHeld(t, h) {
		t.Fatalf("job = %+v; without the final OZ record the nullifier must stay held", job)
	}
}

// prunedJob submits a job whose OZ record is then pruned, with its single
// hash unknown to the chain, submitted `age` ago.
func prunedJob(t *testing.T, h *ozHarness, age time.Duration) *Job {
	t.Helper()
	job, _ := h.createSubmittedJob(t)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) { tx.Status, tx.Hash = ozStatusSubmitted, common.HexToHash("0x99").Hex() })
	h.reconcile(t, job.ID)
	h.oz.mu.Lock()
	delete(h.oz.txs, job.OZTxID)
	h.oz.mu.Unlock()
	if err := h.r.updateJobCritical(job.ID, func(j *Job) { j.SubmitStartedAt = time.Now().Add(-age) }); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestPrunedJobWithoutChainResultIsAbandonedAfterValidity(t *testing.T) {
	h := newOZHarness(t)
	job := prunedJob(t, h, h.r.ozPrunedAbandonAfter()+time.Minute)
	if job = h.reconcile(t, job.ID); job.Status != StatusFailed || !job.Retryable || nullifierHeld(t, h) {
		t.Fatalf("job = %+v; an old pruned job with no chain result must be abandoned and released", job)
	}
}

func TestPrunedJobIsKeptWhileStillPossiblyLive(t *testing.T) {
	h := newOZHarness(t)
	job := prunedJob(t, h, h.r.ozPrunedAbandonAfter()-time.Minute)
	if job = h.reconcile(t, job.ID); isTerminalStatus(job.Status) || !nullifierHeld(t, h) {
		t.Fatalf("job = %+v; before valid_until + margin the job must wait", job)
	}

	h = newOZHarness(t)
	job, data := h.createSubmittedJob(t)
	pending := h.chain.broadcast(t, 7, testPool, data)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) { tx.Status, tx.Hash = ozStatusSubmitted, pending.Hex() })
	h.reconcile(t, job.ID)
	h.oz.mu.Lock()
	delete(h.oz.txs, job.OZTxID)
	h.oz.mu.Unlock()
	if err := h.r.updateJobCritical(job.ID, func(j *Job) { j.SubmitStartedAt = time.Now().Add(-24 * time.Hour) }); err != nil {
		t.Fatal(err)
	}
	if job = h.reconcile(t, job.ID); isTerminalStatus(job.Status) || !nullifierHeld(t, h) {
		t.Fatalf("job = %+v; a transaction still pending on chain is never abandoned", job)
	}
}

func TestReconcileRoundSkipsOZWhileStatusUnavailable(t *testing.T) {
	h := newOZHarness(t)
	for i := 0; i < ozFailuresPerRound+2; i++ {
		req := testWithdrawRequest()
		req.Args[1] = "0x" + strings.Repeat("0"+string(rune('a'+i)), 32)
		job, _, err := h.r.store.CreateOrGetPending(context.Background(), req, requestHash("1", req), h.r.nullifierKey(req))
		if err != nil {
			t.Fatal(err)
		}
		for _, status := range []JobStatus{StatusPreparing, StatusSubmitting, StatusSubmitted} {
			if err := h.r.updateJobCritical(job.ID, func(j *Job) { transitionJob(j, status) }); err != nil {
				t.Fatal(err)
			}
		}
		if err := h.r.updateJobCritical(job.ID, func(j *Job) { j.OZTxID = "tx-" + job.ID }); err != nil {
			t.Fatal(err)
		}
	}

	// Status endpoint down: no per-job OZ polling at all.
	h.oz.setDown(true)
	h.r.refreshOZStatus(context.Background())
	h.r.reconcileActiveJobs(context.Background())
	if gets := h.oz.transactionGets(); gets != 0 {
		t.Fatalf("polled OZ %d times while its status was unavailable", gets)
	}

	// Status fine but every transaction poll fails: the round stops after
	// ozFailuresPerRound consecutive failures.
	h.oz.setDown(false)
	h.r.refreshOZStatus(context.Background())
	h.oz.setDown(true)
	h.r.reconcileActiveJobs(context.Background())
	if gets := h.oz.transactionGets(); gets != ozFailuresPerRound {
		t.Fatalf("polled OZ %d times while it failed, want %d", gets, ozFailuresPerRound)
	}
}

// One job whose OZ record keeps failing must not starve the others.
func TestOneFailingOZRecordDoesNotStarveOtherJobs(t *testing.T) {
	h := newOZHarness(t)
	bad, _ := submitRequest(t, h, requestWithNullifier("0a"))
	good, data := submitRequest(t, h, requestWithNullifier("0b"))
	h.oz.mu.Lock()
	delete(h.oz.txs, bad.OZTxID)
	h.oz.mu.Unlock()
	h.oz.failTx(bad.OZTxID)
	mined := h.chain.mine(t, 7, testPool, data, types.ReceiptStatusSuccessful)
	h.oz.set(good.OZTxID, func(tx *ozTransaction) { tx.Status, tx.Hash = ozStatusConfirmed, mined.Hex() })
	h.r.refreshOZStatus(context.Background())
	for round := 0; round < 2; round++ {
		h.r.reconcileActiveJobs(context.Background())
	}
	if got := h.job(t, good.ID); got.Status != StatusConfirmed {
		t.Fatalf("job behind a failing OZ record must reconcile, got %s", got.Status)
	}
	if got := h.job(t, bad.ID); isTerminalStatus(got.Status) {
		t.Fatalf("the failing job must stay retained, got %s", got.Status)
	}
}

func (c *fakeChain) setFinalizedBlock(number uint64) {
	c.mu.Lock()
	c.finalizedBlock = number
	c.mu.Unlock()
}

func (c *fakeChain) setFinalizedNonce(nonce uint64) {
	c.mu.Lock()
	c.finalizedNonce = nonce
	c.mu.Unlock()
}

func (f *fakeOZ) transactionGets() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.txGets
}
