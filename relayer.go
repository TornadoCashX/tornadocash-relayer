package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Relayer owns the HTTP admission path, the preflight/submit workers and the
// background reconciler, health monitor and price watcher. Fields are grouped
// by concern; each group is guarded by the mutex declared at its top.
type Relayer struct {
	// Immutable dependencies, set by NewRelayer.
	cfg        Config
	chain      *rpcPool // read-only RPCs for preflight and chain verification
	oracle     *rpcPool
	ownsOracle bool
	oz         *ozClient
	poolABI    abi.ABI
	oracleABI  abi.ABI
	store      JobStore

	// Job ownership and worker lifecycle.
	// claimed: job id -> struct{} while a worker or reconciler owns the job.
	claimed sync.Map
	// preflightNotBefore: job id -> earliest next preflight of a deferred job.
	preflightNotBefore sync.Map
	// lifecycleMu orders the stopping check in startWorker with Stop, so no
	// worker can be added to workerWG once shutdown has begun waiting.
	lifecycleMu    sync.Mutex
	workerWG       sync.WaitGroup
	preflightSlots chan struct{}

	// Oracle price cache.
	priceMu sync.RWMutex
	prices  map[string]oraclePrice // replaced as a whole by refreshPrices

	// Readiness and store statistics caches.
	readinessMu       sync.RWMutex
	cachedReadyErrors []string
	readinessCachedAt atomic.Int64 // unix seconds of the last readiness refresh
	statsMu           sync.Mutex
	cachedStats       StoreStats
	statsCachedAt     time.Time

	// OZ relayer state, refreshed by verification and the health monitor.
	ozMu          sync.RWMutex
	ozAddress     common.Address // sender address, read from OZ, never configured locally
	ozStatus      *ozRelayerStatus
	ozStatusAt    time.Time // when ozStatus was last read from OZ
	ozGasPriceCap *big.Int  // OZ policy gas_price_cap, <= MaxGasPriceWei
	ozVerifyErr   error     // last periodic re-verification failure
	ozVerifiedAt  time.Time

	// Heartbeats and metrics counters.
	reconcilerHeartbeat atomic.Int64
	reconcileRound      atomic.Uint64 // rotates the start of each reconcile round
	oracleHeartbeat     atomic.Int64
	httpRequests        atomic.Uint64
	rateLimitRejections atomic.Uint64
	ozRequestFailures   atomic.Uint64
	reconcilerNanos     atomic.Int64
	workersActive       atomic.Int64

	// startupPending is set until the OZ relayer has been verified. Admission
	// and readiness are refused meanwhile.
	startupPending atomic.Bool
	stopping       atomic.Bool
	ctx            context.Context
	cancel         context.CancelFunc
	rateLimiter    *ipRateLimiter
}

const (
	startupValidationTimeout = 30 * time.Second
	// statsCacheTTL bounds how often /status and /metrics hit the store.
	statsCacheTTL = 2 * time.Second
)

// NewRelayer dials and validates the RPC endpoints, parses the contract ABIs
// and opens the job store. Every opened resource is closed again on failure.
func NewRelayer(cfg Config) (*Relayer, error) {
	chain, oracle, ownsOracle, err := dialRPCClients(cfg)
	if err != nil {
		return nil, err
	}
	closeClients := func() {
		chain.Close()
		if ownsOracle {
			oracle.Close()
		}
	}
	if err := validateStartupRPCs(cfg, chain, oracle, ownsOracle); err != nil {
		closeClients()
		return nil, err
	}
	poolABI, oracleABI, err := parseRelayerABIs()
	if err != nil {
		closeClients()
		return nil, err
	}
	store, err := NewSQLiteStore(cfg.SQLitePath, cfg.ChainID.String(), cfg.IdempotencyTTL, cfg.MaxActiveJobs, cfg.MaxProtectedJobs)
	if err != nil {
		closeClients()
		return nil, err
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	relayer := &Relayer{
		cfg:            cfg,
		chain:          chain,
		oracle:         oracle,
		ownsOracle:     ownsOracle,
		oz:             newOZClient(cfg),
		poolABI:        poolABI,
		oracleABI:      oracleABI,
		store:          store,
		preflightSlots: make(chan struct{}, cfg.MaxPreflightConcurrency),
		ctx:            rootCtx,
		cancel:         cancel,
		rateLimiter:    newIPRateLimiter(cfg.RelayRatePerMinute, cfg.RelayRateBurst, cfg.RelayRateMaxClients),
	}
	relayer.reconcilerHeartbeat.Store(time.Now().Unix())
	return relayer, nil
}

// dialRPCClients dials the chain pool and, when oracle prices are configured,
// the oracle pool. On mainnet without ORACLE_RPC_URL the chain pool is shared
// (ownsOracle=false).
func dialRPCClients(cfg Config) (chain, oracle *rpcPool, ownsOracle bool, err error) {
	names, urls := cfg.rpcEndpoints()
	chain, err = dialRPCPool(names, urls)
	if err != nil {
		return nil, nil, false, err
	}
	if len(cfg.OraclePriceTokens) == 0 {
		return chain, nil, false, nil
	}
	if cfg.OracleRPCURL == "" && cfg.ChainID.Uint64() == chainIDEthereumMainnet {
		return chain, chain, false, nil
	}
	oracle, err = dialRPCPool([]string{"ORACLE_RPC_URL"}, []string{cfg.OracleRPCURL})
	if err != nil {
		chain.Close()
		return nil, nil, false, err
	}
	return chain, oracle, true, nil
}

// validateStartupRPCs checks the chain pool and a dedicated oracle endpoint
// within one startupValidationTimeout.
func validateStartupRPCs(cfg Config, chain, oracle *rpcPool, ownsOracle bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), startupValidationTimeout)
	defer cancel()
	if err := validateRPCPool(ctx, chain, cfg); err != nil {
		return err
	}
	if !ownsOracle {
		return nil
	}
	// dialRPCPool never returns a pool without endpoints.
	return validateOracleRPC(ctx, oracle.endpoints[0].client)
}

