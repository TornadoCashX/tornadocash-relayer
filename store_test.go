package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A submission whose outcome is unknown holds its nullifier until an operator
// acts; /health/jobs and /metrics must show it.
func TestUnknownSubmitIsListed(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	job, _, err := store.CreateOrGetPending(ctx, sampleWithdrawRequest(), "unknown-submit", "unknown-submit-nullifier")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET status=?, submit_started_at=? WHERE id=?`,
		string(StatusSubmitting), time.Now().Add(-time.Minute).UnixMilli(), job.ID); err != nil {
		t.Fatal(err)
	}
	if stats, err := store.Stats(ctx); err != nil || len(stats.UnknownSubmits) != 0 {
		t.Fatalf("a fresh submission is not unknown yet: %+v %v", stats.UnknownSubmits, err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET submit_started_at=? WHERE id=?`,
		time.Now().Add(-unknownSubmitWarnAfter-time.Minute).UnixMilli(), job.ID); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats(ctx)
	if err != nil || len(stats.UnknownSubmits) != 1 || stats.UnknownSubmits[0].ID != job.ID {
		t.Fatalf("unknown submit not listed: %+v %v", stats.UnknownSubmits, err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET oz_tx_id='oz-1' WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if stats, _ := store.Stats(ctx); len(stats.UnknownSubmits) != 0 {
		t.Fatalf("a job with an OZ id is not unknown: %+v", stats.UnknownSubmits)
	}
}

// The README "Unknown submit outcome" release SQL must leave a consistent
// state: job FAILED/retryable, nullifier free, the request admissible again.
func TestRunbookReleaseSQL(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	req := sampleWithdrawRequest()
	job, _, err := store.CreateOrGetPending(ctx, req, "runbook", "runbook-nullifier")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET status='SUBMITTING', submit_started_at=1 WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(readme)
	start := strings.Index(text, "   BEGIN;\n")
	end := strings.Index(text[start:], "COMMIT;")
	if start < 0 || end < 0 {
		t.Fatal("release SQL not found in README")
	}
	if _, err := store.db.Exec(strings.ReplaceAll(text[start:start+end+len("COMMIT;")], "<job id>", job.ID)); err != nil {
		t.Fatalf("runbook SQL: %v", err)
	}
	got, _, err := store.Get(ctx, job.ID)
	if err != nil || got.Status != StatusFailed || !got.Retryable {
		t.Fatalf("after runbook: %+v %v", got, err)
	}
	if next, duplicate, err := store.CreateOrGetPending(ctx, req, "runbook", "runbook-nullifier"); err != nil || duplicate || next.ID == job.ID {
		t.Fatalf("request not admissible after release: dup=%v err=%v", duplicate, err)
	}
}

func TestSQLiteDatabaseLockChainBindingAndDurability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relayer.db")
	first, err := NewSQLiteStore(path, "1", time.Hour, 100, 1000)
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	if _, err := NewSQLiteStore(path, "1", time.Hour, 100, 1000); !errors.Is(err, ErrStoreLocked) {
		t.Fatalf("same database should reject second process, got %v", err)
	}
	other, err := NewSQLiteStore(filepath.Join(t.TempDir(), "other.db"), "1", time.Hour, 100, 1000)
	if err != nil {
		t.Fatalf("different database should allow another process: %v", err)
	}
	defer other.Close()
	if err := first.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}
	if _, err := NewSQLiteStore(path, "10", time.Hour, 100, 1000); err == nil || !strings.Contains(err.Error(), "belongs to chain 1") {
		t.Fatalf("database should reject another chain, got %v", err)
	}
	second, err := NewSQLiteStore(path, "1", time.Hour, 100, 1000)
	if err != nil {
		t.Fatalf("reopen store for original chain: %v", err)
	}
	defer second.Close()
	var synchronous int
	if err := second.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatalf("read synchronous mode: %v", err)
	}
	if synchronous != 2 {
		t.Fatalf("SQLite synchronous=%d, want FULL(2)", synchronous)
	}
	second.db.SetMaxIdleConns(0)
	if err := second.db.Ping(); err != nil {
		t.Fatalf("recreate SQLite connection: %v", err)
	}
	if err := second.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatalf("recreated SQLite connection synchronous=%d err=%v", synchronous, err)
	}
	var userVersion int
	if err := second.db.QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil || userVersion != sqliteSchemaVersion {
		t.Fatalf("SQLite user_version=%d err=%v", userVersion, err)
	}
}

