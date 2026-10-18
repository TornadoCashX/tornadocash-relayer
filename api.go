package main

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

const (
	// maxRequestBodyBytes bounds every request body (MaxBytesReader).
	maxRequestBodyBytes = 1 << 20
	// ozUnavailableRetryAfter (seconds) is sent while OZ cannot sign jobs.
	ozUnavailableRetryAfter = "5"
	// rpcUnavailableRetryAfter (seconds) is sent while no RPC endpoint is reachable.
	rpcUnavailableRetryAfter = "30"
)

// errRequestBodyTooLarge reports a body over maxRequestBodyBytes.
var errRequestBodyTooLarge = errors.New("request body too large")

type statusResponse struct {
	RewardAccount     string            `json:"rewardAccount"`
	Instances         InstancesConfig   `json:"instances"`
	NetID             uint64            `json:"netId"`
	EthPrices         *orderedStringMap `json:"ethPrices,omitempty"`
	TornadoServiceFee float64           `json:"tornadoServiceFee"`
	MiningServiceFee  *float64          `json:"miningServiceFee,omitempty"`
	Version           string            `json:"version"`
	Health            healthResponse    `json:"health"`
	CurrentQueue      int               `json:"currentQueue"`
}

// miningServiceFeePercent is reported in /status for compatibility with the
// official relayer UI only; anonymity-mining withdrawals are not supported and
// it is never charged.
const miningServiceFeePercent = 0.05

type healthResponse struct {
	Status    string   `json:"status"`
	Error     string   `json:"error"`
	ErrorsLog []string `json:"errorsLog"`
}