// parseRelayerABIs parses the embedded Tornado pool and offchain oracle ABIs.
func parseRelayerABIs() (poolABI, oracleABI abi.ABI, err error) {
	poolABI, err = abi.JSON(strings.NewReader(tornadoPoolABI))
	if err != nil {
		return abi.ABI{}, abi.ABI{}, err
	}
	oracleABI, err = abi.JSON(strings.NewReader(offchainOracleABI))
	if err != nil {
		return abi.ABI{}, abi.ABI{}, err
	}
	return poolABI, oracleABI, nil
}

// senderAddress is the OZ relayer address used as msg.sender in preflight.
func (r *Relayer) senderAddress() common.Address {
	r.ozMu.RLock()
	defer r.ozMu.RUnlock()
	return r.ozAddress
}

// maxGasPrice is the per-gas ceiling for submitted transactions: the lower of
// maxGasPriceGwei and the OZ relayer gas_price_cap, so OZ never has to clamp
// (and thereby underprice) a request.
func (r *Relayer) maxGasPrice() *big.Int {
	r.ozMu.RLock()
	defer r.ozMu.RUnlock()
	limit := r.cfg.MaxGasPriceWei // > 0, enforced by validateGasConfig
	if r.ozGasPriceCap != nil && r.ozGasPriceCap.Cmp(limit) < 0 {
		limit = r.ozGasPriceCap
	}
	return new(big.Int).Set(limit)
}

// ozAdmissionError explains why new withdrawals must not be accepted right
// now because OZ could not sign them; "" means OZ is usable.
func (r *Relayer) ozAdmissionError() string {
	if r.stopping.Load() {
		return "relayer is shutting down; retry later"
	}
	if r.startupPending.Load() {
		return "relayer is starting; retry shortly"
	}
	// Submitted jobs are only advanced (and nullifiers released) by the
	// reconciler; do not admit work it would never finish.
	if heartbeatStale(r.reconcilerHeartbeat.Load(), r.statusStaleAfter()) {
		return "relayer transaction reconciler is not running; retry later"
	}
	r.ozMu.RLock()
	defer r.ozMu.RUnlock()
	if r.ozVerifyErr != nil {
		return "relayer transaction backend is misconfigured; retry later"
	}
	// A status that has not been refreshed for a while says nothing about OZ now.
	if r.ozStatus == nil || time.Since(r.ozStatusAt) > r.statusStaleAfter() {
		return "relayer transaction backend is unavailable; retry later"
	}
	if r.ozStatus.Paused || r.ozStatus.SystemDisabled {
		return "relayer transaction backend is paused; retry later"
	}
	return ""
}

// cachedOZStatus returns the last OZ status read; nil means unavailable.
func (r *Relayer) cachedOZStatus() *ozRelayerStatus {
	r.ozMu.RLock()
	defer r.ozMu.RUnlock()
	return r.ozStatus
}

// errStartupRPCConfig marks an endpoint that answers but is configured for the
// wrong network; that is fatal even when another endpoint is fine.
var errStartupRPCConfig = errors.New("RPC endpoint is misconfigured")