func TestSQLiteFailAndReleaseIsAtomic(t *testing.T) {
	store := newTestSQLiteStore(t)
	job, _, err := store.CreateOrGetPending(context.Background(), sampleWithdrawRequest(), "atomic-failure-request", "atomic-failure-nullifier")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.FailAndRelease(context.Background(), job.ID, "submitted transaction failed", false); err != nil {
		t.Fatalf("fail and release: %v", err)
	}
	got, ok, err := store.Get(context.Background(), job.ID)
	if err != nil || !ok || got.Status != StatusFailed {
		t.Fatalf("failed job state: job=%+v ok=%v err=%v", got, ok, err)
	}
	if got.Request.Proof != "" || len(got.Request.Args) != 0 {
		t.Fatal("failed job retained withdrawal proof")
	}
	var locks int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM pending_nullifiers WHERE job_id = ?`, job.ID).Scan(&locks); err != nil || locks != 0 {
		t.Fatalf("pending nullifier count=%d err=%v", locks, err)
	}
}

func TestSQLiteFailAndReleaseRetainsLockOnPersistenceFailure(t *testing.T) {
	store := newTestSQLiteStore(t)
	job, _, err := store.CreateOrGetPending(context.Background(), sampleWithdrawRequest(), "atomic-rollback-request", "atomic-rollback-nullifier")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_job_update BEFORE UPDATE ON jobs BEGIN SELECT RAISE(ABORT, 'forced update failure'); END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	if err := store.FailAndRelease(context.Background(), job.ID, "forced failure", true); err == nil {
		t.Fatal("forced failure unexpectedly succeeded")
	}
	if got, ok, err := store.Get(context.Background(), job.ID); err != nil || !ok || got.Status == StatusFailed {
		t.Fatalf("rolled-back job was finalized: job=%+v ok=%v err=%v", got, ok, err)
	}
	var locks int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM pending_nullifiers WHERE job_id = ?`, job.ID).Scan(&locks); err != nil || locks != 1 {
		t.Fatalf("rollback did not retain pending nullifier: count=%d err=%v", locks, err)
	}
}

func TestSQLiteStoreActiveStatsAndIndexes(t *testing.T) {
	store := newTestSQLiteStore(t)
	testStoreActiveStatsAndIndexes(t, store)
}

func TestSQLiteCleanupExpiresTerminalJobIdempotency(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "relayer.db"), "1", time.Nanosecond, 100, 1000)
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	defer store.Close()
	req := sampleWithdrawRequest()
	job, duplicate, err := store.CreateOrGetPending(context.Background(), req, "request-hash-expire", "nullifier-expire")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if duplicate {
		t.Fatalf("first job should not be duplicate")
	}
	markSQLiteJobStale(t, store, job.ID, StatusConfirmed, time.Now().Add(-time.Hour))

	if err := store.Cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	next, duplicate, err := store.CreateOrGetPending(context.Background(), req, "request-hash-expire", "nullifier-expire")
	if err != nil {
		t.Fatalf("create job after cleanup: %v", err)
	}
	if duplicate || next.ID == job.ID {
		t.Fatalf("terminal job idempotency should expire, duplicate=%v old=%s new=%s", duplicate, job.ID, next.ID)
	}
}

func testStoreActiveStatsAndIndexes(t *testing.T, store JobStore) {
	t.Helper()

	req := sampleWithdrawRequest()
	job, duplicate, err := store.CreateOrGetPending(context.Background(), req, "request-hash-a", "nullifier-a")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if duplicate {
		t.Fatalf("first job should not be duplicate")
	}

	sameReq, duplicate, err := store.CreateOrGetPending(context.Background(), req, "request-hash-a", "nullifier-other")
	if err != nil {
		t.Fatalf("same request hash: %v", err)
	}
	if !duplicate || sameReq.ID != job.ID {
		t.Fatalf("same request hash should return original job")
	}

	sameNullifier, duplicate, err := store.CreateOrGetPending(context.Background(), req, "request-hash-b", "nullifier-a")
	if err != nil {
		t.Fatalf("same nullifier: %v", err)
	}
	if !duplicate || sameNullifier.ID != job.ID {
		t.Fatalf("same nullifier should return original job")
	}

	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.TotalJobs != 1 || stats.CurrentQueue != 1 {
		t.Fatalf("unexpected active stats: %+v", stats)
	}

	for _, status := range []JobStatus{StatusPreparing, StatusSubmitting, StatusSubmitted, StatusConfirmed} {
		if err := store.Update(context.Background(), job.ID, func(job *Job) {
			job.Status = status
		}); err != nil {
			t.Fatalf("transition job to %s: %v", status, err)
		}
	}
	stats, err = store.Stats(context.Background())
	if err != nil {
		t.Fatalf("stats after terminal: %v", err)
	}
	if stats.TotalJobs != 1 || stats.CurrentQueue != 0 {
		t.Fatalf("unexpected terminal stats: %+v", stats)
	}
}

