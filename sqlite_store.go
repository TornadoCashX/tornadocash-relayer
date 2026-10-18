package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/gofrs/flock"
	_ "modernc.org/sqlite"
)

// ErrStoreLocked is returned when another relayer process holds the
// database lock file.
var ErrStoreLocked = errors.New("SQLite database is already locked by another relayer process")

const (
	sqliteSchemaVersion = 3
	// sqliteBusyTimeoutMillis is how long SQLite waits on a locked database.
	sqliteBusyTimeoutMillis = 5000
	// unknownSubmitsLimit caps the stalled submissions reported by Stats.
	unknownSubmitsLimit = 100
	// cleanupBatchSize caps the expired terminal jobs deleted per Cleanup run.
	cleanupBatchSize = 1000
	// cleanupMaintenanceEvery runs PRAGMA optimize and a truncating WAL checkpoint once
	// every this many Cleanup runs.
	cleanupMaintenanceEvery = 100
)

// SQLiteStore is the durable JobStore. It uses a single connection and an
// exclusive lock file so only one relayer process owns the database.
type SQLiteStore struct {
	db               *sql.DB
	idempotencyTTL   time.Duration
	maxActiveJobs    uint64
	maxProtectedJobs uint64
	databaseLock     *flock.Flock
	cleanupRuns      uint64
}

// Close closes the database and releases the lock file.
func (s *SQLiteStore) Close() error {
	closeErr := s.db.Close()
	if s.databaseLock != nil {
		if err := s.databaseLock.Unlock(); closeErr == nil && err != nil {
			closeErr = err
		}
	}
	return closeErr
}

// NewSQLiteStore opens (creating if needed) the private database at path,
// takes its exclusive lock, and verifies or initializes the schema bound to
// chainID.
func NewSQLiteStore(path, chainID string, idempotencyTTL time.Duration, maxActiveJobs, maxProtectedJobs uint64) (*SQLiteStore, error) {
	if chainID == "" {
		return nil, errors.New("SQLite chain ID is required")
	}
	if path == ":memory:" {
		return nil, errors.New("SQLite in-memory databases are not supported")
	}
	path, err := ensurePrivateDBFile(path)
	if err != nil {
		return nil, err
	}
	databaseLock, err := acquireDBLock(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		_ = databaseLock.Unlock()
		return nil, err
	}
	// One connection serializes all access; see ListActive.
	db.SetMaxOpenConns(1)
	store := &SQLiteStore{
		db: db, idempotencyTTL: idempotencyTTL,
		maxActiveJobs: maxActiveJobs, maxProtectedJobs: maxProtectedJobs,
		databaseLock: databaseLock,
	}
	if err := store.init(context.Background(), chainID); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.scrubOnce(context.Background()); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("scrub SQLite free space: %w", err)
	}
	for _, databaseFile := range []string{path, path + "-wal", path + "-shm"} {
		if err := secureFileMode(databaseFile); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("secure SQLite file: %w", err)
		}
	}
	return store, nil
}

// ensurePrivateDBFile requires an existing database file to be private or
// creates a new one with ownerOnlyFileMode, then returns its canonical absolute
// path.
func ensurePrivateDBFile(path string) (string, error) {
	if _, err := os.Stat(path); err == nil {
		if err := requirePrivateFile(path, "SQLITE_PATH"); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	} else {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, ownerOnlyFileMode)
		if createErr != nil {
			return "", createErr
		}
		if closeErr := file.Close(); closeErr != nil {
			return "", closeErr
		}
	}
	canonicalPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve SQLITE_PATH: %w", err)
	}
	path, err = filepath.Abs(canonicalPath)
	if err != nil {
		return "", fmt.Errorf("resolve SQLITE_PATH: %w", err)
	}
	return path, nil
}

// acquireDBLock takes the exclusive "<path>.lock" file lock and makes the
// lock file private. It returns ErrStoreLocked when another process holds it.
func acquireDBLock(path string) (*flock.Flock, error) {
	databaseLock := flock.New(path + ".lock")
	locked, err := databaseLock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire SQLite database lock: %w", err)
	}
	if !locked {
		return nil, ErrStoreLocked
	}
	if err := secureFileMode(path + ".lock"); err != nil {
		_ = databaseLock.Unlock()
		return nil, fmt.Errorf("secure SQLite database lock: %w", err)
	}
	return databaseLock, nil
}

