package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrQueueFull is returned when accepting a request would exceed the active or
// protected job capacity.
var ErrQueueFull = errors.New("relayer queue is full")

// ErrNullifierReverted is returned when a withdrawal of this note already
// reverted on chain, so new requests for its nullifier are refused permanently
// (one note cannot make the relayer pay for reverted transactions repeatedly).
var ErrNullifierReverted = errors.New("a withdrawal of this note reverted on chain; this relayer no longer accepts it")

type JobStore interface {
	Close() error
	CreateOrGetPending(ctx context.Context, req WithdrawRequest, reqHash, nullifierKey string) (*Job, bool, error)
	Get(ctx context.Context, id string) (*Job, bool, error)
	Update(ctx context.Context, id string, fn func(*Job)) error
	FailAndRelease(ctx context.Context, id, reason string, retryable bool) error
	MarkReverted(ctx context.Context, nullifierKey string) error
	ListActive(ctx context.Context) ([]*Job, error)
	Stats(ctx context.Context) (StoreStats, error)
	Cleanup(ctx context.Context) error
}

func cloneJob(job *Job) *Job {
	cp := *job
	cp.TxHashes = append([]string(nil), job.TxHashes...)
	return &cp
}

const (
	// storeWriteAttempts bounds how often a critical store write is tried.
	storeWriteAttempts = 3
	// storeWriteTimeout bounds a single store write attempt.
	storeWriteTimeout = 6 * time.Second
	// storeRetryBackoff is multiplied by the attempt number between retries.
	storeRetryBackoff = 100 * time.Millisecond
)

// errStorePermanent marks deterministic store failures (missing job, rejected
// update) that cannot succeed on retry; retryStoreWrite gives up immediately.
var errStorePermanent = errors.New("permanent store error")

// updateJobCritical applies fn to the job through retryStoreWrite and logs a
// persistence failure. It is used for state transitions that must not be lost.
func (r *Relayer) updateJobCritical(jobID string, fn func(*Job)) error {
	err := r.retryStoreWrite(func(ctx context.Context) error {
		return r.store.Update(ctx, jobID, fn)
	})
	if err != nil {
		logErrorf("critical job persistence failed job=%s: %v", jobID, err)
	}
	return err
}

// failJobAndRelease finalizes a job as FAILED and releases its nullifier.
// Callers must only use it when OZ provably holds no live transaction for it.
func (r *Relayer) failJobAndRelease(jobID string, err error) {
	retryable := failureRetryable(err)
	logErrorf("job %s failed (retryable=%v): %v", jobID, retryable, err)
	storeErr := r.retryStoreWrite(func(ctx context.Context) error {
		return r.store.FailAndRelease(ctx, jobID, publicFailureReason(err), retryable)
	})
	if storeErr != nil {
		logErrorf("job %s failure could not be persisted; nullifier ownership retained: %v", jobID, storeErr)
	}
}

// retryStoreWrite runs operation up to storeWriteAttempts times with a linear
// backoff. Each attempt gets its own storeWriteTimeout context derived from
// context.Background(): writes are deliberately detached from the runtime
// context so job transitions still persist while the relayer shuts down.
// Errors wrapping errStorePermanent are returned at once without retrying.
func (r *Relayer) retryStoreWrite(operation func(context.Context) error) error {
	var lastErr error
	for attempt := 1; attempt <= storeWriteAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
		err := operation(ctx)
		cancel()
		if err == nil {
			return nil
		}
		if errors.Is(err, errStorePermanent) {
			return err
		}
		lastErr = err
		if attempt < storeWriteAttempts {
			time.Sleep(time.Duration(attempt) * storeRetryBackoff)
		}
	}
	return fmt.Errorf("after %d attempts: %w", storeWriteAttempts, lastErr)
}