// validateRPCPool requires every reachable endpoint to serve NET_ID with
// EIP-1559, and at least one endpoint to be reachable. An unreachable endpoint
// only starts marked down, so a dead backup does not block startup.
func validateRPCPool(ctx context.Context, pool *rpcPool, cfg Config) error {
	reachable := 0
	var lastErr error
	for _, endpoint := range pool.endpoints {
		err := validateRPCNetwork(ctx, endpoint.client, cfg)
		switch {
		case err == nil:
			reachable++
		case errors.Is(err, errStartupRPCConfig):
			return fmt.Errorf("%s: %w", endpoint.name, err)
		default:
			endpoint.markDown(err.Error())
			logWarnf("%s unavailable at startup; continuing with the other endpoint: %v", endpoint.name, redactSensitive(err.Error()))
			lastErr = fmt.Errorf("%s: %w", endpoint.name, err)
		}
	}
	if reachable == 0 {
		return lastErr
	}
	return nil
}

// validateRPCNetwork requires one endpoint to serve NET_ID with EIP-1559.
func validateRPCNetwork(ctx context.Context, client *ethclient.Client, cfg Config) error {
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("startup RPC chain id: %w", err)
	}
	if cfg.ChainID == nil || chainID.Cmp(cfg.ChainID) != 0 {
		return fmt.Errorf("%w: startup RPC chain id mismatch: rpc=%s config=%v", errStartupRPCConfig, chainID, cfg.ChainID)
	}
	header, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return fmt.Errorf("startup latest block unavailable: %w", err)
	}
	if header == nil {
		return fmt.Errorf("startup latest block unavailable: empty response")
	}
	if header.BaseFee == nil {
		return fmt.Errorf("%w: startup target network does not support EIP-1559 type-2 transactions", errStartupRPCConfig)
	}
	return nil
}

// validateOracleRPC requires the dedicated oracle endpoint to serve mainnet.
func validateOracleRPC(ctx context.Context, client *ethclient.Client) error {
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("startup oracle RPC chain id: %w", err)
	}
	if chainID.Cmp(big.NewInt(chainIDEthereumMainnet)) != 0 {
		return fmt.Errorf("startup oracle RPC must use Ethereum mainnet: rpc=%s", chainID)
	}
	return nil
}

// Close stops the relayer and releases RPC clients and the job store. Callers
// must first wait for workers (see Wait); main refuses to close otherwise.
func (r *Relayer) Close() error {
	r.Stop()
	r.chain.Close()
	if r.ownsOracle {
		r.oracle.Close()
	}
	return r.store.Close()
}

// Stop closes admission and cancels the runtime context. It is idempotent.
func (r *Relayer) Stop() {
	r.lifecycleMu.Lock()
	r.stopping.Store(true)
	r.lifecycleMu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
}

// runtimeContext is cancelled by Stop; tests without NewRelayer get Background.
func (r *Relayer) runtimeContext() context.Context {
	if r.ctx != nil {
		return r.ctx
	}
	return context.Background()
}

// startWorker runs work for jobID under the job claim and JobTimeout. The
// stopping check and workerWG.Add happen under lifecycleMu, so no worker is
// registered after Stop returned (sync.WaitGroup forbids Add racing Wait).
func (r *Relayer) startWorker(jobID string, work func(context.Context)) {
	if r.stopping.Load() || !r.claimJob(jobID) {
		return
	}
	r.lifecycleMu.Lock()
	if r.stopping.Load() {
		r.lifecycleMu.Unlock()
		r.releaseJob(jobID)
		return
	}
	r.workerWG.Add(1)
	r.workersActive.Add(1)
	r.lifecycleMu.Unlock()
	go func() {
		defer r.workerWG.Done()
		defer r.workersActive.Add(-1)
		defer r.releaseJob(jobID)
		workerCtx, workerCancel := context.WithTimeout(r.runtimeContext(), r.cfg.JobTimeout)
		defer workerCancel()
		work(workerCtx)
	}()
}

// storeStats returns store statistics cached for statsCacheTTL.
func (r *Relayer) storeStats(ctx context.Context) (StoreStats, error) {
	r.statsMu.Lock()
	defer r.statsMu.Unlock()
	if !r.statsCachedAt.IsZero() && time.Since(r.statsCachedAt) < statsCacheTTL {
		return r.cachedStats, nil
	}
	stats, err := r.store.Stats(ctx)
	if err != nil {
		return StoreStats{}, err
	}
	r.cachedStats = stats
	r.statsCachedAt = time.Now()
	return stats, nil
}

// Wait blocks until every background goroutine and worker has returned, or
// ctx ends. On timeout the helper goroutine is left behind on purpose: the
// process is about to exit.
func (r *Relayer) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		r.workerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
