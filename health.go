package main

import (
	"context"
	"errors"
	"slices"
	"time"
)

const (
	// readinessTimeout bounds one full readiness check.
	readinessTimeout = 5 * time.Second
	// statusStalePeriods and minStatusStaleAfter define statusStaleAfter.
	statusStalePeriods  = 3
	minStatusStaleAfter = time.Minute
)

// readinessErrors runs every readiness check and returns the failures in a
// stable order: storage, RPC, OpenZeppelin Relayer, reconciler heartbeat.
func (r *Relayer) readinessErrors(parent context.Context, includeStore bool) []string {
	ctx, cancel := context.WithTimeout(parent, readinessTimeout)
	defer cancel()

	errorsLog := make([]string, 0)
	if includeStore {
		if _, err := r.storeStats(ctx); err != nil {
			logErrorf("readiness storage check failed: %v", err)
			errorsLog = append(errorsLog, "storage unavailable")
		}
	}
	errorsLog = r.rpcReadinessErrors(ctx, errorsLog)
	if r.startupPending.Load() {
		errorsLog = append(errorsLog, "openzeppelin relayer not verified")
	} else {
		errorsLog = r.ozReadinessErrors(ctx, errorsLog)
	}
	if !r.startupPending.Load() && heartbeatStale(r.reconcilerHeartbeat.Load(), r.statusStaleAfter()) {
		errorsLog = append(errorsLog, "transaction reconciler stopped")
	}
	return errorsLog
}

// readinessRPCUnavailable is the readiness error for "no RPC endpoint is
// healthy". admissionRefusal matches on it to close admission.
const readinessRPCUnavailable = "rpc unavailable"

// rpcReadinessErrors probes every RPC endpoint and appends its failures.
func (r *Relayer) rpcReadinessErrors(ctx context.Context, errorsLog []string) []string {
	// One healthy endpoint is enough to serve; a wrong chain id on any
	// endpoint is a configuration error and always reported.
	healthy := 0
	for _, result := range r.chain.probe(ctx, r.cfg.ChainID) {
		switch {
		case result.err == nil:
			healthy++
		case errors.Is(result.err, errRPCChainMismatch):
			logErrorf("readiness %s: %v", result.name, result.err)
			errorsLog = appendUnique(errorsLog, "rpc chain id mismatch")
		default:
			logErrorf("readiness %s check failed: %v", result.name, redactSensitive(result.err.Error()))
		}
	}
	if healthy == 0 {
		errorsLog = appendUnique(errorsLog, readinessRPCUnavailable)
	}
	return errorsLog
}

// ozReadinessErrors appends OpenZeppelin Relayer verification, readiness and
// cached status failures.
func (r *Relayer) ozReadinessErrors(ctx context.Context, errorsLog []string) []string {
	r.ozMu.RLock()
	verifyErr := r.ozVerifyErr
	r.ozMu.RUnlock()
	if verifyErr != nil {
		errorsLog = append(errorsLog, "openzeppelin relayer verification failed")
	}
	if err := r.oz.Ready(ctx); err != nil {
		logErrorf("readiness OpenZeppelin Relayer check failed: %v", err)
		errorsLog = append(errorsLog, "openzeppelin relayer not ready")
	}
	status := r.cachedOZStatus()
	switch {
	case status == nil:
		errorsLog = append(errorsLog, "openzeppelin relayer status unavailable")
	case status.Paused:
		errorsLog = append(errorsLog, "openzeppelin relayer paused")
	case status.SystemDisabled:
		errorsLog = append(errorsLog, "openzeppelin relayer disabled (check balance and RPC)")
	}
	return errorsLog
}

// priceErrors lists missing or stale oracle prices per currency. They only
// affect withdrawals from ERC20 pools of that currency (preflight refuses them
// as retryable), so /status reports them in errorsLog without marking the
// whole relayer unhealthy; /ready still fails on them for operator alerts.
func (r *Relayer) priceErrors() []string {
	errorsLog := make([]string, 0)
	if !r.shouldUseOraclePrices() {
		return errorsLog
	}
	for _, currency := range orderedKeys(r.cfg.OraclePriceTokens, tokenOrder) {
		if _, ok := r.ethPrice(currency); !ok {
			errorsLog = append(errorsLog, "price oracle unavailable: "+currency)
		}
	}
	if heartbeatStale(r.oracleHeartbeat.Load(), r.priceStaleAfter()) {
		errorsLog = append(errorsLog, "price oracle stale")
	}
	return errorsLog
}

// statusStaleAfter is how old the reconciler heartbeat, the OZ status and the
// cached readiness may be before they are no longer trusted.
func (r *Relayer) statusStaleAfter() time.Duration {
	return max(statusStalePeriods*r.cfg.PollPeriod, minStatusStaleAfter)
}

func (r *Relayer) refreshReadinessCache(ctx context.Context) {
	r.setReadinessErrors(r.readinessErrors(ctx, false))
}

func (r *Relayer) setReadinessErrors(errorsLog []string) {
	r.readinessMu.Lock()
	r.cachedReadyErrors = append([]string(nil), errorsLog...)
	r.readinessMu.Unlock()
	r.readinessCachedAt.Store(time.Now().Unix())
}

// cachedReadinessErrors is the last readiness result, plus an error when that
// result is too old to describe the relayer now.
func (r *Relayer) cachedReadinessErrors() []string {
	r.readinessMu.RLock()
	errorsLog := append([]string(nil), r.cachedReadyErrors...)
	r.readinessMu.RUnlock()
	// Until OZ is verified the cache may still be empty (it is first written
	// after the startup price refresh); /status must not report healthy then.
	if r.startupPending.Load() {
		errorsLog = appendUnique(errorsLog, "openzeppelin relayer not verified")
	}
	if heartbeatStale(r.readinessCachedAt.Load(), r.statusStaleAfter()) {
		errorsLog = appendUnique(errorsLog, "health status stale")
	}
	return errorsLog
}

func heartbeatStale(unix int64, limit time.Duration) bool {
	return unix > 0 && time.Since(time.Unix(unix, 0)) > limit
}

// appendUnique appends value unless values already contains it.
func appendUnique(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

// publicJobError is the client-visible text of a job failure. Only classified
// public failures expose their text, and a text referencing an endpoint URL is
// replaced by the fixed publicSensitiveReason; anything else is internal.
func publicJobError(err error) string {
	if err == nil {
		return "withdraw failed"
	}
	var failure *jobFailure
	if errors.As(err, &failure) && failure.public {
		return publicReasonText(err.Error())
	}
	return "withdraw failed (internal error)"
}