func TestSQLiteQueueLimitPreservesIdempotentRetries(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "relayer.db"), "1", time.Hour, 1, 10)
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	defer store.Close()
	req := sampleWithdrawRequest()
	first, duplicate, err := store.CreateOrGetPending(context.Background(), req, "queue-request-a", "queue-nullifier-a")
	if err != nil || duplicate {
		t.Fatalf("create first queue job: duplicate=%v err=%v", duplicate, err)
	}
	retry, duplicate, err := store.CreateOrGetPending(context.Background(), req, "queue-request-a", "queue-nullifier-a")
	if err != nil || !duplicate || retry.ID != first.ID {
		t.Fatalf("idempotent retry should bypass queue limit: duplicate=%v err=%v", duplicate, err)
	}
	_, _, err = store.CreateOrGetPending(context.Background(), req, "queue-request-b", "queue-nullifier-b")
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("new request should hit queue limit, got %v", err)
	}
}

type flakyUpdateStore struct {
	JobStore
	failures int
	calls    int
}

func (s *flakyUpdateStore) Update(ctx context.Context, id string, fn func(*Job)) error {
	s.calls++
	if s.failures > 0 {
		s.failures--
		return errors.New("temporary sqlite write failure")
	}
	return s.JobStore.Update(ctx, id, fn)
}

func TestStoreStatsCache(t *testing.T) {
	store := newTestSQLiteStore(t)
	relayer := &Relayer{store: store}
	first, err := relayer.storeStats(context.Background())
	if err != nil || first.TotalJobs != 0 {
		t.Fatalf("initial stats=%+v err=%v", first, err)
	}
	if _, _, err := store.CreateOrGetPending(context.Background(), sampleWithdrawRequest(), "stats-cache-request", "stats-cache-nullifier"); err != nil {
		t.Fatalf("create job: %v", err)
	}
	cached, err := relayer.storeStats(context.Background())
	if err != nil || cached.TotalJobs != 0 {
		t.Fatalf("stats cache did not reuse recent result: %+v err=%v", cached, err)
	}
	relayer.statsMu.Lock()
	relayer.statsCachedAt = time.Time{}
	relayer.statsMu.Unlock()
	refreshed, err := relayer.storeStats(context.Background())
	if err != nil || refreshed.TotalJobs != 1 {
		t.Fatalf("stats cache refresh=%+v err=%v", refreshed, err)
	}
}

func TestCleanupDeletesTerminalJobsInBatches(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "cleanup.db"), "1", time.Nanosecond, 100, 2000)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()
	now := time.Now().Add(-time.Hour).UnixMilli()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatalf("begin insert: %v", err)
	}
	for i := 0; i < 1001; i++ {
		id := fmt.Sprintf("terminal-%04d", i)
		if _, err := tx.Exec(`INSERT INTO jobs(id, request_hash, nullifier_key, type, status, request_json, created_at, updated_at) VALUES (?, ?, ?, 'TORNADO_WITHDRAW', ?, '{}', ?, ?)`, id, "request-"+id, "nullifier-"+id, string(StatusConfirmed), now, now); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert terminal job: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit terminal jobs: %v", err)
	}
	if err := store.Cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	var remaining int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("remaining terminal jobs=%d err=%v, want 1", remaining, err)
	}
}

func newTestSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "relayer.db"), "1", time.Hour, 100, 1000)
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func markSQLiteJobStale(t *testing.T, store *SQLiteStore, id string, status JobStatus, updatedAt time.Time) {
	t.Helper()
	if _, err := store.db.ExecContext(
		context.Background(),
		`UPDATE jobs SET status = ?, updated_at = ? WHERE id = ?`,
		string(status),
		updatedAt.UTC().UnixMilli(),
		id,
	); err != nil {
		t.Fatalf("update stale sqlite job: %v", err)
	}
}