func sqliteDSN(path string) string {
	dsn := url.URL{Scheme: "file", Path: path}
	query := url.Values{}
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(FULL)")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", sqliteBusyTimeoutMillis))
	query.Add("_pragma", "foreign_keys(ON)")
	// Deleted rows, overwritten cells (request_json cleared on a terminal
	// status) and freed pages are zeroed instead of left in the file, so
	// proofs and recipients do not survive in free space.
	query.Add("_pragma", "secure_delete(ON)")
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

// sqliteSchemaStatements create the v3 schema in an empty database.
var sqliteSchemaStatements = []string{
	`CREATE TABLE store_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE jobs (
				id TEXT PRIMARY KEY,
				request_hash TEXT NOT NULL UNIQUE,
				nullifier_key TEXT NOT NULL,
				type TEXT NOT NULL,
				status TEXT NOT NULL CHECK(status IN ('QUEUED','PREPARING','SUBMITTING','SUBMITTED','MINED','CONFIRMED','FAILED')),
				request_json TEXT NOT NULL,
				oz_tx_id TEXT,
				tx_hash TEXT,
				tx_hashes_json TEXT NOT NULL DEFAULT '[]',
				failed_reason TEXT,
				retryable INTEGER NOT NULL DEFAULT 0,
				submit_started_at INTEGER NOT NULL DEFAULT 0,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
	`CREATE TABLE pending_nullifiers (
				nullifier_key TEXT PRIMARY KEY,
				job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE
			)`,
	`CREATE UNIQUE INDEX idx_jobs_oz_tx_id ON jobs(oz_tx_id) WHERE oz_tx_id IS NOT NULL`,
	`CREATE INDEX idx_jobs_status_updated_at ON jobs(status, updated_at)`,
	`CREATE INDEX idx_pending_nullifiers_job ON pending_nullifiers(job_id)`,
}

// Schema v3 is the OpenZeppelin-Relayer schema. It deliberately shares no
// tables with the v2 self-signing schema: v2 databases are refused so the old
// in-flight jobs are drained by the old binary instead of being reinterpreted.
func (s *SQLiteStore) init(ctx context.Context, chainID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackTx(tx)
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	switch version {
	case 0:
		if err := createSchema(ctx, tx, chainID); err != nil {
			return err
		}
	case sqliteSchemaVersion:
		if err := checkChainBinding(ctx, tx, chainID); err != nil {
			return err
		}
		// Added without a version bump: older v3 binaries ignore the table.
		if _, err := tx.ExecContext(ctx, createRevertedNullifiers); err != nil {
			return err
		}
	case 1, 2:
		return errors.New("SQLITE_PATH is a database of the previous self-signing relayer; drain it with the old binary and point SQLITE_PATH to a new file")
	default:
		return fmt.Errorf("SQLite schema version %d is unsupported; version %d is required", version, sqliteSchemaVersion)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, sqliteSchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// createSchema creates the v3 schema and records the chain binding. It refuses
// a version-0 database that already contains user tables.
func createSchema(ctx context.Context, tx *sql.Tx, chainID string) error {
	var userTables int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&userTables); err != nil {
		return err
	}
	if userTables != 0 {
		return errors.New("SQLITE_PATH contains an unknown schema; use a new database file")
	}
	for _, statement := range sqliteSchemaStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, createRevertedNullifiers); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO store_meta(key, value) VALUES ('chain_id', ?)`, chainID); err != nil {
		return err
	}
	return nil
}