type idResponse struct {
	ID string `json:"id"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type ozHealthResponse struct {
	Address             string `json:"address"`
	Provider            string `json:"provider"`
	RelayerID           string `json:"relayerId"`
	Available           bool   `json:"available"`
	Balance             string `json:"balance,omitempty"`
	PendingTransactions uint64 `json:"pendingTransactions"`
	Nonce               string `json:"nonce,omitempty"`
	Paused              bool   `json:"paused"`
	SystemDisabled      bool   `json:"systemDisabled"`
}

type readinessResponse struct {
	Status string   `json:"status"`
	Errors []string `json:"errors"`
}

type orderedStringMap map[string]string

func (values orderedStringMap) MarshalJSON() ([]byte, error) {
	return marshalOrderedStringMap(values, tokenOrder)
}

type jobStatusResponse struct {
	ID             string    `json:"id"`
	Type           string    `json:"type"`
	Status         JobStatus `json:"status"`
	FailedReason   string    `json:"failedReason"`
	TxHash         string    `json:"txHash,omitempty"`
	InternalStatus JobStatus `json:"internalStatus,omitempty"`
}

func (r *Relayer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", r.handleIndex)
	mux.HandleFunc("/status", r.handleStatus)
	mux.HandleFunc("/v1/status", r.handleStatus)
	mux.HandleFunc("/relay", r.handleWithdraw)
	mux.HandleFunc("/v1/tornadoWithdraw", r.handleWithdraw)
	mux.HandleFunc("/v1/jobs/", r.handleJob)
	mux.HandleFunc("/health", r.handleLiveness)
	mux.HandleFunc("/ready", r.handleReadiness)
	mux.HandleFunc("/health/oz", r.handleOZHealth)
	mux.HandleFunc("/health/jobs", r.handleJobHealth)
	mux.HandleFunc("/metrics", r.handleMetrics)
	return r.withAccessLog(withCORS(r.cfg.CORSOrigins, withJSONLimit(r.withRelayRateLimit(mux), maxRequestBodyBytes)))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += n
	return n, err
}

func (r *Relayer) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, req)
		r.httpRequests.Add(1)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		clientIP := "unknown"
		if ip := r.clientIP(req); ip != nil {
			clientIP = ip.String()
		}
		logDebugf("http method=%s path=%q status=%d bytes=%d duration=%s client_ip=%s peer=%s source_domain=%s cf_ray=%s",
			req.Method, req.URL.Path, status, rec.bytes, time.Since(started).Round(time.Millisecond), clientIP, req.RemoteAddr,
			requestSourceDomain(req), cloudflareRay(req))
	})
}

func withJSONLimit(next http.Handler, limit int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.Body = http.MaxBytesReader(w, req.Body, limit)
		next.ServeHTTP(w, req)
	})
}

func withCORS(origins []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		origin := req.Header.Get("Origin")
		if slices.Contains(origins, "*") {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else if origin != "" && slices.Contains(origins, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		if req.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (r *Relayer) handleIndex(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`This is a Go Tornado withdraw relayer. Check <a href="/v1/status">/v1/status</a> for settings`))
}

func (r *Relayer) handleStatus(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	stats, statsErr := r.storeStats(req.Context())
	health := healthResponse{
		Status:    "true",
		Error:     "",
		ErrorsLog: []string{},
	}
	if statsErr != nil {
		health.Status = "false"
		health.Error = "storage unavailable"
		health.ErrorsLog = []string{"storage unavailable"}
		logErrorf("status store stats failed: %v", statsErr)
	}
	for _, readyError := range r.cachedReadinessErrors() {
		health.Status = "false"
		if health.Error == "" {
			health.Error = readyError
		}
		health.ErrorsLog = appendUnique(health.ErrorsLog, readyError)
	}
	// Price problems only affect ERC20 pools of that currency: reported, but
	// they do not mark the relayer unhealthy for every pool.
	for _, priceError := range r.priceErrors() {
		health.ErrorsLog = appendUnique(health.ErrorsLog, priceError)
	}
	status := statusResponse{
		RewardAccount:     r.cfg.RewardAccount.Hex(),
		Instances:         r.cfg.Instances,
		NetID:             r.cfg.ChainID.Uint64(),
		TornadoServiceFee: float64(r.cfg.ServiceFeeBPS) / 100,
		Version:           r.cfg.Version,
		Health:            health,
		CurrentQueue:      stats.CurrentQueue,
	}
	if r.shouldUseOraclePrices() {
		ethPrices := orderedStringMap(r.ethPriceStrings())
		status.EthPrices = &ethPrices
		miningServiceFee := miningServiceFeePercent
		status.MiningServiceFee = &miningServiceFee
	}

	writeJSON(w, http.StatusOK, status)
}

func (r *Relayer) handleWithdraw(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if retryAfter, reason := r.admissionRefusal(); reason != "" {
		w.Header().Set("Retry-After", retryAfter)
		writeError(w, http.StatusServiceUnavailable, reason)
		return
	}
	body, status, err := r.parseWithdrawRequest(req)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	reqHash := requestHash(r.cfg.ChainID.String(), body)
	nullifierKey := r.nullifierKey(body)
	// Persist request/nullifier ownership before preflight and OZ submission.
	job, duplicate, err := r.store.CreateOrGetPending(req.Context(), body, reqHash, nullifierKey)
	if err != nil {
		writeCreateJobError(w, err)
		return
	}
	if duplicate {
		writeJSON(w, http.StatusOK, idResponse{ID: job.ID})
		return
	}
	// A worker may already own this job (claim dedup); that is fine.
	r.startWithdrawProcessing(job.ID, body)
	writeJSON(w, http.StatusOK, idResponse{ID: job.ID})
}

// admissionRefusal returns the Retry-After value and reason when new
// withdrawals cannot be admitted now; reason is empty when they can.
func (r *Relayer) admissionRefusal() (retryAfter, reason string) {
	if reason := r.ozAdmissionError(); reason != "" {
		// OZ is unverified, paused or disabled: it could not sign the job.
		return ozUnavailableRetryAfter, reason
	}
	// The last health probe reached no RPC endpoint, so preflight would fail.
	// The probe result decides (not one 429), so a single error does not close admission.
	if slices.Contains(r.cachedReadinessErrors(), readinessRPCUnavailable) {
		return rpcUnavailableRetryAfter, readinessRPCUnavailable
	}
	return "", ""
}

// parseWithdrawRequest decodes, canonicalizes and validates the request body.
// On error it also returns the HTTP status to answer with.
func (r *Relayer) parseWithdrawRequest(req *http.Request) (WithdrawRequest, int, error) {
	var body WithdrawRequest
	if err := decodeWithdrawRequest(req, &body); err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			return WithdrawRequest{}, http.StatusRequestEntityTooLarge, err
		}
		return WithdrawRequest{}, http.StatusBadRequest, err
	}
	valid, err := r.validateWithdraw(body)
	if err != nil {
		return WithdrawRequest{}, http.StatusBadRequest, err
	}
	return valid.req, http.StatusOK, nil
}

// writeCreateJobError maps a CreateOrGetPending failure to its HTTP answer.
func writeCreateJobError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrQueueFull):
		writeError(w, http.StatusServiceUnavailable, ErrQueueFull.Error())
	case errors.Is(err, ErrNullifierReverted):
		writeError(w, http.StatusBadRequest, ErrNullifierReverted.Error())
	default:
		logErrorf("create withdraw job failed: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func (r *Relayer) handleOZHealth(w http.ResponseWriter, req *http.Request) {
	if !r.requireHealthGET(w, req) {
		return
	}
	item := ozHealthResponse{
		Address:   r.senderAddress().Hex(),
		Provider:  "openzeppelin-relayer",
		RelayerID: r.cfg.OZRelayerID,
	}
	if status := r.cachedOZStatus(); status != nil {
		item.Balance = status.Balance
		item.PendingTransactions = status.PendingTransactionCount
		item.Nonce = status.Nonce
		item.Paused = status.Paused
		item.SystemDisabled = status.SystemDisabled
		item.Available = r.ozAdmissionError() == ""
	}
	writeJSON(w, http.StatusOK, item)
}

func (r *Relayer) handleLiveness(w http.ResponseWriter, req *http.Request) {
	if !r.requireHealthGET(w, req) {
		return
	}
	writeJSON(w, http.StatusOK, readinessResponse{Status: "ok", Errors: []string{}})
}

func (r *Relayer) handleReadiness(w http.ResponseWriter, req *http.Request) {
	if !r.requireHealthGET(w, req) {
		return
	}
	errorsLog := append(r.readinessErrors(req.Context(), true), r.priceErrors()...)
	if len(errorsLog) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, readinessResponse{Status: "not_ready", Errors: errorsLog})
		return
	}
	writeJSON(w, http.StatusOK, readinessResponse{Status: "ready", Errors: []string{}})
}

func decodeWithdrawRequest(req *http.Request, body *WithdrawRequest) error {
	if err := decodeStrictJSON(req.Body, body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errRequestBodyTooLarge
		}
		return errors.New("invalid JSON body")
	}
	return nil
}

func (r *Relayer) handleJob(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	id := strings.TrimPrefix(req.URL.Path, "/v1/jobs/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing job id")
		return
	}

	job, ok, err := r.store.Get(req.Context(), id)
	if err != nil {
		logErrorf("get job %s failed: %v", id, err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if !ok {
		writeError(w, http.StatusBadRequest, "The job doesn't exist")
		return
	}
	writeJSON(w, http.StatusOK, officialJobStatus(job))
}

func officialJobStatus(job *Job) jobStatusResponse {
	displayStatus := job.Status
	if displayStatus == StatusSubmitting || displayStatus == StatusSubmitted {
		displayStatus = StatusSent
	}
	status := jobStatusResponse{
		ID:           job.ID,
		Type:         job.Type,
		Status:       displayStatus,
		FailedReason: job.FailedReason,
		TxHash:       job.TxHash,
	}
	if displayStatus != job.Status {
		status.InternalStatus = job.Status
	}
	return status
}