func TestSQLiteRejectsInMemoryDatabase(t *testing.T) {
	if _, err := NewSQLiteStore(":memory:", "1", time.Hour, 100, 1000); err == nil || !strings.Contains(err.Error(), "in-memory") {
		t.Fatalf("in-memory database should be rejected, got %v", err)
	}
	cfg := productionRuntimeConfig()
	cfg.SQLitePath = ":memory:"
	if err := validateRuntimeConfig(cfg); err == nil || !strings.Contains(err.Error(), "persistent database") {
		t.Fatalf("in-memory runtime configuration should be rejected, got %v", err)
	}
}

func TestSQLiteRejectsIllegalStatusTransition(t *testing.T) {
	store := newTestSQLiteStore(t)
	job, _, err := store.CreateOrGetPending(context.Background(), sampleWithdrawRequest(), "illegal-transition", "illegal-nullifier")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(context.Background(), job.ID, func(job *Job) { job.Status = StatusConfirmed }); err == nil || !strings.Contains(err.Error(), "illegal job status transition") {
		t.Fatalf("illegal transition error=%v", err)
	}
	got, ok, err := store.Get(context.Background(), job.ID)
	if err != nil || !ok || got.Status != StatusQueued {
		t.Fatalf("illegal transition changed job: job=%+v ok=%v err=%v", got, ok, err)
	}
}

func TestSQLiteProtectedJobLimit(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "relayer.db"), "1", time.Hour, 10, 1)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()
	request := sampleWithdrawRequest()
	job, _, err := store.CreateOrGetPending(context.Background(), request, "protected-a", "protected-nullifier-a")
	if err != nil {
		t.Fatalf("create protected job: %v", err)
	}
	for _, status := range []JobStatus{StatusPreparing, StatusSubmitting, StatusSubmitted} {
		if err := store.Update(context.Background(), job.ID, func(job *Job) { job.Status = status }); err != nil {
			t.Fatalf("transition job to %s: %v", status, err)
		}
	}
	if _, _, err := store.CreateOrGetPending(context.Background(), request, "protected-b", "protected-nullifier-b"); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("protected job limit should reject new job, got %v", err)
	}
}

func TestCriticalJobUpdateRetriesTransientStoreFailure(t *testing.T) {
	base := newTestSQLiteStore(t)
	job, _, err := base.CreateOrGetPending(context.Background(), sampleWithdrawRequest(), "retry-request", "retry-nullifier")
	if err != nil {
		t.Fatalf("create retry job: %v", err)
	}
	flaky := &flakyUpdateStore{JobStore: base, failures: 2}
	relayer := &Relayer{store: flaky}
	if err := relayer.updateJobCritical(job.ID, func(job *Job) { job.Status = StatusPreparing }); err != nil {
		t.Fatalf("critical update should recover: %v", err)
	}
	if flaky.calls != 3 {
		t.Fatalf("critical update calls=%d, want 3", flaky.calls)
	}
	got, ok, err := base.Get(context.Background(), job.ID)
	if err != nil || !ok || got.Status != StatusPreparing {
		t.Fatalf("critical update: job=%+v ok=%v err=%v", got, ok, err)
	}
}

func TestJobClaimDedup(t *testing.T) {
	relayer := &Relayer{}
	if !relayer.claimJob("job-1") {
		t.Fatalf("first claim should succeed")
	}
	if relayer.claimJob("job-1") {
		t.Fatalf("duplicate claim should fail")
	}
	relayer.releaseJob("job-1")
	if !relayer.claimJob("job-1") {
		t.Fatalf("claim should succeed after release")
	}
}

func TestSQLiteAdmissionDuplicatesAndRetryableResubmission(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	req := sampleWithdrawRequest()
	original, _, err := store.CreateOrGetPending(ctx, req, "admission-request", "admission-nullifier")
	if err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{"admission-request", "different-request-same-nullifier"} {
		got, duplicate, err := store.CreateOrGetPending(ctx, req, hash, "admission-nullifier")
		if err != nil || !duplicate || got.ID != original.ID {
			t.Fatalf("duplicate: job=%v dup=%v err=%v", got, duplicate, err)
		}
	}
	if err := store.FailAndRelease(ctx, original.ID, "retryable", true); err != nil {
		t.Fatal(err)
	}
	next, duplicate, err := store.CreateOrGetPending(ctx, req, "admission-request", "admission-nullifier")
	if err != nil || duplicate || next.ID == original.ID {
		t.Fatalf("retry after retryable failure: job=%v dup=%v err=%v", next, duplicate, err)
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.CurrentQueue != 1 {
		t.Fatalf("retry admission stats=%+v err=%v", stats, err)
	}
}
