package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// rpcRequestTimeout bounds every read-only JSON-RPC HTTP request so a hung
// node cannot hold preflight slots until the job timeout.
var rpcRequestTimeout = 10 * time.Second

const (
	// rpcDownCooldown: an endpoint that failed is skipped for this long unless
	// a later health probe succeeds first. When every endpoint is down they are
	// still tried, so a cooldown never makes the relayer refuse to read.
	rpcDownCooldown = 30 * time.Second
	// rpcMaxLagBlocks: an endpoint whose head is further behind the best
	// endpoint is treated as down (a stalled node answers but serves old state).
	rpcMaxLagBlocks = 10
	// rpcProbeTimeout bounds one endpoint's health probe; readiness callers
	// allow 5s in total.
	rpcProbeTimeout = 3 * time.Second
	// rpcSlowAfter: a request that runs out of time after at least this long
	// counts against the endpoint (a healthy node answers far sooner), even
	// though the deadline lives in the request context; a caller whose own
	// budget was nearly spent does not.
	rpcSlowAfter = 3 * time.Second
)

func validateRPCURL(name, raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return errors.New(name + " must be a valid http(s) URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return nil
	default:
		return errors.New(name + " must use http or https")
	}
}

func dialRPC(name, raw string, transport http.RoundTripper) (*ethclient.Client, error) {
	if err := validateRPCURL(name, raw); err != nil {
		return nil, err
	}
	httpClient := &http.Client{Timeout: rpcRequestTimeout, Transport: transport}
	client, err := rpc.DialOptions(context.Background(), raw, rpc.WithHTTPClient(httpClient))
	if err != nil {
		return nil, err
	}
	return ethclient.NewClient(client), nil
}

// rpcEndpoint is one read-only node. name is the environment variable, never
// the URL: RPC URLs commonly embed API keys.
type rpcEndpoint struct {
	name      string
	client    *ethclient.Client
	downUntil atomic.Int64 // unix nanoseconds; 0 = healthy

	headerMu sync.Mutex // guards the latestHeader cache
	header   *types.Header
	headerAt time.Time
}

func (e *rpcEndpoint) healthy(now time.Time) bool {
	return now.UnixNano() >= e.downUntil.Load()
}

func (e *rpcEndpoint) markDown(reason string) {
	if e.healthy(time.Now()) {
		logWarnf("RPC endpoint %s marked down for %s: %s", e.name, rpcDownCooldown, redactSensitive(reason))
	}
	e.downUntil.Store(time.Now().Add(rpcDownCooldown).UnixNano())
}

func (e *rpcEndpoint) markUp() {
	if e.downUntil.Swap(0) != 0 {
		logInfof("RPC endpoint %s is healthy again", e.name)
	}
}

// endpointHealthTransport marks its endpoint down on transport failures and
// on HTTP 429/5xx, so the next operation moves to another endpoint without
// waiting for the periodic probe.
type endpointHealthTransport struct {
	endpoint *rpcEndpoint
}

