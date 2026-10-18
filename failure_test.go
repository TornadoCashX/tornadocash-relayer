package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRedactSensitiveKeepsValidUTF8(t *testing.T) {
	message := strings.Repeat("a", maxPublicReasonLength-1) + "€€"
	got := redactSensitive(message)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated reason is not valid UTF-8: %q", got[len(got)-8:])
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated reason lacks ellipsis: %q", got[len(got)-8:])
	}
}

func TestRedactURLsCoversIPC(t *testing.T) {
	got := redactURLs("dial ipc:///var/run/geth.ipc failed; https://rpc.example/key123 down")
	if strings.Contains(got, "geth.ipc") || strings.Contains(got, "key123") {
		t.Fatalf("URL leaked: %q", got)
	}
}

func TestPublicReasonWithURLIsFixedText(t *testing.T) {
	raw := `node said Post "https://eth.example/v3/KEY": refused`
	for _, err := range []error{
		retryableFailure(errors.New(raw)),
		// Already redacted upstream (e.g. reconciler reasons) still counts.
		permanentFailure(errors.New("reverted on chain: " + redactSensitive(raw))),
	} {
		if got := publicJobError(err); got != publicSensitiveReason {
			t.Fatalf("publicJobError=%q, want fixed %q", got, publicSensitiveReason)
		}
	}
	if got := publicFailureReason(retryableFailure(errors.New(raw))); got != publicSensitiveReason+"; retryable: the same request can be submitted again" {
		t.Fatalf("publicFailureReason=%q", got)
	}
	// URL-free public text is kept verbatim.
	if got := publicJobError(permanentFailure(errors.New("withdraw nullifier hash is already spent"))); got != "withdraw nullifier hash is already spent" {
		t.Fatalf("plain reason changed: %q", got)
	}
	// Logs keep the pattern-based redaction, not the fixed text.
	if got := redactURLs(raw); got != `node said Post "https://[redacted] refused` {
		t.Fatalf("log redaction changed: %q", got)
	}
}

func TestPublicReasonsAreRedacted(t *testing.T) {
	for _, err := range []error{
		retryableFailure(errors.New(`node said Post "https://eth.example/v3/KEY": refused`)),
		permanentFailure(errors.New("see wss://ws.example/KEY for details")),
	} {
		reason := publicFailureReason(err)
		if strings.Contains(reason, "KEY") || !strings.HasPrefix(reason, publicSensitiveReason+"; ") {
			t.Fatalf("URL-bearing reason must be replaced by the fixed text: %q", reason)
		}
	}
	if long := redactSensitive(strings.Repeat("x", 1000)); len(long) > maxPublicReasonLength+3 {
		t.Fatalf("public reason not bounded: %d", len(long))
	}
}

// OZ free text (balances, internals) must not reach the public failedReason.
func TestOZMessagesAreNotPublic(t *testing.T) {
	h := newOZHarness(t)
	h.oz.submitCode, h.oz.submitError = http.StatusBadRequest, "balance 123 wei is too low"
	job, _ := submitRequest(t, h, requestWithNullifier("0c"))
	if job.Status != StatusFailed || strings.Contains(job.FailedReason, "balance") {
		t.Fatalf("OZ rejection text leaked: %+v", job)
	}
}

func TestLogsRedactRPCURLs(t *testing.T) {
	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousWriter)
	logErrorf("rpc failed: %v", errors.New("dial https://rpc.example/v3/secret-api-key: connection refused"))
	if strings.Contains(logs.String(), "secret-api-key") || !strings.Contains(logs.String(), "https://[redacted]") {
		t.Fatalf("RPC URL was not redacted: %s", logs.String())
	}
}

