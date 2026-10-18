package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	// startupPriceSeedTimeout bounds the initial oracle price refresh.
	startupPriceSeedTimeout = 20 * time.Second
	// ozVerifyRetryFloor is the minimum delay between startup verifications.
	ozVerifyRetryFloor = 5 * time.Second
	// ozStatusTimeout bounds one OZ status read of the health monitor.
	ozStatusTimeout = 10 * time.Second
	// reconcileJobTimeout bounds the OZ and chain reads for one job.
	reconcileJobTimeout = 30 * time.Second
	// ozListLookback widens the OZ transaction search before SubmitStartedAt
	// to absorb clock skew between this host and OZ.
	ozListLookback = 5 * time.Minute
)

// finalizedBlock returns a fresh "finalized" block tag for eth calls.
func finalizedBlock() *big.Int {
	return big.NewInt(int64(rpc.FinalizedBlockNumber))
}

// runStartup verifies the OZ relayer, seeds prices and then starts the
// background loops. Admission stays closed until OZ is verified, because the
// OZ sender address is the msg.sender of every preflight simulation.
func (r *Relayer) runStartup(ctx context.Context) {
	// Recovery must not depend on successful admission verification. Persisted
	// jobs may still be confirmable from OZ or chain while new requests remain
	// closed, so start reconciliation immediately.
	r.workerWG.Add(2)
	go func() {
		defer r.workerWG.Done()
		r.runReconciler(ctx)
	}()
	go func() {
		defer r.workerWG.Done()
		r.runHealthMonitor(ctx)
	}()
	if r.shouldUseOraclePrices() {
		priceCtx, priceCancel := context.WithTimeout(ctx, startupPriceSeedTimeout)
		r.refreshPrices(priceCtx)
		priceCancel()
	}
	for {
		err := r.verifyOZRelayer(ctx)
		if err == nil {
			break
		}
		logErrorf("OpenZeppelin Relayer verification failed; withdrawals stay closed: %v", err)
		r.setReadinessErrors([]string{"openzeppelin relayer not verified"})
		select {
		case <-ctx.Done():
			return
		case <-time.After(max(r.cfg.PollPeriod, ozVerifyRetryFloor)):
		}
	}
	r.startupPending.Store(false)
	r.refreshReadinessCache(ctx)
	logInfof("OpenZeppelin Relayer verified sender=%s; accepting withdrawals", r.senderAddress().Hex())

	if r.shouldUseOraclePrices() {
		r.workerWG.Add(1)
		go func() {
			defer r.workerWG.Done()
			r.runPriceWatcher(ctx)
		}()
	}
}

// verifyOZRelayer checks that the configured OZ relayer signs on NET_ID, only
// accepts the configured pools and has a gas price cap no higher than
// maxGasPriceGwei, then records its sender address.
func (r *Relayer) verifyOZRelayer(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, startupValidationTimeout)
	defer cancel()
	relayer, err := r.oz.GetRelayer(ctx)
	if err != nil {
		return err
	}
	if relayer.NetworkType != "" && relayer.NetworkType != "evm" {
		return fmt.Errorf("OZ relayer %s is not an EVM relayer (network_type=%s)", relayer.ID, relayer.NetworkType)
	}
	if !common.IsHexAddress(relayer.Address) || common.HexToAddress(relayer.Address) == (common.Address{}) {
		return fmt.Errorf("OZ relayer %s has no valid address", relayer.ID)
	}
	gasPriceCap, err := r.checkOZPolicy(relayer.Policies)
	if err != nil {
		return err
	}
	if relayer.Paused || relayer.SystemDisabled {
		return fmt.Errorf("OZ relayer %s is paused or system-disabled", relayer.ID)
	}
	chainID, err := r.oz.ChainID(ctx)
	if err != nil {
		return err
	}
	if chainID.Cmp(r.cfg.ChainID) != 0 {
		return fmt.Errorf("OZ relayer %s signs for chain %s, but NET_ID=%s", relayer.ID, chainID, r.cfg.ChainID)
	}
	status, err := r.oz.GetStatus(ctx)
	if err != nil {
		return err
	}
	address := common.HexToAddress(relayer.Address)
	r.ozMu.Lock()
	r.ozAddress = address
	r.ozGasPriceCap = gasPriceCap
	r.ozStatus = status
	r.ozStatusAt = time.Now()
	r.ozVerifyErr = nil
	r.ozVerifiedAt = time.Now()
	r.ozMu.Unlock()
	return nil
}

// ozReverifyPeriod: OZ relayers can be reconfigured at runtime (PATCH), so the
// address, chain and policy are re-checked periodically.
const ozReverifyPeriod = 5 * time.Minute