// checkChainBinding verifies the database was created for chainID.
func checkChainBinding(ctx context.Context, tx *sql.Tx, chainID string) error {
	var stored string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM store_meta WHERE key='chain_id'`).Scan(&stored); err != nil {
		return fmt.Errorf("read SQLite chain binding: %w", err)
	}
	if stored != chainID {
		return fmt.Errorf("SQLite database belongs to chain %s, not NET_ID=%s", stored, chainID)
	}
	return nil
}

// CreateOrGetPending records request and nullifier ownership in one
// transaction. An identical request returns the existing job unless it failed
// retryably; a different request for a nullifier that is still owned returns
// the owning job.
func (s *SQLiteStore) CreateOrGetPending(ctx context.Context, req WithdrawRequest, reqHash, nullifierKey string) (*Job, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer rollbackTx(tx)
	if job, ok, err := jobByQuery(ctx, tx, `SELECT id FROM jobs WHERE request_hash=?`, reqHash); err != nil {
		return nil, false, err
	} else if ok {
		if job.Status != StatusFailed || !job.Retryable {
			return job, true, nil
		}
		// Keep the failed job for audit, free its request hash for the retry.
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET request_hash=request_hash || ':retried:' || id WHERE id=?`, job.ID); err != nil {
			return nil, false, err
		}
	}
	if job, ok, err := jobByQuery(ctx, tx, `SELECT job_id FROM pending_nullifiers WHERE nullifier_key=?`, nullifierKey); err != nil || ok {
		return job, ok, err
	}
	var reverted int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM reverted_nullifiers WHERE nullifier_key=?`, nullifierKey).Scan(&reverted); err != nil {
		return nil, false, err
	}
	if reverted > 0 {
		return nil, false, ErrNullifierReverted
	}
	if err := s.checkCapacity(ctx, tx); err != nil {
		return nil, false, err
	}
	requestJSON, err := json.Marshal(req)
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	job := &Job{ID: newID(), Type: "TORNADO_WITHDRAW", Status: StatusQueued, Request: req, RequestHash: reqHash, NullifierKey: nullifierKey, UpdatedAt: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id, request_hash, nullifier_key, type, status, request_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, job.ID, reqHash, nullifierKey, job.Type, string(job.Status), string(requestJSON), now.UnixMilli(), now.UnixMilli()); err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pending_nullifiers(nullifier_key, job_id) VALUES (?, ?)`, nullifierKey, job.ID); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return cloneJob(job), false, nil
}

func (s *SQLiteStore) checkCapacity(ctx context.Context, tx *sql.Tx) error {
	if s.maxActiveJobs > 0 {
		statuses := activeStatuses()
		var active uint64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status IN (`+statusPlaceholders(len(statuses))+`)`, statusValues(statuses)...).Scan(&active); err != nil {
			return err
		}
		if active >= s.maxActiveJobs {
			return ErrQueueFull
		}
	}
	if s.maxProtectedJobs > 0 {
		var protected uint64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_nullifiers`).Scan(&protected); err != nil {
			return err
		}
		if protected >= s.maxProtectedJobs {
			return ErrQueueFull
		}
	}
	return nil
}

// Get loads a job by id.
func (s *SQLiteStore) Get(ctx context.Context, id string) (*Job, bool, error) {
	return loadJob(ctx, s.db, id)
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func jobByQuery(ctx context.Context, tx *sql.Tx, query, arg string) (*Job, bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, query, arg).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return loadJob(ctx, tx, id)
}