func (t *endpointHealthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	started := time.Now()
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		// A canceled caller is not the endpoint's fault. A deadline is: the
		// client timeout (rpcRequestTimeout) is enforced through the request
		// context, so a hung node would otherwise never be marked down.
		switch ctxErr := req.Context().Err(); {
		case ctxErr == nil:
			t.endpoint.markDown(err.Error())
		case errors.Is(ctxErr, context.DeadlineExceeded) && time.Since(started) >= rpcSlowAfter:
			t.endpoint.markDown(fmt.Sprintf("no response within %s", time.Since(started).Round(time.Second)))
		}
		return resp, err
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		t.endpoint.markDown(fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
	return resp, err
}

// rpcPool holds the read-only chain endpoints (the HTTP_RPC_URL list).
//
// Operations rotate across healthy endpoints, but every read of a single
// operation (one preflight, one job reconciliation) is pinned to one endpoint.
// Safety decisions compare several reads (receipt vs canonical header, pending
// vs finalized), and mixing two nodes' views in one decision is exactly the
// inconsistency the finality checks guard against.
type rpcPool struct {
	endpoints []*rpcEndpoint
	cursor    atomic.Uint64
	probeMu   sync.Mutex
}

// dialRPCPool dials every configured URL; names[i] labels urls[i].
func dialRPCPool(names, urls []string) (*rpcPool, error) {
	pool := &rpcPool{}
	for i, raw := range urls {
		endpoint := &rpcEndpoint{name: names[i]}
		client, err := dialRPC(names[i], raw, &endpointHealthTransport{endpoint: endpoint})
		if err != nil {
			pool.Close()
			return nil, err
		}
		endpoint.client = client
		pool.endpoints = append(pool.endpoints, endpoint)
	}
	if len(pool.endpoints) == 0 {
		return nil, errors.New("no RPC endpoint configured")
	}
	return pool, nil
}

func (p *rpcPool) Close() {
	if p == nil {
		return
	}
	for _, endpoint := range p.endpoints {
		if endpoint.client != nil {
			endpoint.client.Close()
		}
	}
}

// order lists every endpoint, healthy ones first, starting at the rotation
// position start. Unhealthy endpoints stay at the end as a last resort.
func (p *rpcPool) order(start uint64) []*rpcEndpoint {
	n := len(p.endpoints)
	healthy := make([]*rpcEndpoint, 0, n)
	var down []*rpcEndpoint
	now := time.Now()
	for i := 0; i < n; i++ {
		endpoint := p.endpoints[(start+uint64(i))%uint64(n)]
		if endpoint.healthy(now) {
			healthy = append(healthy, endpoint)
		} else {
			down = append(down, endpoint)
		}
	}
	return append(healthy, down...)
}

// rotation advances the round-robin cursor once per operation and returns the
// endpoints to try for it, preferred first.
func (p *rpcPool) rotation() []*rpcEndpoint {
	return p.order(p.cursor.Add(1) - 1)
}

// preferred is the endpoint used by reads that are not pinned to an
// operation. It does not advance the cursor.
func (p *rpcPool) preferred() *rpcEndpoint {
	return p.order(p.cursor.Load())[0]
}

type rpcEndpointKey struct{}

func withRPCEndpoint(ctx context.Context, endpoint *rpcEndpoint) context.Context {
	return context.WithValue(ctx, rpcEndpointKey{}, endpoint)
}

// client returns the endpoint pinned to ctx when it belongs to this pool, or
// the currently preferred one.
func (p *rpcPool) client(ctx context.Context) *ethclient.Client {
	return p.endpoint(ctx).client
}

// endpoint is the endpoint that client(ctx) reads from.
func (p *rpcPool) endpoint(ctx context.Context) *rpcEndpoint {
	if endpoint := p.pinned(ctx); endpoint != nil {
		return endpoint
	}
	return p.preferred()
}

// pinned returns this pool's endpoint pinned to ctx, if any.
func (p *rpcPool) pinned(ctx context.Context) *rpcEndpoint {
	endpoint, _ := ctx.Value(rpcEndpointKey{}).(*rpcEndpoint)
	for _, own := range p.endpoints {
		if endpoint != nil && own == endpoint {
			return endpoint
		}
	}
	return nil
}

// eth is the chain RPC for ctx (see rpcPool.client).
func (r *Relayer) eth(ctx context.Context) *ethclient.Client {
	return r.chain.client(ctx)
}

// pinRPC pins ctx to the next endpoint in rotation for a single operation.
func (r *Relayer) pinRPC(ctx context.Context) context.Context {
	if r.chain.pinned(ctx) != nil {
		return ctx
	}
	return withRPCEndpoint(ctx, r.chain.rotation()[0])
}

// rpcHealth is one endpoint's probe result.
type rpcHealth struct {
	name   string
	err    error
	number uint64
}

// probe checks every endpoint (chain id, latest head, lag behind the best
// head), updates their health and reports the results. A wrong chain id is a
// configuration error and is reported separately by the caller.
func (p *rpcPool) probe(ctx context.Context, chainID *big.Int) []rpcHealth {
	p.probeMu.Lock()
	defer p.probeMu.Unlock()
	results := make([]rpcHealth, len(p.endpoints))
	var wg sync.WaitGroup
	for i, endpoint := range p.endpoints {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each endpoint gets its own deadline, shorter than any caller's,
			// so a hung node fails its probe (and is marked down) instead of
			// running the shared context out.
			probeCtx, cancel := context.WithTimeout(ctx, rpcProbeTimeout)
			defer cancel()
			results[i] = probeEndpoint(probeCtx, endpoint, chainID)
		}()
	}
	wg.Wait()
	var best uint64
	for _, result := range results {
		if result.err == nil && result.number > best {
			best = result.number
		}
	}
	for i, endpoint := range p.endpoints {
		if results[i].err == nil && best > results[i].number+rpcMaxLagBlocks {
			results[i].err = fmt.Errorf("head %d is %d blocks behind the best endpoint", results[i].number, best-results[i].number)
		}
		if results[i].err != nil {
			if !errors.Is(ctx.Err(), context.Canceled) {
				endpoint.markDown(results[i].err.Error())
			}
			continue
		}
		endpoint.markUp()
	}
	return results
}

var errRPCChainMismatch = errors.New("rpc chain id mismatch")

func probeEndpoint(ctx context.Context, endpoint *rpcEndpoint, chainID *big.Int) rpcHealth {
	result := rpcHealth{name: endpoint.name}
	got, err := endpoint.client.ChainID(ctx)
	if err != nil {
		result.err = fmt.Errorf("chain id: %w", err)
		return result
	}
	if chainID != nil && got.Cmp(chainID) != 0 {
		result.err = fmt.Errorf("%w: rpc=%s config=%s", errRPCChainMismatch, got, chainID)
		return result
	}
	header, err := endpoint.client.HeaderByNumber(ctx, nil)
	if err != nil {
		result.err = fmt.Errorf("latest block: %w", err)
		return result
	}
	if header == nil || header.Number == nil {
		result.err = errors.New("latest block: empty response")
		return result
	}
	result.number = header.Number.Uint64()
	return result
}

// headerCacheTTL is how long latestHeader reuses an endpoint's head.
const headerCacheTTL = 2 * time.Second

// latestHeader caches the head for headerCacheTTL; fee planning only needs the
// current base fee. The cache is per endpoint, so a preflight pinned to one
// endpoint never takes its base fee from the other. headerMu is held across
// the fetch on purpose: concurrent callers wait for one request and then share
// its result instead of all hitting the RPC. Callers get a copy, so the cached
// header cannot be mutated through the return value.
func (r *Relayer) latestHeader(ctx context.Context) (*types.Header, error) {
	endpoint := r.chain.endpoint(ctx)
	endpoint.headerMu.Lock()
	defer endpoint.headerMu.Unlock()
	if endpoint.header != nil && time.Since(endpoint.headerAt) < headerCacheTTL {
		return types.CopyHeader(endpoint.header), nil
	}
	header, err := endpoint.client.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, err
	}
	if header == nil || header.Number == nil {
		return nil, errors.New("latest header is empty")
	}
	endpoint.header, endpoint.headerAt = header, time.Now()
	return types.CopyHeader(header), nil
}
