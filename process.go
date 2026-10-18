package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// withdrawTxPlan is the fully checked transaction handed to OZ. OZ signs it
// with its own key and nonce; fee bumps later are bounded by OZ gas_price_cap.
type withdrawTxPlan struct {
	to       common.Address
	data     []byte
	gasLimit uint64
	tip      *big.Int
	maxFee   *big.Int
}

// marketBaseFeeMultiplier: the submitted max fee per gas is
// min(baseFee*multiplier + tip, gas price limit).
const marketBaseFeeMultiplier = 2

// startWithdrawProcessing starts a claimed worker that preflights and submits jobID.
func (r *Relayer) startWithdrawProcessing(jobID string, req WithdrawRequest) {
	r.startWorker(jobID, func(ctx context.Context) {
		r.processWithdraw(ctx, jobID, req)
	})
}

// acquirePreflight takes one of MaxPreflightConcurrency slots or fails when ctx ends.
func (r *Relayer) acquirePreflight(ctx context.Context) error {
	select {
	case r.preflightSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return errors.New("preflight queue timed out")
	}
}

// releasePreflight returns a slot taken by acquirePreflight.
func (r *Relayer) releasePreflight() {
	<-r.preflightSlots
}

// processWithdraw runs preflight and hands the transaction to OZ. Everything
// after the job enters SUBMITTING is owned by the reconciler.
func (r *Relayer) processWithdraw(ctx context.Context, jobID string, req WithdrawRequest) {
	if err := r.acquirePreflight(ctx); err != nil {
		if !errors.Is(ctx.Err(), context.Canceled) {
			r.failJobAndRelease(jobID, retryableFailure(errors.New("relayer is busy: preflight queue timed out")))
		}
		return
	}
	preparing := false
	if err := r.updateJobCritical(jobID, func(job *Job) {
		// A job left in PREPARING by a shutdown or crash is preflighted again:
		// nothing was handed to OZ yet (submit persists SUBMITTING first) and
		// the caller holds the job claim, so only one worker runs it.
		preparing = job.Status == StatusPreparing || transitionJob(job, StatusPreparing)
	}); err != nil {
		r.releasePreflight()
		r.failJobAndRelease(jobID, retryableInternalFailure(errors.New("job persistence failed before preflight")))
		return
	}
	if !preparing {
		r.releasePreflight()
		return
	}
	plan, err := r.prepareWithdrawWithFailover(ctx, req)
	r.releasePreflight()
	if err != nil {
		if !errors.Is(ctx.Err(), context.Canceled) && !r.deferPreflight(jobID, err) {
			r.failJobAndRelease(jobID, err)
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	r.submitToOZ(jobID, plan)
}

// preflightRetryWindow: a job whose preflight failed on market conditions
// (gas price above the limit, proof fee below the current gas cost, oracle
// price missing) is preflighted again every preflightRetryInterval until this
// long after admission. The fee is fixed in the proof, so the user could only
// retry with a new proof; gas spikes usually clear within minutes.
const (
	preflightRetryWindow   = 3 * time.Minute
	preflightRetryInterval = 30 * time.Second
)

// deferPreflight returns the job to QUEUED for a later preflight when err is
// deferrable and the retry window is still open. The nullifier stays locked;
// every retry re-checks isSpent and the root, so a withdrawal done elsewhere
// in the meantime ends the job as already spent.
func (r *Relayer) deferPreflight(jobID string, err error) bool {
	if !failureDeferrable(err) {
		return false
	}
	deferred := false
	if storeErr := r.updateJobCritical(jobID, func(job *Job) {
		if time.Since(job.CreatedAt)+preflightRetryInterval > preflightRetryWindow {
			return
		}
		deferred = transitionJob(job, StatusQueued)
	}); storeErr != nil || !deferred {
		return false
	}
	r.preflightNotBefore.Store(jobID, time.Now().Add(preflightRetryInterval))
	logWarnf("job %s preflight deferred, retrying in %s: %v", jobID, preflightRetryInterval, err)
	return true
}

// preflightDue reports whether a QUEUED/PREPARING job may be preflighted now.
func (r *Relayer) preflightDue(jobID string) bool {
	value, ok := r.preflightNotBefore.Load(jobID)
	if !ok {
		return true
	}
	if time.Now().Before(value.(time.Time)) {
		return false
	}
	r.preflightNotBefore.Delete(jobID)
	return true
}

// prepareWithdrawWithFailover runs the whole preflight on one endpoint (so
// root, nullifier, estimate and simulation come from one chain view) and, only
// when that endpoint failed with an internal/RPC error, reruns it from scratch
// on the next endpoint. Business outcomes (revert, spent nullifier, fee too
// low, gas price above the limit) are final and never retried elsewhere.
func (r *Relayer) prepareWithdrawWithFailover(ctx context.Context, req WithdrawRequest) (*withdrawTxPlan, error) {
	call, err := r.buildWithdrawCall(req)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, endpoint := range r.chain.rotation() {
		plan, err := r.prepareWithdraw(withRPCEndpoint(ctx, endpoint), call)
		if err == nil || !rpcFailoverable(err) || ctx.Err() != nil {
			return plan, err
		}
		logWarnf("preflight on %s failed, trying the next RPC endpoint: %v", endpoint.name, redactSensitive(err.Error()))
		lastErr = err
	}
	return nil, lastErr
}

// rpcFailoverable reports a non-public transient failure, i.e. an RPC or
// internal error rather than a decision about the request.
// An unknown root is the one public outcome that is also asked elsewhere: it
// usually means this node lags behind the root the client just used.
func rpcFailoverable(err error) bool {
	var failure *jobFailure
	return errors.As(err, &failure) && failure.retryable && (!failure.public || errors.Is(err, errRootUnknown))
}

var errRootUnknown = errors.New("withdraw root is not known by the tornado pool")

// errNullifierSpent is the public reason for a withdraw whose nullifier was
// already used on chain, found by preflight or by the reconciler.
var errNullifierSpent = errors.New("withdraw nullifier hash is already spent")

// prepareWithdraw performs every chain check that must pass before OZ may
// sign: root, nullifier, recipient, fee budget, gas estimate and a final
// eth_call with the exact submitted gas and fee fields from the OZ sender
// address. All reads use the RPC endpoint pinned to ctx.
func (r *Relayer) prepareWithdraw(ctx context.Context, call *withdrawCall) (*withdrawTxPlan, error) {
	if err := r.preflightWithdraw(ctx, call.validWithdraw); err != nil {
		return nil, err
	}
	nativeBudget, err := r.nativeGasBudget(call.meta, call.fee)
	if err != nil {
		return nil, err
	}
	estimate, gasLimit, err := r.bufferedGasLimit(ctx, call)
	if err != nil {
		return nil, err
	}
	baseFee, err := r.latestBaseFee(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := r.prepareDynamicFeePlan(ctx, call.data, nativeBudget, estimate, gasLimit, baseFee)
	if err != nil {
		var tooLow *feeTooLowError
		if errors.As(err, &tooLow) {
			return nil, r.feeShortfallError(call.meta, call.fee, tooLow)
		}
		return nil, err
	}
	plan.to = call.pool
	if err := r.simulateWithdrawTx(ctx, call.from, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// withdrawCall is a validated request with its packed calldata, fee and the
// verified OZ sender.
type withdrawCall struct {
	validWithdraw
	from common.Address
	data []byte
	fee  *big.Int
}

// buildWithdrawCall runs the request-only checks: validation, verified sender,
// calldata and fee <= the pool denomination. It makes no chain reads.
func (r *Relayer) buildWithdrawCall(req WithdrawRequest) (*withdrawCall, error) {
	valid, err := r.validateWithdraw(req)
	if err != nil {
		return nil, permanentFailure(err)
	}
	from := r.senderAddress()
	if from == (common.Address{}) {
		return nil, retryableFailure(errors.New("OpenZeppelin Relayer sender is not verified yet"))
	}
	data, fee, err := r.withdrawCallData(valid.req)
	if err != nil {
		return nil, permanentFailure(err)
	}
	if fee.Cmp(valid.meta.Denomination) > 0 {
		return nil, permanentFailure(fmt.Errorf("provided fee exceeds pool denomination: got %s, denomination %s", fee, valid.meta.Denomination))
	}
	return &withdrawCall{validWithdraw: valid, from: from, data: data, fee: fee}, nil
}

// bufferedGasLimit estimates the withdraw from the OZ sender and applies the
// configured buffer, refusing a limit above maxGasLimit.
func (r *Relayer) bufferedGasLimit(ctx context.Context, call *withdrawCall) (estimate, gasLimit uint64, err error) {
	pool := call.pool
	msg := ethereum.CallMsg{From: call.from, To: &pool, Value: new(big.Int), Data: call.data}
	estimate, err = r.estimateGasWithDiagnostic(ctx, msg)
	if err != nil {
		return 0, 0, classifyEstimateError(err)
	}
	gasLimit, err = applyGasBuffer(estimate, r.cfg.GasLimitBufferBPS)
	if err != nil {
		return 0, 0, retryableInternalFailure(err)
	}
	if gasLimit > r.cfg.MaxGasLimit {
		return 0, 0, permanentFailure(fmt.Errorf(
			"buffered gas limit %d (estimate %d, buffer %d bps) exceeds configured maxGasLimit %d",
			gasLimit, estimate, r.cfg.GasLimitBufferBPS, r.cfg.MaxGasLimit))
	}
	return estimate, gasLimit, nil
}

// latestBaseFee returns the EIP-1559 base fee of the latest block.
func (r *Relayer) latestBaseFee(ctx context.Context) (*big.Int, error) {
	header, err := r.latestHeader(ctx)
	if err != nil {
		return nil, retryableInternalFailure(fmt.Errorf("latest block unavailable: %w", err))
	}
	if header.BaseFee == nil {
		return nil, retryableInternalFailure(errors.New("target network does not support EIP-1559 type-2 transactions"))
	}
	return header.BaseFee, nil
}

// withdrawCallData packs the pool withdraw call and returns it with the fee.
// Only zero-refund requests are supported. The shape checks matter for jobCall,
// which rebuilds stored requests without re-applying admission policy.
func (r *Relayer) withdrawCallData(req WithdrawRequest) ([]byte, *big.Int, error) {
	if len(req.Args) != withdrawArgCount {
		return nil, nil, fmt.Errorf("withdraw request must contain exactly %d args", withdrawArgCount)
	}
	fee, err := parseBig(req.Args[argFee])
	if err != nil {
		return nil, nil, fmt.Errorf("invalid fee: %w", err)
	}
	refund, err := parseBig(req.Args[argRefund])
	if err != nil {
		return nil, nil, fmt.Errorf("invalid refund: %w", err)
	}
	if refund.Sign() != 0 {
		return nil, nil, errors.New("non-zero refund transactions are not supported")
	}
	data, err := r.poolABI.Pack(
		"withdraw",
		common.FromHex(req.Proof),
		common.HexToHash(req.Args[argRoot]),
		common.HexToHash(req.Args[argNullifierHash]),
		common.HexToAddress(req.Args[argRecipient]),
		common.HexToAddress(req.Args[argRelayer]),
		fee,
		refund,
	)
	if err != nil {
		return nil, nil, err
	}
	return data, fee, nil
}

// checkFeeBudget enforces that the proof's gas budget (fee - service fee)
// covers the expected execution cost at current prices, minus the configured
// tolerance:
//
//	estimateGas x (base fee + tip) x (1 - GAS_FEE_TOLERANCE_PERCENT)
//
// The gas-limit buffer only protects execution and is not charged to the user:
// unused gas is never paid. A market price above min(maxGasPriceGwei, OZ
// gas_price_cap) is refused. Price moves after submission, gas used above the
// estimate and OZ fee bumps are a bounded operator cost (<= gasLimit x cap).
func (r *Relayer) checkFeeBudget(nativeBudget *big.Int, estimatedGas uint64, market *big.Int, reference string) error {
	limit := r.maxGasPrice()
	if market.Cmp(limit) > 0 {
		return deferrableFailure(fmt.Errorf(
			"network gas price %s wei/gas (%s) is above the relayer gas price limit %s wei/gas",
			market, reference, limit))
	}
	expected := new(big.Int).Mul(market, new(big.Int).SetUint64(estimatedGas))
	// GAS_FEE_TOLERANCE_PERCENT accepts a budget slightly below the expected
	// cost (client-side estimate drift); the operator pays the difference. The
	// error still reports the full expected cost so clients pay it next time.
	if nativeBudget.Cmp(applyBPSCeil(expected, bpsDenominator-r.cfg.GasFeeToleranceBPS)) < 0 {
		return &feeTooLowError{
			requiredGasNative: expected,
			budgetNative:      new(big.Int).Set(nativeBudget),
			estimatedGas:      estimatedGas,
			marketPrice:       new(big.Int).Set(market),
			reference:         reference,
		}
	}
	return nil
}

// prepareDynamicFeePlan prices the transaction from the market only:
// maxFee = min(2 x base fee + tip, gas price limit). The user budget decides
// admission but never lowers the submitted price, so an admitted transaction is
// not underpriced and stuck.
func (r *Relayer) prepareDynamicFeePlan(ctx context.Context, data []byte, nativeBudget *big.Int, estimatedGas, gasLimit uint64, baseFee *big.Int) (*withdrawTxPlan, error) {
	tip, err := r.priorityFee(ctx)
	if err != nil {
		return nil, err
	}
	market := new(big.Int).Add(baseFee, tip)
	if err := r.checkFeeBudget(nativeBudget, estimatedGas, market, fmt.Sprintf("base fee %s + tip %s", baseFee, tip)); err != nil {
		return nil, err
	}
	maxFee := r.marketMaxFee(baseFee, tip)
	logDebugf("fee plan estimatedGas=%d gasLimit=%d baseFee=%s priorityFee=%s maxFee=%s nativeBudget=%s",
		estimatedGas, gasLimit, baseFee, tip, maxFee, nativeBudget)
	return &withdrawTxPlan{data: data, gasLimit: gasLimit, tip: new(big.Int).Set(tip), maxFee: maxFee}, nil
}

// Recent-block window for the observed priority fee (eth_feeHistory).
const (
	tipHistoryBlocks     = 20
	tipHistoryPercentile = 50
)

// priorityFee is min(node suggestion, median of the per-block median tips paid
// in the last tipHistoryBlocks blocks). Some nodes keep suggesting an outdated
// floor (e.g. 1 gwei on BSC while blocks include 0.05 gwei); fee history is what
// blocks actually contain, so it is independent of the node's oracle config.
func (r *Relayer) priorityFee(ctx context.Context) (*big.Int, error) {
	tip, err := r.eth(ctx).SuggestGasTipCap(ctx)
	if err != nil {
		return nil, retryableInternalFailure(fmt.Errorf("suggest EIP-1559 priority fee: %w", err))
	}
	if tip == nil || tip.Sign() < 0 {
		return nil, retryableInternalFailure(errors.New("RPC returned an invalid EIP-1559 priority fee"))
	}
	if observed := r.observedTip(ctx); observed != nil && observed.Cmp(tip) < 0 {
		logDebugf("priority fee: node suggests %s, recent blocks median %s; using the lower", tip, observed)
		tip = observed
	}
	return tip, nil
}

// observedTip returns the median of the per-block median priority fees of
// recent blocks, or nil when fee history is unavailable (the node suggestion is
// then used). Empty blocks report 0 and are ignored.
func (r *Relayer) observedTip(ctx context.Context) *big.Int {
	history, err := r.eth(ctx).FeeHistory(ctx, tipHistoryBlocks, nil, []float64{tipHistoryPercentile})
	if err != nil || history == nil {
		logDebugf("eth_feeHistory unavailable, using node priority fee suggestion: %v", err)
		return nil
	}
	tips := make([]*big.Int, 0, len(history.Reward))
	for _, rewards := range history.Reward {
		if len(rewards) > 0 && rewards[0] != nil && rewards[0].Sign() > 0 {
			tips = append(tips, rewards[0])
		}
	}
	if len(tips) == 0 {
		return nil
	}
	slices.SortFunc(tips, (*big.Int).Cmp)
	return new(big.Int).Set(tips[len(tips)/2])
}

// marketMaxFee is min(2 x base fee + tip, gas price limit). Callers check
// base fee + tip <= limit first, so the result always covers the market price.
func (r *Relayer) marketMaxFee(baseFee, tip *big.Int) *big.Int {
	maxFee := new(big.Int).Mul(baseFee, big.NewInt(marketBaseFeeMultiplier))
	maxFee.Add(maxFee, tip)
	if limit := r.maxGasPrice(); maxFee.Cmp(limit) > 0 {
		maxFee.Set(limit)
	}
	return maxFee
}

// simulationError keeps the raw estimateGas / eth_call errors for logging and
// classification; only a sanitized revert reason is ever exposed.
type simulationError struct {
	estimateErr error
	callErr     error
}

func (e *simulationError) Error() string {
	if e.callErr != nil {
		return fmt.Sprintf("estimateGas failed: %v; eth_call: %v", e.estimateErr, e.callErr)
	}
	return fmt.Sprintf("estimateGas failed: %v", e.estimateErr)
}

func (e *simulationError) Unwrap() error { return e.estimateErr }

// estimateGasWithDiagnostic runs eth_call after a failed estimate on the same
// RPC so a revert reason can be reported.
func (r *Relayer) estimateGasWithDiagnostic(ctx context.Context, msg ethereum.CallMsg) (uint64, error) {
	estimated, err := r.eth(ctx).EstimateGas(ctx, msg)
	if err == nil {
		return estimated, nil
	}
	sim := &simulationError{estimateErr: err}
	if _, callErr := r.eth(ctx).CallContract(ctx, msg, nil); callErr != nil {
		sim.callErr = callErr
	}
	return 0, sim
}

// classifyEstimateError separates simulation reverts (permanent) from RPC
// problems (retryable). Only the revert reason is exposed.
func classifyEstimateError(err error) error {
	estimateErr, callErr := err, error(nil)
	var sim *simulationError
	if errors.As(err, &sim) {
		estimateErr, callErr = sim.estimateErr, sim.callErr
	}
	switch {
	case isRevertError(estimateErr):
		return permanentFailure(errors.New("transaction simulation reverted: " + revertReason(estimateErr)))
	case isRevertError(callErr):
		return permanentFailure(errors.New("transaction simulation reverted: " + revertReason(callErr)))
	case isInsufficientFundsError(estimateErr):
		return retryableFailure(errors.New("relayer sender has insufficient native balance for gas estimation"))
	default:
		return retryableInternalFailure(err)
	}
}

// simulateWithdrawTx eth_calls the exact plan (gas and fee fields) from the
// OZ sender; a revert is final, an RPC problem is retryable.
func (r *Relayer) simulateWithdrawTx(ctx context.Context, from common.Address, plan *withdrawTxPlan) error {
	to := plan.to
	msg := ethereum.CallMsg{
		From: from, To: &to, Value: new(big.Int), Data: plan.data,
		Gas: plan.gasLimit, GasTipCap: plan.tip, GasFeeCap: plan.maxFee,
	}
	if _, err := r.eth(ctx).CallContract(ctx, msg, nil); err != nil {
		switch {
		case isRevertError(err):
			return permanentFailure(errors.New("transaction simulation reverted: " + revertReason(err)))
		case isInsufficientFundsError(err):
			return retryableFailure(errors.New("relayer sender has insufficient native balance for transaction simulation"))
		default:
			return retryableInternalFailure(fmt.Errorf("final transaction simulation failed: %w", err))
		}
	}
	return nil
}

// isInsufficientFundsError matches the balance errors of common node clients.
func isInsufficientFundsError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "insufficient funds") ||
		strings.Contains(msg, "insufficient balance") ||
		strings.Contains(msg, "balance too low")
}

// validatePlan is the last defensive check before OZ signs anything.
func (r *Relayer) validatePlan(plan *withdrawTxPlan) error {
	if plan == nil || plan.to == (common.Address{}) {
		return errors.New("withdraw transaction target pool is not set")
	}
	if _, ok := r.cfg.InstanceByAddress[plan.to]; !ok {
		return errors.New("withdraw transaction target is not a configured pool")
	}
	withdrawMethod, ok := r.poolABI.Methods["withdraw"]
	if !ok || len(plan.data) < len(withdrawMethod.ID) || !bytes.Equal(plan.data[:len(withdrawMethod.ID)], withdrawMethod.ID) {
		return errors.New("withdraw transaction calldata has an invalid selector")
	}
	if plan.tip == nil || plan.maxFee == nil || plan.tip.Sign() < 0 || plan.maxFee.Sign() <= 0 || plan.maxFee.Cmp(plan.tip) < 0 {
		return errors.New("invalid EIP-1559 fee plan")
	}
	return nil
}

// submitToOZ persists SUBMITTING before the POST so a crash or timeout is
// reconciled by listing OZ transactions instead of submitting twice.
func (r *Relayer) submitToOZ(jobID string, plan *withdrawTxPlan) {
	if err := r.validatePlan(plan); err != nil {
		r.failJobAndRelease(jobID, retryableInternalFailure(err))
		return
	}
	submitting := false
	if err := r.updateJobCritical(jobID, func(job *Job) {
		if transitionJob(job, StatusSubmitting) {
			job.SubmitStartedAt = time.Now().UTC()
			submitting = true
		}
	}); err != nil {
		r.failJobAndRelease(jobID, retryableInternalFailure(errors.New("job persistence failed before submit")))
		return
	}
	if !submitting {
		return
	}
	// Submit is detached from the worker deadline: an aborted POST would make
	// the outcome unknown for no benefit.
	ctx, cancel := context.WithTimeout(context.Background(), ozRequestTimeout)
	defer cancel()
	tx, err := r.oz.Submit(ctx, r.ozSubmitRequestFor(plan))
	if err != nil {
		r.ozRequestFailures.Add(1)
		r.handleOZSubmitError(jobID, err)
		return
	}
	logInfof("job %s submitted to OpenZeppelin Relayer tx=%s status=%s", jobID, tx.ID, tx.Status)
	r.applyOZTransaction(jobID, tx)
}

// ozSubmitRequestFor converts a validated plan into the OZ submit payload.
// valid_until is counted from now (just after SubmitStartedAt is persisted).
func (r *Relayer) ozSubmitRequestFor(plan *withdrawTxPlan) ozSubmitRequest {
	return ozSubmitRequest{
		To:                   plan.to.Hex(),
		Value:                "0",
		Data:                 hexutil.Encode(plan.data),
		GasLimit:             plan.gasLimit,
		MaxFeePerGas:         jsonNumber(plan.maxFee),
		MaxPriorityFeePerGas: jsonNumber(plan.tip),
		ValidUntil:           time.Now().UTC().Add(r.cfg.OZTxValidity).Format(time.RFC3339),
	}
}

// handleOZSubmitError releases the job only when OZ provably created nothing;
// any other failure leaves the outcome unknown for the reconciler.
func (r *Relayer) handleOZSubmitError(jobID string, err error) {
	if errors.Is(err, errOZNotFound) {
		// Unknown relayer id: OZ created nothing.
		logErrorf("job %s rejected: OpenZeppelin Relayer %q not found", jobID, r.cfg.OZRelayerID)
		r.failJobAndRelease(jobID, retryableFailure(errors.New("transaction rejected by OpenZeppelin Relayer: relayer not found")))
		return
	}
	var httpErr *ozHTTPError
	if errors.As(err, &httpErr) && httpErr.definitiveRejection() {
		// OZ validated and refused the request; no transaction exists.
		logWarnf("job %s rejected by OpenZeppelin Relayer: %v", jobID, err)
		// OZ's message can carry balances or internals; it is logged only.
		r.failJobAndRelease(jobID, retryableFailure(fmt.Errorf("transaction rejected by OpenZeppelin Relayer (HTTP %d)", httpErr.status)))
		return
	}
	// Outcome unknown: the reconciler searches for the transaction but never
	// releases the nullifier based only on a negative list result.
	logWarnf("job %s OpenZeppelin submit outcome unknown, reconciling: %v", jobID, err)
}

// jsonNumber renders a wei amount as an exact JSON number.
func jsonNumber(value *big.Int) json.Number {
	return json.Number(value.String())
}
