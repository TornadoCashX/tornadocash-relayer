package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scrubMarker is a proof whose hex text is easy to find in raw file bytes.
// Files are searched for scrubFragment: freed cells are partly overwritten
// (freeblock header, the new smaller cell), so the full proof is rarely
// contiguous while most of it still is.
var (
	scrubMarker   = strings.Repeat("5ec7e7", 85) + "5e"
	scrubFragment = strings.Repeat("5ec7e7", 4)
)

func scrubRequest() WithdrawRequest {
	req := sampleWithdrawRequest()
	req.Proof = "0x" + scrubMarker
	return req
}

// fileContains reports whether path (missing files count as empty) holds marker.
func fileContains(t *testing.T, path, marker string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return bytes.Contains(data, []byte(marker))
}

func storeFilesContain(t *testing.T, path, marker string) []string {
	t.Helper()
	var found []string
	for _, file := range []string{path, path + "-wal"} {
		if fileContains(t, file, marker) {
			found = append(found, filepath.Base(file))
		}
	}
	return found
}

func TestSQLiteSecureDeleteEnabled(t *testing.T) {
	store := newTestSQLiteStore(t)
	var mode int
	if err := store.db.QueryRow(`PRAGMA secure_delete`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != 1 {
		t.Fatalf("secure_delete = %d, want 1", mode)
	}
}

// A terminal job clears request_json; after the next maintenance run neither
// the database file nor the WAL may still contain the proof.
func TestSQLiteTerminalJobLeavesNoProofOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relayer.db")
	store, err := NewSQLiteStore(path, "1", time.Hour, 100, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	job, _, err := store.CreateOrGetPending(ctx, scrubRequest(), "request-scrub", "nullifier-scrub")
	if err != nil {
		t.Fatal(err)
	}
	if found := storeFilesContain(t, path, scrubFragment); len(found) == 0 {
		t.Fatal("precondition: active job proof should be on disk")
	}
	if err := store.FailAndRelease(ctx, job.ID, "test", true); err != nil {
		t.Fatal(err)
	}
	store.cleanupRuns = cleanupMaintenanceEvery - 1
	if err := store.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if found := storeFilesContain(t, path, scrubFragment); len(found) != 0 {
		t.Fatalf("proof of a terminal job still on disk in %v", found)
	}
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() != 0 {
		t.Fatalf("WAL not truncated: %d bytes", info.Size())
	}
}

// A database written without secure_delete keeps cleared content in free
// space; opening it once with this binary must remove it.
func TestSQLiteScrubOnceRemovesLegacyResidue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relayer.db")
	store, err := NewSQLiteStore(path, "1", time.Hour, 100, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate an older binary: no secure_delete, row written then cleared.
	legacy, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=secure_delete(OFF)")
	if err != nil {
		t.Fatal(err)
	}
	legacy.SetMaxOpenConns(1)
	now := time.Now().UnixMilli()
	steps := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM store_meta WHERE key=?`, []any{scrubMetaKey}},
		{`INSERT INTO jobs(id, request_hash, nullifier_key, type, status, request_json, created_at, updated_at) VALUES ('legacy', 'h', 'n', 'TORNADO_WITHDRAW', 'FAILED', ?, ?, ?)`, []any{`{"proof":"0x` + scrubMarker + `"}`, now, now}},
		{`UPDATE jobs SET request_json='{}' WHERE id='legacy'`, nil},
		{`PRAGMA wal_checkpoint(TRUNCATE)`, nil},
	}
	for _, step := range steps {
		if _, err := legacy.Exec(step.query, step.args...); err != nil {
			t.Fatalf("%s: %v", step.query, err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if !fileContains(t, path, scrubFragment) {
		t.Fatal("precondition: legacy residue should be in the database file")
	}

	store, err = NewSQLiteStore(path, "1", time.Hour, 100, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if found := storeFilesContain(t, path, scrubFragment); len(found) != 0 {
		t.Fatalf("legacy residue survived the scrub in %v", found)
	}
	var marked int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM store_meta WHERE key=?`, scrubMetaKey).Scan(&marked); err != nil || marked != 1 {
		t.Fatalf("scrub marker = %d, err=%v", marked, err)
	}
	if job, ok, err := store.Get(context.Background(), "legacy"); err != nil || !ok || job.Status != StatusFailed {
		t.Fatalf("live rows must survive VACUUM: ok=%v err=%v", ok, err)
	}
}