// reverifyOZRelayer re-runs verifyOZRelayer once ozReverifyPeriod has passed.
func (r *Relayer) reverifyOZRelayer(ctx context.Context) {
	r.ozMu.RLock()
	due := time.Since(r.ozVerifiedAt) >= ozReverifyPeriod
	r.ozMu.RUnlock()
	if !due {
		return
	}
	// On failure ozVerifiedAt stays unchanged, so the check repeats every round
	// and admission reopens as soon as OZ verifies again (a short OZ restart
	// must not close withdrawals for a whole ozReverifyPeriod).
	if err := r.verifyOZRelayer(ctx); err != nil {
		logErrorf("OpenZeppelin Relayer re-verification failed; new withdrawals refused: %v", err)
		r.ozMu.Lock()
		r.ozVerifyErr = err
		r.ozMu.Unlock()
	}
}

// checkOZPolicy requires EIP-1559 pricing, caller-provided gas limits, a
// receiver whitelist equal to the configured pools and a positive gas price
// cap within maxGasPriceGwei. It returns that cap.
func (r *Relayer) checkOZPolicy(policy ozEVMPolicy) (*big.Int, error) {
	allowed := make(map[common.Address]bool, len(policy.WhitelistReceivers))
	for _, receiver := range policy.WhitelistReceivers {
		if common.IsHexAddress(receiver) {
			allowed[common.HexToAddress(receiver)] = true
		}
	}
	if policy.EIP1559Pricing == nil || !*policy.EIP1559Pricing {
		return nil, errors.New("OZ relayer policy eip1559_pricing must be true")
	}
	if policy.GasLimitEstimation == nil || *policy.GasLimitEstimation {
		return nil, errors.New("OZ relayer policy gas_limit_estimation must be false")
	}
	if len(allowed) == 0 {
		return nil, errors.New("OZ relayer policy whitelist_receivers must list the configured pools")
	}
	for pool := range r.cfg.InstanceByAddress {
		if !allowed[pool] {
			return nil, fmt.Errorf("OZ relayer policy whitelist_receivers is missing pool %s", pool.Hex())
		}
	}
	if len(allowed) != len(r.cfg.InstanceByAddress) {
		return nil, errors.New("OZ relayer policy whitelist_receivers must exactly match the configured pools")
	}
	capValue, ok := new(big.Int).SetString(policy.GasPriceCap.String(), 10)
	if !ok || capValue.Sign() <= 0 {
		return nil, errors.New("OZ relayer policy gas_price_cap must be set and positive")
	}
	if r.cfg.MaxGasPriceWei.Sign() > 0 && capValue.Cmp(r.cfg.MaxGasPriceWei) > 0 {
		return nil, fmt.Errorf("OZ relayer policy gas_price_cap %s wei exceeds maxGasPriceGwei %s wei", capValue, r.cfg.MaxGasPriceWei)
	}
	if r.cfg.MaxGasPriceWei.Sign() > 0 && capValue.Cmp(r.cfg.MaxGasPriceWei) < 0 {
		logWarnf("OZ relayer policy gas_price_cap %s wei is below maxGasPriceGwei %s wei; the lower value limits submissions", capValue, r.cfg.MaxGasPriceWei)
	}
	return capValue, nil
}