func TestFailJobRedactsStoredRequestAndSanitizesInternalError(t *testing.T) {
	store := newTestSQLiteStore(t)
	request := WithdrawRequest{
		Proof:    "0x" + strings.Repeat("11", 256),
		Args:     []string{"root", "nullifier", "recipient", "relayer", "fee", "refund"},
		Contract: "0x12D66f87A04A9E220743712cE6d9bB1B5616B8Fc",
	}
	job, _, err := store.CreateOrGetPending(context.Background(), request, "request-hash-redact", "nullifier-redact")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	relayer := &Relayer{store: store}
	relayer.failJobAndRelease(job.ID, errors.New("dial tcp 10.0.0.1:8545: connection refused /secret/path"))
	got, ok, err := store.Get(context.Background(), job.ID)
	if err != nil || !ok {
		t.Fatalf("get failed job: ok=%v err=%v", ok, err)
	}
	if got.Request.Proof != "" || len(got.Request.Args) != 0 || got.Request.Contract != "" {
		t.Fatalf("stored terminal job retained request: %#v", got.Request)
	}
	if !strings.HasPrefix(got.FailedReason, "withdraw failed (internal error)") || strings.Contains(got.FailedReason, "10.0.0.1") {
		t.Fatalf("internal error was not sanitized: %q", got.FailedReason)
	}
	if !got.Retryable || !strings.Contains(got.FailedReason, "retryable") {
		t.Fatalf("unclassified internal error must be stored as retryable: retryable=%v reason=%q", got.Retryable, got.FailedReason)
	}
}

func TestFailedJobRetryDependsOnFailureClass(t *testing.T) {
	store := newTestSQLiteStore(t)
	relayer := &Relayer{store: store}
	ctx := context.Background()
	request := sampleWithdrawRequest()

	// Retryable failure: the identical request creates a new job.
	hash := requestHash("1", request)
	first, _, err := store.CreateOrGetPending(ctx, request, hash, "1:pool:retryable")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	relayer.failJobAndRelease(first.ID, retryableFailure(errors.New("provided fee gas budget is below current base fee")))
	second, duplicate, err := store.CreateOrGetPending(ctx, request, hash, "1:pool:retryable")
	if err != nil || duplicate || second.ID == first.ID {
		t.Fatalf("retryable failure must allow a new job: duplicate=%v sameID=%v err=%v", duplicate, second != nil && second.ID == first.ID, err)
	}
	old, _, _ := store.Get(ctx, first.ID)
	if old.Status != StatusFailed || !strings.Contains(old.FailedReason, "below current base fee") {
		t.Fatalf("old failed job must be kept with its reason: %+v", old)
	}
	// While the new job is active, the identical request is idempotent again.
	if again, duplicate, err := store.CreateOrGetPending(ctx, request, hash, "1:pool:retryable"); err != nil || !duplicate || again.ID != second.ID {
		t.Fatalf("active retry must be idempotent: duplicate=%v err=%v", duplicate, err)
	}

	// Permanent failure: the identical request returns the failed job.
	permanentRequest := sampleWithdrawRequest()
	permanentRequest.Args[1] = "0x" + strings.Repeat("44", 32)
	permanentHash := requestHash("1", permanentRequest)
	failed, _, err := store.CreateOrGetPending(ctx, permanentRequest, permanentHash, "1:pool:permanent")
	if err != nil {
		t.Fatalf("create permanent: %v", err)
	}
	relayer.failJobAndRelease(failed.ID, permanentFailure(errors.New("withdraw nullifier hash is already spent")))
	got, duplicate, err := store.CreateOrGetPending(ctx, permanentRequest, permanentHash, "1:pool:permanent")
	if err != nil || !duplicate || got.ID != failed.ID || got.Status != StatusFailed {
		t.Fatalf("permanent failure must return the failed job: duplicate=%v status=%s err=%v", duplicate, got.Status, err)
	}
	if !strings.Contains(got.FailedReason, "already spent") || !strings.Contains(got.FailedReason, "not retryable") {
		t.Fatalf("permanent failure reason must be explicit: %q", got.FailedReason)
	}
}

func TestFailureClassification(t *testing.T) {
	for _, c := range []struct {
		err       error
		retryable bool
	}{
		{classifyEstimateError(errors.New("execution reverted: Invalid withdraw proof")), false},
		{classifyEstimateError(errors.New("Post \"https://rpc\": context deadline exceeded")), true},
		// The eth_call diagnosis wording must not make network errors look like reverts.
		{classifyEstimateError(fmt.Errorf("estimateGas failed: %w; eth_call: %v", errors.New("timeout"), errors.New("timeout"))), true},
		{classifyEstimateError(errors.New("insufficient funds for gas")), true},
		{fmt.Errorf("wrapped: %w", permanentFailure(errors.New("x"))), false},
		{errors.New("unclassified"), true},
	} {
		if got := failureRetryable(c.err); got != c.retryable {
			t.Fatalf("%v: retryable=%v want %v", c.err, got, c.retryable)
		}
	}
}