func loadJob(ctx context.Context, query rowQuerier, id string) (*Job, bool, error) {
	var job Job
	var requestJSON, hashesJSON string
	var ozTxID, txHash, failedReason sql.NullString
	var createdAt, updatedAt, submitStartedAt int64
	err := query.QueryRowContext(ctx, `SELECT id, request_hash, nullifier_key, type, status, request_json, oz_tx_id, tx_hash,
		tx_hashes_json, failed_reason, retryable, submit_started_at, created_at, updated_at FROM jobs WHERE id=?`, id).Scan(
		&job.ID, &job.RequestHash, &job.NullifierKey, &job.Type, &job.Status, &requestJSON, &ozTxID, &txHash,
		&hashesJSON, &failedReason, &job.Retryable, &submitStartedAt, &createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal([]byte(requestJSON), &job.Request); err != nil {
		return nil, false, fmt.Errorf("decode job request: %w", err)
	}
	if err := json.Unmarshal([]byte(hashesJSON), &job.TxHashes); err != nil {
		return nil, false, fmt.Errorf("decode job hashes: %w", err)
	}
	job.OZTxID, job.TxHash, job.FailedReason = ozTxID.String, txHash.String, failedReason.String
	job.CreatedAt = time.UnixMilli(createdAt).UTC()
	job.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	if submitStartedAt > 0 {
		job.SubmitStartedAt = time.UnixMilli(submitStartedAt).UTC()
	}
	return &job, true, nil
}

// Update applies fn atomically. Identity fields are immutable; a job that
// reaches a terminal status releases its pending nullifier in the same
// transaction, so ownership can never outlive or precede the final state.
// Deterministic failures (missing job, rejected change) wrap errStorePermanent.
func (s *SQLiteStore) Update(ctx context.Context, id string, fn func(*Job)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackTx(tx)
	job, ok, err := loadJob(ctx, tx, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: job %s not found", errStorePermanent, id)
	}
	original := cloneJob(job)
	fn(job)
	if err := validateJobUpdate(original, job); err != nil {
		return fmt.Errorf("%w: %v", errStorePermanent, err)
	}
	// An unchanged terminal job is not rewritten, so a no-op such as
	// FailAndRelease on a finished job does not bump updated_at and postpone
	// its TTL cleanup. Non-terminal jobs are always rewritten: Job.UpdatedAt
	// serves as a business fallback timestamp (submitStarted in reconciler.go)
	// and must keep advancing on every update.
	if isTerminalStatus(original.Status) && jobsEqual(original, job) {
		return nil
	}
	job.UpdatedAt = time.Now().UTC()
	if err := writeJob(ctx, tx, job); err != nil {
		return err
	}
	return tx.Commit()
}

// jobsEqual reports whether two jobs are identical, treating nil and empty
// TxHashes alike (cloneJob turns an empty slice into nil).
func jobsEqual(a, b *Job) bool {
	left, right := *a, *b
	left.TxHashes = nonNilStrings(left.TxHashes)
	right.TxHashes = nonNilStrings(right.TxHashes)
	return reflect.DeepEqual(left, right)
}

// validateJobUpdate rejects changes to immutable fields and illegal status
// transitions between original and the job modified by an Update callback.
func validateJobUpdate(original, job *Job) error {
	if job.ID != original.ID || job.RequestHash != original.RequestHash || job.NullifierKey != original.NullifierKey {
		return errors.New("job identity fields are immutable")
	}
	if isTerminalStatus(original.Status) && job.Status != original.Status {
		return errors.New("terminal job status is immutable")
	}
	if job.Status != original.Status {
		probe := &Job{Status: original.Status}
		if !transitionJob(probe, job.Status) {
			return fmt.Errorf("illegal job status transition %s -> %s", original.Status, job.Status)
		}
	}
	if original.OZTxID != "" && job.OZTxID != original.OZTxID {
		return errors.New("OpenZeppelin transaction id is immutable once recorded")
	}
	return nil
}

// writeJob persists the mutable job fields. A terminal job also releases its
// pending nullifier and drops the stored request.
func writeJob(ctx context.Context, tx *sql.Tx, job *Job) error {
	hashesJSON, err := json.Marshal(nonNilStrings(job.TxHashes))
	if err != nil {
		return err
	}
	var submitStartedAt int64
	if !job.SubmitStartedAt.IsZero() {
		submitStartedAt = job.SubmitStartedAt.UnixMilli()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status=?, oz_tx_id=?, tx_hash=?, tx_hashes_json=?, failed_reason=?,
		retryable=?, submit_started_at=?, updated_at=? WHERE id=?`,
		string(job.Status), nullString(job.OZTxID), nullString(job.TxHash), string(hashesJSON), nullString(job.FailedReason),
		job.Retryable, submitStartedAt, job.UpdatedAt.UnixMilli(), job.ID); err != nil {
		return err
	}
	if isTerminalStatus(job.Status) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_nullifiers WHERE job_id=?`, job.ID); err != nil {
			return err
		}
		// A finished job no longer needs the proof; keep only the audit trail.
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET request_json='{}' WHERE id=?`, job.ID); err != nil {
			return err
		}
	}
	return nil
}

// FailAndRelease marks a non-terminal job FAILED and releases its nullifier.
// An already terminal job is left unchanged.
func (s *SQLiteStore) FailAndRelease(ctx context.Context, id, reason string, retryable bool) error {
	return s.Update(ctx, id, func(job *Job) {
		if transitionJob(job, StatusFailed) {
			job.FailedReason = reason
			job.Retryable = retryable
		}
	})
}

const createRevertedNullifiers = `CREATE TABLE IF NOT EXISTS reverted_nullifiers (
	nullifier_key TEXT PRIMARY KEY,
	reverted_at INTEGER NOT NULL
)`

// MarkReverted permanently refuses new requests for a nullifier whose
// withdrawal reverted on chain. Rows are never deleted (not even by Cleanup).
func (s *SQLiteStore) MarkReverted(ctx context.Context, nullifierKey string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO reverted_nullifiers(nullifier_key, reverted_at) VALUES (?, ?)
		ON CONFLICT(nullifier_key) DO NOTHING`, nullifierKey, time.Now().UnixMilli())
	return err
}

