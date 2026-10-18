package main

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	// sensitiveURLPattern matches http(s), ws(s) and ipc URLs up to the next
	// whitespace (RPC endpoints may embed API keys in path or query). It is
	// shared by log lines and client-visible failure reasons.
	sensitiveURLPattern = regexp.MustCompile(`(?i)(https?|wss?|ipc)://[^\s]+`)
	revertPattern       = regexp.MustCompile(`(?i)(execution reverted|reverted|invalid opcode)(:?\s*[^;\n]*)`)
)

// sensitiveURLReplacement keeps the URL scheme and drops everything after it.
const sensitiveURLReplacement = "$1://[redacted]"

const maxPublicReasonLength = 300

// publicSensitiveReason is the fixed client-visible text that replaces a
// public failure message referencing an endpoint URL. Pattern-based redaction
// stays log-only: on client output it cannot tell where a URL ends and leaves
// partial upstream text behind, so clients get this fixed text instead.
const publicSensitiveReason = "withdraw failed (upstream service error; details redacted)"

// publicReasonText returns the client-visible form of a public failure
// message: publicSensitiveReason when it references a URL (raw or already
// redacted by redactSensitive), otherwise the bounded message itself.
func publicReasonText(message string) string {
	if sensitiveURLPattern.MatchString(message) {
		return publicSensitiveReason
	}
	return redactSensitive(message)
}

// redactURLs replaces every URL in message with its redacted form.
func redactURLs(message string) string {
	return sensitiveURLPattern.ReplaceAllString(message, sensitiveURLReplacement)
}

// redactSensitive removes URLs (RPC endpoints may contain API keys) and bounds
// the length of any text that is stored or returned to clients. Truncation
// backs off to a rune boundary so the result stays valid UTF-8.
func redactSensitive(message string) string {
	message = redactURLs(message)
	if len(message) > maxPublicReasonLength {
		cut := maxPublicReasonLength
		for cut > 0 && !utf8.RuneStart(message[cut]) {
			cut--
		}
		message = message[:cut] + "..."
	}
	return message
}

// revertReason extracts only the revert part of a node error, e.g.
// "execution reverted: Invalid withdraw proof".
func revertReason(err error) string {
	if err == nil {
		return "reverted"
	}
	match := revertPattern.FindString(err.Error())
	if match == "" {
		return "reverted"
	}
	return redactSensitive(strings.TrimSpace(match))
}

// jobFailure classifies why a job failed.
//
//   - retryable: the same request (same proof) may succeed later, e.g. RPC
//     outage, gas spike, OZ relayer rejection/unavailability, failed or expired OZ transaction.
//     An identical resubmission creates a new job.
//   - permanent: the same request can never succeed, e.g. nullifier already
//     spent, recipient is a contract, fee below service fee, simulation
//     revert, on-chain revert (unless its root left the root history). An
//     identical resubmission returns the failed job; after such a revert any
//     new request for the nullifier is refused (ErrNullifierReverted).
//
// public controls whether the message may be shown to API clients.
type jobFailure struct {
	err       error
	retryable bool
	public    bool
	// deferrable: a market condition (gas price, fee vs gas cost, oracle
	// price) that often clears within minutes; preflight is retried for
	// preflightRetryWindow before the job fails.
	deferrable bool
}

func (f *jobFailure) Error() string { return f.err.Error() }
func (f *jobFailure) Unwrap() error { return f.err }

// classify wraps err as a jobFailure; a nil err stays nil.
func classify(err error, failure jobFailure) error {
	if err == nil {
		return nil
	}
	failure.err = err
	return &failure
}

func permanentFailure(err error) error {
	return classify(err, jobFailure{public: true})
}

func retryableFailure(err error) error {
	return classify(err, jobFailure{retryable: true, public: true})
}

// deferrableFailure is a retryable public failure caused by market
// conditions; see jobFailure.deferrable.
func deferrableFailure(err error) error {
	return classify(err, jobFailure{retryable: true, public: true, deferrable: true})
}

// retryableInternalFailure is transient but its text may contain internal
// details (RPC errors), so it is not exposed to clients.
func retryableInternalFailure(err error) error {
	return classify(err, jobFailure{retryable: true})
}

func failureDeferrable(err error) bool {
	var failure *jobFailure
	return errors.As(err, &failure) && failure.deferrable
}

// failureRetryable reports whether an identical request may be retried.
// Unclassified errors are internal/unexpected and treated as transient.
func failureRetryable(err error) bool {
	var failure *jobFailure
	if errors.As(err, &failure) {
		return failure.retryable
	}
	return true
}

// publicFailureReason is the failedReason stored for FAILED jobs.
func publicFailureReason(err error) string {
	reason := publicJobError(err)
	if failureRetryable(err) {
		return reason + "; retryable: the same request can be submitted again"
	}
	return reason + "; not retryable"
}

func isRevertError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "revert") || strings.Contains(message, "invalid opcode")
}