// runReconciler re-verifies OZ, cleans the store and advances active jobs
// every PollPeriod until ctx is cancelled.
func (r *Relayer) runReconciler(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.PollPeriod)
	defer ticker.Stop()
	for {
		started := time.Now()
		r.reconcilerHeartbeat.Store(started.Unix())
		r.reverifyOZRelayer(ctx)
		if err := r.store.Cleanup(ctx); err != nil {
			logWarnf("store cleanup skipped: %v", err)
		}
		r.reconcileActiveJobs(ctx)
		r.reconcilerNanos.Store(time.Since(started).Nanoseconds())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runHealthMonitor refreshes the OZ status and the cached readiness on its own
// ticker. It is independent of the reconciler so that a long or stuck
// reconciliation round cannot leave /status and admission on stale data; a
// stuck reconciler shows up instead as a stale heartbeat.
func (r *Relayer) runHealthMonitor(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.PollPeriod)
	defer ticker.Stop()
	for {
		r.refreshOZStatus(ctx)
		if !r.startupPending.Load() {
			r.refreshReadinessCache(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// refreshOZStatus caches the OZ relayer status; nil marks OZ unavailable.
func (r *Relayer) refreshOZStatus(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, ozStatusTimeout)
	defer cancel()
	status, err := r.oz.GetStatus(ctx)
	if err != nil {
		r.ozRequestFailures.Add(1)
		logWarnf("OpenZeppelin Relayer status unavailable: %v", err)
		status = nil
	}
	r.ozMu.Lock()
	r.ozStatus = status
	r.ozStatusAt = time.Now()
	r.ozMu.Unlock()
}

// ozFailuresPerRound consecutive OZ failures mean OZ itself is down; the rest
// of the round skips OZ instead of waiting out one timeout per job.
const ozFailuresPerRound = 3

// reconcileActiveJobs runs one reconciliation round over every active job.
func (r *Relayer) reconcileActiveJobs(ctx context.Context) {
	jobs, err := r.store.ListActive(ctx)
	if err != nil {
		logWarnf("active job reconciliation skipped: %v", err)
		return
	}
	// OZ is shared by every job: if its status endpoint just failed, polling
	// each job would only wait out one timeout per job.
	ozUp := r.cachedOZStatus() != nil
	// One job whose OZ record keeps failing must not starve the rest: the
	// round stops polling OZ only after several consecutive failures, and it
	// starts at a rotating offset so no job is always first.
	consecutiveOZFailures := 0
	if n := len(jobs); n > 1 {
		offset := int(r.reconcileRound.Add(1) % uint64(n))
		jobs = append(jobs[offset:], jobs[:offset]...)
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		// Progress, not only round starts, proves the reconciler is alive: a
		// round over many slow jobs is long but not stuck.
		r.reconcilerHeartbeat.Store(time.Now().Unix())
		switch job.Status {
		case StatusQueued, StatusPreparing:
			// Nothing was submitted yet (e.g. restart during preflight). These
			// require a verified OZ sender for simulation, unlike submitted jobs.
			if !r.startupPending.Load() && r.preflightDue(job.ID) {
				r.startWithdrawProcessing(job.ID, job.Request)
			}
		default:
			if !ozUp || !r.claimJob(job.ID) {
				continue
			}
			ozFailed := r.reconcileJob(ctx, job.ID)
			r.releaseJob(job.ID)
			if !ozFailed {
				consecutiveOZFailures = 0
			} else if consecutiveOZFailures++; consecutiveOZFailures >= ozFailuresPerRound {
				ozUp = false
			}
		}
	}
}

// reconcileJob advances one job that OZ may own. The caller holds its claim.
// It reports whether OZ failed to answer, so the round can stop polling OZ.
func (r *Relayer) reconcileJob(parent context.Context, jobID string) (ozFailed bool) {
	ctx, cancel := context.WithTimeout(parent, reconcileJobTimeout)
	defer cancel()
	// Every chain read of this job's decision comes from one endpoint; the
	// next round may use the other one.
	ctx = r.pinRPC(ctx)
	job, ok, err := r.store.Get(ctx, jobID)
	if err != nil || !ok || isTerminalStatus(job.Status) {
		return false
	}
	if job.OZTxID == "" {
		return r.reconcileUnknownSubmit(ctx, job)
	}
	tx, err := r.oz.GetTransaction(ctx, job.OZTxID)
	if errors.Is(err, errOZNotFound) {
		// OZ prunes terminal transactions; chain state decides.
		r.resolveByChain(ctx, job, nil, "OpenZeppelin Relayer no longer has the transaction")
		return false
	}
	if err != nil {
		r.ozRequestFailures.Add(1)
		// OZ unavailable: it is the source of truth for this transaction, so
		// the job (and its nullifier) waits until OZ answers again.
		logWarnf("job %s OpenZeppelin poll failed: %v", jobID, err)
		return true
	}
	r.applyOZTransactionCtx(ctx, job.ID, tx)
	return false
}

// unknownSubmitWarnAfter only controls when a still-unknown submission is
// logged for operator attention; the nullifier is never released by time.
const unknownSubmitWarnAfter = 2 * time.Minute

// reconcileUnknownSubmit resolves a SUBMITTING job whose POST outcome is
// unknown. The withdraw calldata contains the nullifier, so to+data identifies
// the OZ transaction uniquely. A negative list result is not proof that the
// POST created nothing: OZ has no idempotency key and its list can be delayed
// or pruned. The job therefore keeps its nullifier until a matching transaction
// is found or an operator resolves it.
func (r *Relayer) reconcileUnknownSubmit(ctx context.Context, job *Job) (ozFailed bool) {
	pool, data, err := r.jobCall(job)
	if err != nil {
		// The POST may still have created a transaction; without its to+data
		// it cannot be found, so the job stays quarantined for an operator.
		logErrorf("job %s stored request cannot be rebuilt; retaining nullifier for manual review: %v", job.ID, err)
		return false
	}
	started := submitStarted(job)
	tx, err := r.oz.FindTransaction(ctx, pool, hexutil.Encode(data), started.Add(-ozListLookback))
	if err != nil {
		r.ozRequestFailures.Add(1)
		logWarnf("job %s OpenZeppelin reconciliation failed: %v", job.ID, err)
		// Only an OZ outage pauses the round. A scan problem specific to this
		// job (page limit, bad record, scan longer than the job deadline) must
		// not stall every other job behind it, as this job is retained forever.
		return !errors.Is(err, errOZScanIncomplete) && ctx.Err() == nil
	}
	if tx != nil {
		logInfof("job %s reconciled OpenZeppelin transaction %s", job.ID, tx.ID)
		r.applyOZTransactionCtx(ctx, job.ID, tx)
		return false
	}
	if time.Since(started) > unknownSubmitWarnAfter {
		logWarnf("job %s submission outcome remains unknown; retaining nullifier for manual review", job.ID)
	}
	return false
}

// applyOZTransaction applies a fresh OZ record with its own deadline and a
// pinned RPC endpoint.
func (r *Relayer) applyOZTransaction(jobID string, tx *ozTransaction) {
	ctx, cancel := context.WithTimeout(r.runtimeContext(), reconcileJobTimeout)
	defer cancel()
	r.applyOZTransactionCtx(r.pinRPC(ctx), jobID, tx)
}

// applyOZTransactionCtx stores the OZ id and the current hash (OZ replaces the
// hash on every fee bump) and maps the OZ status onto the job.
func (r *Relayer) applyOZTransactionCtx(ctx context.Context, jobID string, tx *ozTransaction) {
	if tx == nil || tx.ID == "" {
		logErrorf("job %s received an invalid OpenZeppelin transaction", jobID)
		return
	}
	if tx.Hash != "" && !validTxHash(tx.Hash) {
		logErrorf("job %s OpenZeppelin transaction %s returned an invalid hash", jobID, tx.ID)
		return
	}
	job, ok, err := r.store.Get(ctx, jobID)
	if err != nil || !ok || isTerminalStatus(job.Status) {
		return
	}
	if job.OZTxID != "" && job.OZTxID != tx.ID {
		logErrorf("job %s OpenZeppelin id mismatch stored=%s got=%s", jobID, job.OZTxID, tx.ID)
		return
	}
	next := nextStatusForOZ(jobID, job.Status, tx.Status)
	if err := r.updateJobCritical(jobID, func(stored *Job) {
		if stored.OZTxID == "" {
			stored.OZTxID = tx.ID
		}
		recordTxHash(stored, tx.Hash)
		if stored.Status != next {
			transitionJob(stored, next)
		}
	}); err != nil {
		return
	}
	job, ok, err = r.store.Get(ctx, jobID)
	if err != nil || !ok {
		return
	}
	r.finishOZTransaction(ctx, job, tx)
}

// nextStatusForOZ maps an OZ transaction status onto the job status to persist
// together with the OZ id and hash. Final OZ statuses only advance SUBMITTING
// to SUBMITTED here; their final decision is taken by finishOZTransaction once
// the id and hash are durable.
func nextStatusForOZ(jobID string, current JobStatus, ozStatus string) JobStatus {
	switch ozStatus {
	case ozStatusPending, ozStatusSent, ozStatusSubmitted:
		return StatusSubmitted
	case ozStatusMined:
		return StatusMined
	case ozStatusConfirmed, ozStatusFailed, ozStatusCanceled, ozStatusExpired:
		if current == StatusSubmitting {
			return StatusSubmitted
		}
		return current
	default:
		logWarnf("job %s unknown OpenZeppelin status %q", jobID, ozStatus)
		return current
	}
}

// finishOZTransaction takes the final decision for a final OZ status.
func (r *Relayer) finishOZTransaction(ctx context.Context, job *Job, tx *ozTransaction) {
	jobID := job.ID
	switch tx.Status {
	case ozStatusConfirmed:
		r.verifyConfirmed(ctx, job, tx)
	case ozStatusFailed, ozStatusCanceled, ozStatusExpired:
		// reason becomes the public failedReason; OZ's free-text status_reason
		// (balances, internals) is logged only.
		if tx.StatusReason != "" {
			logWarnf("job %s OpenZeppelin status %s: %s", jobID, tx.Status, redactSensitive(tx.StatusReason))
		}
		r.resolveByChain(ctx, job, tx, "OpenZeppelin Relayer status "+tx.Status)
	}
}

// verifyConfirmed cross-checks an OZ "confirmed" answer against the local
// read-only RPC: the receipt must exist with status 1, the transaction must be
// this withdraw to this pool, and the nullifier must be spent. A NOOP (fee
// replacement after expiry or cancel) does not match and is resolved by chain.
func (r *Relayer) verifyConfirmed(ctx context.Context, job *Job, tx *ozTransaction) {
	pool, data, err := r.jobCall(job)
	if err != nil {
		logErrorf("job %s stored request invalid: %v", job.ID, err)
		return
	}
	if !tx.targets(pool, hexutil.Encode(data)) {
		r.resolveByChain(ctx, job, tx, "OpenZeppelin Relayer confirmed a replacement transaction instead of the withdrawal")
		return
	}
	confirmed, reverted, err := r.withdrawReceiptOK(ctx, common.HexToHash(tx.Hash), pool, data)
	if err != nil {
		logWarnf("job %s confirmation not yet visible on HTTP_RPC_URL: %v", job.ID, err)
		return
	}
	if reverted {
		r.resolveByChain(ctx, job, tx, "withdraw transaction reverted on chain")
		return
	}
	if !confirmed {
		// The chain transaction at OZ's hash is not this withdrawal although
		// OZ reports it is: resolve by chain instead of waiting forever.
		r.resolveByChain(ctx, job, tx, "OpenZeppelin Relayer confirmed a transaction that does not match the withdrawal on chain")
		return
	}
	spent, err := r.nullifierSpent(ctx, job, nil)
	if err != nil || !spent {
		logWarnf("job %s receipt found but nullifier not yet spent on HTTP_RPC_URL (err=%v)", job.ID, err)
		return
	}
	if err := r.updateJobCritical(job.ID, func(stored *Job) {
		recordTxHash(stored, tx.Hash)
		if transitionJob(stored, StatusConfirmed) {
			stored.FailedReason = ""
		}
	}); err != nil {
		return // updateJobCritical logged it; the next round retries
	}
	logInfof("job %s confirmed tx=%s", job.ID, tx.Hash)
}

// hashOutcome classifies one recorded transaction hash of a job.
type hashOutcome int

const (
	hashAbsent    hashOutcome = iota // unknown to the chain, or not this withdrawal
	hashUncertain                    // pending, unfinalized or unreadable: forbids release
	hashReverted                     // finalized matching withdrawal with status 0
	hashSucceeded                    // finalized matching withdrawal with status 1
)

// hashVerdict aggregates the outcomes of every recorded hash of a job.
type hashVerdict struct {
	succeededHash string // first finalized successful hash, if any
	sawReverted   bool
	// blocked: uncertainty forbids failure/release, but not a later finalized success.
	blocked bool
}

// resolveByChain resolves a job whose OZ transaction did not (verifiably)
// execute the withdrawal. Any recorded hash that succeeded on chain wins. An
// unspent nullifier is released only with proof that no version of the
// transaction can still execute (see provenDead); otherwise protection is
// retained, because OZ exposes only its current hash and an unobserved
// replacement may still be live. tx is the final OZ record, or nil once OZ
// pruned it; a pruned job with no on-chain result is abandoned after
// ozPrunedAbandonAfter.
func (r *Relayer) resolveByChain(ctx context.Context, job *Job, tx *ozTransaction, reason string) {
	pool, data, err := r.jobCall(job)
	if err != nil {
		logErrorf("job %s stored request invalid: %v", job.ID, err)
		return
	}
	verdict := r.inspectJobHashes(ctx, job, pool, data, reason)
	if verdict.succeededHash != "" {
		r.confirmByChain(job, verdict.succeededHash, reason)
		return
	}
	// Do not let a stale pending/error on an earlier hash hide a finalized
	// successful replacement. But absent such success, retain the lock until
	// every uncertain attempt is resolved; in particular, never fall through
	// to the revert or nonce-based release paths while blocked.
	if verdict.blocked {
		return
	}
	if r.resolveIfSpent(ctx, job, reason) {
		return
	}
	if verdict.sawReverted {
		r.resolveReverted(ctx, job, reason)
		return
	}
	r.releaseIfProvenDead(ctx, job, tx, reason)
}

// inspectJobHashes checks every recorded hash; a finalized success stops the
// scan immediately.
func (r *Relayer) inspectJobHashes(ctx context.Context, job *Job, pool common.Address, data []byte, reason string) hashVerdict {
	var verdict hashVerdict
	for _, hash := range jobTxHashes(job) {
		switch r.inspectHash(ctx, job, hash, pool, data, reason) {
		case hashSucceeded:
			verdict.succeededHash = hash
			return verdict
		case hashReverted:
			verdict.sawReverted = true
		case hashUncertain:
			verdict.blocked = true
		}
	}
	return verdict
}

// inspectHash classifies one hash. OZ no longer vouches for this transaction
// (final or pruned), so a terminal outcome needs a receipt that a reorg cannot
// remove.
func (r *Relayer) inspectHash(ctx context.Context, job *Job, hash string, pool common.Address, data []byte, reason string) hashOutcome {
	confirmed, reverted, err := r.withdrawReceiptOK(ctx, common.HexToHash(hash), pool, data)
	if errors.Is(err, ethereum.NotFound) {
		return r.inspectMissingReceipt(ctx, job, hash, reason)
	}
	if err != nil {
		logWarnf("job %s receipt lookup failed: %v", job.ID, err)
		return hashUncertain
	}
	if !confirmed && !reverted {
		return hashAbsent
	}
	final, succeeded, err := r.receiptFinalized(ctx, common.HexToHash(hash))
	switch {
	case err != nil:
		logWarnf("job %s finality check failed: %v", job.ID, err)
		return hashUncertain
	case !final:
		logInfof("job %s tx %s is not verifiably finalized (%s); waiting", job.ID, hash, redactSensitive(reason))
		return hashUncertain
	case succeeded:
		return hashSucceeded
	default:
		return hashReverted
	}
}

// inspectMissingReceipt handles a hash without a receipt. A transaction OZ
// gave up on (or forgot) can still be in a mempool; releasing the nullifier
// then would race it.
func (r *Relayer) inspectMissingReceipt(ctx context.Context, job *Job, hash, reason string) hashOutcome {
	_, pending, err := r.eth(ctx).TransactionByHash(ctx, common.HexToHash(hash))
	switch {
	case err == nil:
		// Pending, or mined while the node has no receipt yet (an
		// inconsistent or lagging view): either way it may still win.
		logWarnf("job %s tx %s is known on chain without a receipt (pending=%v, %s); waiting", job.ID, hash, pending, reason)
		return hashUncertain
	case errors.Is(err, ethereum.NotFound):
		return hashAbsent
	default:
		logWarnf("job %s transaction lookup failed: %v", job.ID, err)
		return hashUncertain
	}
}

// confirmByChain records a finalized successful hash as CONFIRMED.
func (r *Relayer) confirmByChain(job *Job, hash, reason string) {
	if err := r.updateJobCritical(job.ID, func(stored *Job) {
		recordTxHash(stored, hash)
		transitionJob(stored, StatusConfirmed)
	}); err != nil {
		return // updateJobCritical logged it; the next round retries
	}
	logInfof("job %s confirmed by chain tx=%s (%s)", job.ID, hash, reason)
}

// resolveIfSpent reports true when the isSpent check decided the job or must
// pause it; false means the nullifier is unspent at latest.
func (r *Relayer) resolveIfSpent(ctx context.Context, job *Job, reason string) bool {
	spent, err := r.nullifierSpent(ctx, job, nil)
	if err != nil {
		logWarnf("job %s isSpent check failed: %v", job.ID, err)
		return true
	}
	if !spent {
		return false
	}
	// A spend seen only at latest can still be reorganized away.
	finalSpent, err := r.nullifierSpent(ctx, job, finalizedBlock())
	switch {
	case err != nil:
		logWarnf("job %s finalized isSpent check failed: %v", job.ID, err)
	case !finalSpent:
		logInfof("job %s nullifier is spent but not finalized yet (%s); waiting", job.ID, redactSensitive(reason))
	default:
		r.failJobAndRelease(job.ID, permanentFailure(errNullifierSpent))
	}
	return true
}

// resolveReverted handles a finalized revert with an unspent nullifier.
//
// The proof's root can fall out of the pool's root history while OZ queues or
// bumps the transaction. That revert is not the user's doing, and a new proof
// with a current root can succeed. Roots only leave the history, so a root
// still known now was known when the transaction reverted; an attacker cannot
// cheaply push it out (that takes a full root history of deposits).
//
// Otherwise the revert is attributable to the request, e.g. the recipient
// added an EIP-7702 delegation after preflight. Each revert costs the relayer
// gas and earns no fee, so this note is refused permanently (recorded before
// the nullifier lock is released).
func (r *Relayer) resolveReverted(ctx context.Context, job *Job, reason string) {
	known, err := r.rootKnown(ctx, job)
	if err != nil {
		logWarnf("job %s isKnownRoot check failed: %v", job.ID, err)
		return
	}
	if !known {
		r.failJobAndRelease(job.ID, retryableFailure(errors.New("submitted transaction reverted and its merkle root is no longer known; generate a new proof: "+redactSensitive(reason))))
		return
	}
	if err := r.retryStoreWrite(func(ctx context.Context) error {
		return r.store.MarkReverted(ctx, job.NullifierKey)
	}); err != nil {
		logErrorf("job %s revert could not be recorded; nullifier ownership retained: %v", job.ID, err)
		return
	}
	r.failJobAndRelease(job.ID, permanentFailure(errors.New("submitted transaction reverted on chain: "+redactSensitive(reason))))
}

// releaseIfProvenDead releases the nullifier only when no version of the
// transaction can still execute, or abandons a pruned job.
//
// OZ pruned the record and the chain shows no result: no recorded hash mined
// or is pending, and the nullifier is unspent. Once OZ's valid_until is well
// past, the job is abandoned instead of being held forever. The residual risk
// is an unseen copy still mining later; the pool contract rejects a second
// spend, so the cost is at most a reverted resubmission.
func (r *Relayer) releaseIfProvenDead(ctx context.Context, job *Job, tx *ozTransaction, reason string) {
	dead, proof, err := r.provenDead(ctx, job, tx)
	if err != nil {
		logWarnf("job %s nonce check failed: %v", job.ID, err)
		return
	}
	if dead {
		r.failJobAndRelease(job.ID, retryableFailure(errors.New("submitted transaction did not execute ("+proof+"): "+redactSensitive(reason))))
		return
	}
	if tx == nil {
		if since := time.Since(submitStarted(job)); since > r.ozPrunedAbandonAfter() {
			r.failJobAndRelease(job.ID, retryableFailure(fmt.Errorf("submitted transaction has no on-chain result %s after submission; abandoned", since.Truncate(time.Minute))))
			return
		}
	}
	logWarnf("job %s terminal OZ outcome is not yet safe to release (%s); retaining nullifier", job.ID, redactSensitive(reason))
}

// ozPrunedAbandonMargin is added to OZ's valid_until before a job whose OZ
// record is gone and whose transaction has no on-chain result is abandoned.
const ozPrunedAbandonMargin = 15 * time.Minute

func (r *Relayer) ozPrunedAbandonAfter() time.Duration {
	return r.cfg.OZTxValidity + ozPrunedAbandonMargin
}

// submitStarted is when the job was handed to OZ (valid_until is counted
// from just after it).
func submitStarted(job *Job) time.Time {
	if !job.SubmitStartedAt.IsZero() {
		return job.SubmitStartedAt
	}
	return job.UpdatedAt
}

// provenDead reports whether no version of the OZ transaction can ever execute,
// given that no recorded hash succeeded and the nullifier is unspent. It needs
// the final OZ record (OZ will not sign or resend it again):
//   - never broadcast: OZ recorded no hash and Go never saw one, so nothing was
//     signed (e.g. rejected for balance or block gas limit before signing);
//   - nonce consumed: the original, every fee bump and any NOOP share one
//     nonce. Once the sender's nonce at the finalized block is past it, that
//     slot is permanently used, and since isSpent is false the transaction that
//     used it was not this withdrawal.
func (r *Relayer) provenDead(ctx context.Context, job *Job, tx *ozTransaction) (bool, string, error) {
	if tx == nil || !ozFinalStatus(tx.Status) || tx.ID != job.OZTxID {
		return false, "", nil
	}
	if tx.Hash == "" && len(jobTxHashes(job)) == 0 {
		return true, "never broadcast", nil
	}
	if tx.Nonce == "" || !common.IsHexAddress(tx.From) {
		return false, "", nil
	}
	nonce, err := strconv.ParseUint(tx.Nonce.String(), 10, 64)
	if err != nil {
		return false, "", fmt.Errorf("OZ transaction %s has invalid nonce %q", tx.ID, tx.Nonce)
	}
	from := common.HexToAddress(tx.From)
	if from == (common.Address{}) {
		return false, "", nil
	}
	// The nonce and isSpent are read at the same finalized block number: the
	// caller's isSpent read was at latest, and a lagging backend could serve
	// a latest view older than the finalized one.
	header, err := r.finalizedHeader(ctx)
	if err != nil {
		return false, "", err
	}
	finalized, err := r.eth(ctx).NonceAt(ctx, from, header.Number)
	if err != nil {
		return false, "", err
	}
	if finalized <= nonce {
		return false, "", nil
	}
	spent, err := r.nullifierSpent(ctx, job, header.Number)
	if err != nil {
		return false, "", err
	}
	if spent {
		return false, "", nil // spent after all: the next round resolves it as spent
	}
	return true, fmt.Sprintf("nonce %d finalized without this withdrawal", nonce), nil
}

// finalizedHeader reads the finalized head of the RPC endpoint pinned to ctx;
// a nil header or number is reported as an error.
func (r *Relayer) finalizedHeader(ctx context.Context) (*types.Header, error) {
	header, err := r.eth(ctx).HeaderByNumber(ctx, finalizedBlock())
	if err != nil {
		return nil, fmt.Errorf("finalized block unavailable: %w", err)
	}
	if header == nil || header.Number == nil {
		return nil, errors.New("finalized block unavailable: empty response")
	}
	return header, nil
}

// withdrawReceiptOK reports whether hash is this exact withdraw and succeeded.
// reverted is true for a mined matching transaction with status 0.
func (r *Relayer) withdrawReceiptOK(ctx context.Context, hash common.Hash, pool common.Address, data []byte) (confirmed, reverted bool, err error) {
	if hash == (common.Hash{}) {
		return false, false, ethereum.NotFound
	}
	receipt, err := r.eth(ctx).TransactionReceipt(ctx, hash)
	if err != nil {
		return false, false, err
	}
	tx, _, err := r.eth(ctx).TransactionByHash(ctx, hash)
	if err != nil {
		return false, false, err
	}
	if receipt.TxHash != hash || tx.Hash() != hash {
		return false, false, errors.New("RPC returned a transaction or receipt for a different hash")
	}
	if tx.To() == nil || *tx.To() != pool || !bytes.Equal(tx.Data(), data) {
		return false, false, nil
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return false, true, nil
	}
	return true, false, nil
}

// receiptFinalized reports whether hash is included at or below the finalized
// block of HTTP_RPC_URL, and whether it succeeded there. Height alone is not
// proof: an RPC gateway may return a cached orphan receipt. Its block hash
// must also match the canonical header at that height.
func (r *Relayer) receiptFinalized(ctx context.Context, hash common.Hash) (final, succeeded bool, err error) {
	finalized, err := r.finalizedHeader(ctx)
	if err != nil {
		return false, false, err
	}
	receipt, err := r.eth(ctx).TransactionReceipt(ctx, hash)
	if errors.Is(err, ethereum.NotFound) {
		return false, false, nil // reorganized out since the first lookup
	}
	if err != nil {
		return false, false, err
	}
	if receipt.TxHash != hash {
		return false, false, errors.New("RPC returned a receipt for a different hash")
	}
	if receipt.BlockNumber == nil || receipt.BlockNumber.Sign() < 0 || receipt.BlockNumber.Cmp(finalized.Number) > 0 {
		return false, false, nil
	}
	canonical := finalized
	if receipt.BlockNumber.Cmp(finalized.Number) != 0 {
		canonical, err = r.eth(ctx).HeaderByNumber(ctx, receipt.BlockNumber)
		if err != nil {
			return false, false, fmt.Errorf("canonical receipt block unavailable: %w", err)
		}
	}
	if canonical == nil || canonical.Number == nil || canonical.Number.Cmp(receipt.BlockNumber) != 0 {
		return false, false, errors.New("canonical receipt block unavailable or has unexpected height")
	}
	if receipt.BlockHash != canonical.Hash() {
		return false, false, nil // orphan receipt or inconsistent RPC view
	}
	if receipt.Status != types.ReceiptStatusSuccessful && receipt.Status != types.ReceiptStatusFailed {
		return false, false, errors.New("receipt has an invalid status")
	}
	return true, receipt.Status == types.ReceiptStatusSuccessful, nil
}

// nullifierSpent reads isSpent at block (nil = latest) on the job's pool.
func (r *Relayer) nullifierSpent(ctx context.Context, job *Job, block *big.Int) (bool, error) {
	return r.poolBool(ctx, job, "isSpent", argNullifierHash, block)
}

// rootKnown reads isKnownRoot(_root) at latest on the job's pool.
func (r *Relayer) rootKnown(ctx context.Context, job *Job) (bool, error) {
	return r.poolBool(ctx, job, "isKnownRoot", argRoot, nil)
}

// poolBool calls a bool view of the job's pool with stored arg argIndex
// ([root, nullifier, ...]) at block (nil = latest).
func (r *Relayer) poolBool(ctx context.Context, job *Job, method string, argIndex int, block *big.Int) (bool, error) {
	req, pool, err := storedRequest(job)
	if err != nil {
		return false, err
	}
	call, err := r.poolABI.Pack(method, common.HexToHash(req.Args[argIndex]))
	if err != nil {
		return false, err
	}
	result, err := r.eth(ctx).CallContract(ctx, ethereum.CallMsg{To: &pool, Data: call}, block)
	if err != nil {
		return false, err
	}
	return r.unpackPoolBool(method, result)
}

// jobCall rebuilds the exact pool target and calldata of a stored job from the
// request persisted at admission (already validated then). It deliberately
// does not re-apply the current admission policy (REWARD_ACCOUNT, configured
// pools): a transaction submitted under an earlier configuration must still be
// recognized, or its nullifier could be released while it can still execute.
func (r *Relayer) jobCall(job *Job) (common.Address, []byte, error) {
	req, pool, err := storedRequest(job)
	if err != nil {
		return common.Address{}, nil, err
	}
	data, _, err := r.withdrawCallData(req)
	if err != nil {
		return common.Address{}, nil, err
	}
	return pool, data, nil
}

// storedRequest canonicalizes a persisted request and checks the fields its
// pool calls are built from; the result has withdrawArgCount args.
func storedRequest(job *Job) (WithdrawRequest, common.Address, error) {
	req, err := canonicalWithdrawRequest(job.Request)
	if err != nil {
		return WithdrawRequest{}, common.Address{}, err
	}
	if !addressRe.MatchString(req.Contract) {
		return WithdrawRequest{}, common.Address{}, errors.New("stored .contract is not an Ethereum address")
	}
	if !proofRe.MatchString(req.Proof) {
		return WithdrawRequest{}, common.Address{}, errors.New("stored .proof is malformed")
	}
	return req, common.HexToAddress(req.Contract), nil
}

// jobTxHashes returns every distinct hash recorded for the job, oldest first.
func jobTxHashes(job *Job) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(job.TxHashes)+1)
	for _, hash := range slices.Concat(job.TxHashes, []string{job.TxHash}) {
		if hash == "" || seen[hash] {
			continue
		}
		seen[hash] = true
		out = append(out, hash)
	}
	return out
}

// claimJob gives the caller exclusive ownership of jobID within this process;
// it reports false when another worker already holds it.
func (r *Relayer) claimJob(jobID string) bool {
	_, held := r.claimed.LoadOrStore(jobID, struct{}{})
	return !held
}

// releaseJob drops the claim taken by claimJob.
func (r *Relayer) releaseJob(jobID string) {
	r.claimed.Delete(jobID)
}