// ListActive returns all non-terminal jobs in creation order.
func (s *SQLiteStore) ListActive(ctx context.Context) ([]*Job, error) {
	statuses := activeStatuses()
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE status IN (`+statusPlaceholders(len(statuses))+`) ORDER BY created_at`, statusValues(statuses)...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	// The pool has a single connection (SetMaxOpenConns(1)) that the open
	// result set holds; rows must be closed before loadJob can run a query,
	// otherwise it would wait for a connection forever.
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	jobs := make([]*Job, 0, len(ids))
	for _, id := range ids {
		job, ok, err := loadJob(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		if ok {
			jobs = append(jobs, job)
		}
	}
	return jobs, nil
}

// Stats reports job counts and up to unknownSubmitsLimit submissions whose
// outcome has been unknown for longer than unknownSubmitWarnAfter.
func (s *SQLiteStore) Stats(ctx context.Context) (StoreStats, error) {
	var stats StoreStats
	statuses := activeStatuses()
	query := `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status IN (` + statusPlaceholders(len(statuses)) + `) THEN 1 ELSE 0 END), 0),
		(SELECT COUNT(*) FROM pending_nullifiers) FROM jobs`
	if err := s.db.QueryRowContext(ctx, query, statusValues(statuses)...).Scan(&stats.TotalJobs, &stats.CurrentQueue, &stats.ProtectedJobs); err != nil {
		return StoreStats{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, submit_started_at FROM jobs
		WHERE status=? AND oz_tx_id IS NULL AND submit_started_at>0 AND submit_started_at<=?
		ORDER BY submit_started_at LIMIT ?`,
		string(StatusSubmitting), time.Now().Add(-unknownSubmitWarnAfter).UnixMilli(), unknownSubmitsLimit)
	if err != nil {
		return StoreStats{}, err
	}
	defer rows.Close()
	stats.UnknownSubmits = []unknownSubmit{}
	for rows.Next() {
		var item unknownSubmit
		var startedAt int64
		if err := rows.Scan(&item.ID, &startedAt); err != nil {
			return StoreStats{}, err
		}
		item.SubmittedAt = time.UnixMilli(startedAt).UTC()
		stats.UnknownSubmits = append(stats.UnknownSubmits, item)
	}
	if err := rows.Err(); err != nil {
		return StoreStats{}, err
	}
	return stats, nil
}

func statusPlaceholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func statusValues(statuses []JobStatus) []any {
	values := make([]any, len(statuses))
	for i, status := range statuses {
		values[i] = string(status)
	}
	return values
}

// Cleanup deletes up to cleanupBatchSize terminal jobs older than the
// idempotency TTL and periodically runs SQLite maintenance.
func (s *SQLiteStore) Cleanup(ctx context.Context) error {
	if s.idempotencyTTL > 0 {
		cutoff := time.Now().UTC().Add(-s.idempotencyTTL).UnixMilli()
		if _, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id IN (
			SELECT id FROM jobs WHERE status IN (?, ?) AND updated_at<=?
			ORDER BY updated_at LIMIT ?
		)`, string(StatusConfirmed), string(StatusFailed), cutoff, cleanupBatchSize); err != nil {
			return err
		}
	}
	s.cleanupRuns++
	if s.cleanupRuns%cleanupMaintenanceEvery == 0 {
		if _, err := s.db.ExecContext(ctx, `PRAGMA optimize`); err != nil {
			return err
		}
		if err := s.truncateWAL(ctx); err != nil {
			return err
		}
	}
	return nil
}

// truncateWAL checkpoints the WAL into the database and truncates the WAL
// file to zero bytes. Old WAL frames still hold full request rows (proof,
// recipient) after the row was cleared; a PASSIVE checkpoint would keep them
// in the file until overwritten. The store uses one connection, so no reader
// can hold the WAL open and TRUNCATE completes.
func (s *SQLiteStore) truncateWAL(ctx context.Context) error {
	var busy, logFrames, checkpointed int
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		logWarnf("SQLite WAL truncate incomplete: busy=%d log_frames=%d checkpointed=%d", busy, logFrames, checkpointed)
	}
	return nil
}

// scrubMetaKey marks a database whose free space was rebuilt once with
// secure_delete enabled.
const scrubMetaKey = "secure_delete_scrubbed"

// scrubOnce removes content left in free space by binaries that ran without
// secure_delete: VACUUM rebuilds the file from live rows only, then the WAL is
// truncated. It runs once per database (marked in store_meta).
func (s *SQLiteStore) scrubOnce(ctx context.Context) error {
	var marked int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM store_meta WHERE key=?`, scrubMetaKey).Scan(&marked); err != nil {
		return err
	}
	if marked > 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("SQLite VACUUM: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO store_meta(key, value) VALUES (?, '1')`, scrubMetaKey); err != nil {
		return err
	}
	return s.truncateWAL(ctx)
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func rollbackTx(tx *sql.Tx) { _ = tx.Rollback() }
