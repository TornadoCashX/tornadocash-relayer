package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func (r *Relayer) handleJobHealth(w http.ResponseWriter, req *http.Request) {
	if !r.requireHealthGET(w, req) {
		return
	}
	stats, err := r.storeStats(req.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "storage unavailable")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// prometheusTextContentType is the Prometheus text exposition format.
const prometheusTextContentType = "text/plain; version=0.0.4; charset=utf-8"

func (r *Relayer) handleMetrics(w http.ResponseWriter, req *http.Request) {
	if !r.requireHealthGET(w, req) {
		return
	}
	stats, err := r.storeStats(req.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "storage unavailable")
		return
	}
	w.Header().Set("Content-Type", prometheusTextContentType)
	w.WriteHeader(http.StatusOK)
	r.writeJobMetrics(w, stats)
	r.writeOZMetrics(w)
	r.writeRPCMetrics(w)
	if info, err := os.Stat(r.cfg.SQLitePath); err == nil {
		_, _ = fmt.Fprintf(w, "relayer_database_size_bytes %d\n", info.Size())
	}
}

// writeJobMetrics writes HTTP, job, worker and reconciler metrics.
func (r *Relayer) writeJobMetrics(w io.Writer, stats StoreStats) {
	_, _ = fmt.Fprintf(w, "relayer_http_requests_total %d\n", r.httpRequests.Load())
	_, _ = fmt.Fprintf(w, "relayer_rate_limit_rejections_total %d\n", r.rateLimitRejections.Load())
	_, _ = fmt.Fprintf(w, "relayer_jobs_total %d\n", stats.TotalJobs)
	_, _ = fmt.Fprintf(w, "relayer_jobs_current %d\n", stats.CurrentQueue)
	_, _ = fmt.Fprintf(w, "relayer_jobs_protected %d\n", stats.ProtectedJobs)
	oldest := 0.0
	if len(stats.UnknownSubmits) > 0 {
		oldest = time.Since(stats.UnknownSubmits[0].SubmittedAt).Seconds()
	}
	_, _ = fmt.Fprintf(w, "relayer_jobs_unknown_submit %d\n", len(stats.UnknownSubmits))
	_, _ = fmt.Fprintf(w, "relayer_jobs_unknown_submit_oldest_age_seconds %.0f\n", oldest)
	_, _ = fmt.Fprintf(w, "relayer_workers_active %d\n", r.workersActive.Load())
	_, _ = fmt.Fprintf(w, "relayer_reconciler_duration_seconds %.6f\n", float64(r.reconcilerNanos.Load())/float64(time.Second))
}

// writeOZMetrics writes OpenZeppelin Relayer request and status metrics.
func (r *Relayer) writeOZMetrics(w io.Writer) {
	_, _ = fmt.Fprintf(w, "relayer_oz_request_failures_total %d\n", r.ozRequestFailures.Load())
	available, pending := 0, uint64(0)
	if status := r.cachedOZStatus(); status != nil {
		pending = status.PendingTransactionCount
		if !status.Paused && !status.SystemDisabled {
			available = 1
		}
	}
	_, _ = fmt.Fprintf(w, "relayer_oz_available %d\n", available)
	_, _ = fmt.Fprintf(w, "relayer_oz_pending_transactions %d\n", pending)
}

// writeRPCMetrics writes one up gauge per RPC endpoint.
func (r *Relayer) writeRPCMetrics(w io.Writer) {
	if r.chain == nil {
		return
	}
	now := time.Now()
	for _, endpoint := range r.chain.endpoints {
		up := 0
		if endpoint.healthy(now) {
			up = 1
		}
		_, _ = fmt.Fprintf(w, "relayer_rpc_endpoint_up{endpoint=%q} %d\n", endpoint.name, up)
	}
}
