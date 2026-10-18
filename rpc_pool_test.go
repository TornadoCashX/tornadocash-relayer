package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// singleRPC wraps one test client as the chain pool.
func singleRPC(client *ethclient.Client) *rpcPool {
	return &rpcPool{endpoints: []*rpcEndpoint{{name: "HTTP_RPC_URL", client: client}}}
}

// fakeNode is a minimal JSON-RPC node (single calls and batches).
type fakeNode struct {
	chainID  atomic.Uint64
	head     atomic.Uint64
	status   int // non-zero: answer every HTTP request with this status
	spent    bool
	unknown  bool // isKnownRoot answers false (a node lagging behind the root)
	requests atomic.Int64
	poolABI  abi.ABI
}

func (n *fakeNode) answer(method string, params []json.RawMessage) any {
	word := func(v bool) string {
		out := make([]byte, 32)
		if v {
			out[31] = 1
		}
		return hexutil.Encode(out)
	}
	switch method {
	case "eth_chainId":
		return hexutil.EncodeUint64(n.chainID.Load())
	case "eth_getBlockByNumber":
		return &types.Header{Number: new(big.Int).SetUint64(n.head.Load()), Difficulty: new(big.Int), BaseFee: big.NewInt(1)}
	case "eth_getCode":
		return "0x"
	case "eth_call":
		var call struct {
			Input string `json:"input"`
			Data  string `json:"data"`
		}
		_ = json.Unmarshal(params[0], &call)
		input := call.Input + call.Data
		selector := func(m string) string { return hexutil.Encode(n.poolABI.Methods[m].ID) }
		switch {
		case strings.HasPrefix(input, selector("denomination")):
			return hexutil.Encode(common.LeftPadBytes(big.NewInt(1e17).Bytes(), 32))
		case strings.HasPrefix(input, selector("isKnownRoot")):
			return word(!n.unknown)
		case strings.HasPrefix(input, selector("isSpent")):
			return word(n.spent)
		}
		return "0x"
	}
	return nil
}

func (n *fakeNode) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	n.requests.Add(1)
	if n.status != 0 {
		http.Error(w, "unavailable", n.status)
		return
	}
	type call struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	body, _ := io.ReadAll(req.Body)
	reply := func(c call) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": c.ID, "result": n.answer(c.Method, c.Params)}
	}
	w.Header().Set("Content-Type", "application/json")
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
		var calls []call
		_ = json.Unmarshal(body, &calls)
		out := make([]map[string]any, 0, len(calls))
		for _, c := range calls {
			out = append(out, reply(c))
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	var c call
	_ = json.Unmarshal(body, &c)
	_ = json.NewEncoder(w).Encode(reply(c))
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	poolABI, err := abi.JSON(strings.NewReader(tornadoPoolABI))
	if err != nil {
		t.Fatal(err)
	}
	n := &fakeNode{poolABI: poolABI}
	n.chainID.Store(testChainID.Uint64())
	n.head.Store(100)
	return n
}

// testRPCPool builds a pool over the given handlers (nil = an address nothing
// listens on), with the production health transport.
func testRPCPool(t *testing.T, handlers ...http.Handler) *rpcPool {
	t.Helper()
	names, urls := []string{"HTTP_RPC_URL[1]", "HTTP_RPC_URL[2]"}, make([]string, len(handlers))
	for i, handler := range handlers {
		if handler == nil {
			server := httptest.NewServer(http.NotFoundHandler())
			server.Close() // connection refused: a dead endpoint
			urls[i] = server.URL
			continue
		}
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		urls[i] = server.URL
	}
	pool, err := dialRPCPool(names[:len(urls)], urls)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestRPCPoolRotatesHealthyEndpoints(t *testing.T) {
	pool := testRPCPool(t, newFakeNode(t), newFakeNode(t))
	var got []string
	for i := 0; i < 4; i++ {
		got = append(got, pool.rotation()[0].name)
	}
	if strings.Join(got, ",") != "HTTP_RPC_URL[1],HTTP_RPC_URL[2],HTTP_RPC_URL[1],HTTP_RPC_URL[2]" {
		t.Fatalf("operations must alternate endpoints, got %v", got)
	}

	pool.endpoints[0].markDown("test")
	for i := 0; i < 3; i++ {
		if order := pool.rotation(); order[0].name != "HTTP_RPC_URL[2]" || len(order) != 2 {
			t.Fatalf("a down endpoint must move to the end, got %s first", order[0].name)
		}
	}
	pool.endpoints[1].markDown("test")
	if order := pool.rotation(); len(order) != 2 {
		t.Fatal("with every endpoint down both must still be tried")
	}
}

func TestRPCTransportMarksEndpointDown(t *testing.T) {
	failing := newFakeNode(t)
	failing.status = http.StatusBadGateway
	pool := testRPCPool(t, failing, nil)
	for _, endpoint := range pool.endpoints {
		_, _ = endpoint.client.ChainID(context.Background())
	}
	for _, endpoint := range pool.endpoints {
		if endpoint.healthy(time.Now()) {
			t.Fatalf("%s must be marked down after HTTP 502 / connection refused", endpoint.name)
		}
	}
}

func TestRPCProbeMarksDeadLaggingAndWrongChain(t *testing.T) {
	good, lagging := newFakeNode(t), newFakeNode(t)
	lagging.head.Store(good.head.Load() - rpcMaxLagBlocks - 1)
	pool := testRPCPool(t, good, lagging)
	results := pool.probe(context.Background(), testChainID)
	if results[0].err != nil || results[1].err == nil || pool.endpoints[1].healthy(time.Now()) {
		t.Fatalf("lagging endpoint must be marked down: %+v", results)
	}
	lagging.head.Store(good.head.Load())
	if results = pool.probe(context.Background(), testChainID); results[1].err != nil || !pool.endpoints[1].healthy(time.Now()) {
		t.Fatalf("caught-up endpoint must be healthy again: %+v", results)
	}
	lagging.chainID.Store(999)
	if results = pool.probe(context.Background(), testChainID); !errors.Is(results[1].err, errRPCChainMismatch) {
		t.Fatalf("wrong chain must be reported as a mismatch: %+v", results)
	}
}

func TestReadinessNeedsOnlyOneHealthyEndpoint(t *testing.T) {
	r := &Relayer{cfg: Config{ChainID: testChainID}, chain: testRPCPool(t, newFakeNode(t), nil)}
	for _, message := range r.rpcReadinessErrors(context.Background(), nil) {
		if strings.HasPrefix(message, "rpc") {
			t.Fatalf("one healthy endpoint is enough, got %q", message)
		}
	}
	r.chain = testRPCPool(t, nil, nil)
	if errs := r.rpcReadinessErrors(context.Background(), nil); !slices.Contains(errs, "rpc unavailable") {
		t.Fatalf("all endpoints down must be reported, got %v", errs)
	}
	wrong := newFakeNode(t)
	wrong.chainID.Store(999)
	r.chain = testRPCPool(t, newFakeNode(t), wrong)
	if errs := r.rpcReadinessErrors(context.Background(), nil); !slices.Contains(errs, "rpc chain id mismatch") {
		t.Fatalf("a wrong-chain backup is a configuration error, got %v", errs)
	}
}

func TestStartupToleratesDeadBackupButNotWrongChain(t *testing.T) {
	cfg := Config{ChainID: testChainID}
	if err := validateRPCPool(context.Background(), testRPCPool(t, newFakeNode(t), nil), cfg); err != nil {
		t.Fatalf("a dead backup must not block startup: %v", err)
	}
	if err := validateRPCPool(context.Background(), testRPCPool(t, nil, nil), cfg); err == nil {
		t.Fatal("startup needs at least one reachable endpoint")
	}
	wrong := newFakeNode(t)
	wrong.chainID.Store(999)
	if err := validateRPCPool(context.Background(), testRPCPool(t, newFakeNode(t), wrong), cfg); err == nil || !strings.Contains(err.Error(), "HTTP_RPC_URL[2]") {
		t.Fatalf("a wrong-chain endpoint must stop startup, got %v", err)
	}
}

func TestRPCURLConfig(t *testing.T) {
	for raw, want := range map[string][]string{
		"https://rpc-a.invalid":                                {"https://rpc-a.invalid"},
		`["https://rpc-a.invalid"]`:                            {"https://rpc-a.invalid"},
		` ["https://rpc-a.invalid", " https://rpc-b.invalid"]`: {"https://rpc-a.invalid", "https://rpc-b.invalid"},
	} {
		got, err := parseRPCURLs(raw)
		if err != nil || strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("parse %q = %v, %v; want %v", raw, got, err, want)
		}
	}
	if _, err := parseRPCURLs(`["https://rpc-a.invalid",`); err == nil {
		t.Fatal("malformed JSON must be rejected")
	}

	cfg := productionRuntimeConfig()
	if names, _ := cfg.rpcEndpoints(); strings.Join(names, ",") != "HTTP_RPC_URL" {
		t.Fatalf("one URL keeps the plain name, got %v", names)
	}
	cfg.RPCURLs = []string{"https://rpc-a.invalid", "https://rpc-b.invalid"}
	if err := validateRuntimeConfig(cfg); err != nil {
		t.Fatalf("two valid RPCs rejected: %v", err)
	}
	if names, _ := cfg.rpcEndpoints(); strings.Join(names, ",") != "HTTP_RPC_URL[1],HTTP_RPC_URL[2]" {
		t.Fatalf("endpoint names = %v", names)
	}
	for name, urls := range map[string][]string{
		"duplicate":     {"https://rpc-a.invalid", "https://rpc-a.invalid/"},
		"not http":      {"https://rpc-a.invalid", "ws://rpc-b.invalid"},
		"empty":         {},
		"too many":      {"https://a.invalid", "https://b.invalid", "https://c.invalid"},
		"empty element": {"https://rpc-a.invalid", ""},
	} {
		cfg.RPCURLs = urls
		if err := validateRuntimeConfig(cfg); err == nil {
			t.Fatalf("%s: HTTP_RPC_URL=%v must be rejected", name, urls)
		}
	}
}

// The old variable must not be silently ignored (the backup would vanish).
func TestLegacySecondRPCVariableIsRejected(t *testing.T) {
	t.Setenv("HTTP_RPC_URL_2", "https://rpc-b.invalid")
	t.Setenv("NET_ID", "1")
	t.Setenv("REWARD_ACCOUNT", "0xda0b76cc92e94a6f972Ed29C104BBe8d73c2f753")
	if _, err := LoadConfig("relayer.config.example.json"); err == nil || !strings.Contains(err.Error(), "JSON array") {
		t.Fatalf("HTTP_RPC_URL_2 must be refused with a migration hint, got %v", err)
	}
}

func failoverRelayer(t *testing.T, pool *rpcPool) *Relayer {
	t.Helper()
	poolABI, err := abi.JSON(strings.NewReader(tornadoPoolABI))
	if err != nil {
		t.Fatal(err)
	}
	return &Relayer{
		cfg: Config{
			ChainID:           testChainID,
			RewardAccount:     testRewardAccount,
			InstanceByAddress: map[common.Address]InstanceMeta{testPool: units(InstanceMeta{Currency: "eth", Amount: "0.1", Decimals: 18})},
		},
		chain:     pool,
		poolABI:   poolABI,
		ozAddress: common.HexToAddress("0x5555555555555555555555555555555555555555"),
	}
}

func TestPreflightFailsOverToTheOtherEndpoint(t *testing.T) {
	backup := newFakeNode(t)
	backup.spent = true // a decision only the live backup can produce
	r := failoverRelayer(t, testRPCPool(t, nil, backup))
	_, err := r.prepareWithdrawWithFailover(context.Background(), testWithdrawRequest())
	if err == nil || !strings.Contains(err.Error(), "already spent") || failureRetryable(err) {
		t.Fatalf("preflight must be rerun on the live endpoint, got %v", err)
	}
}

func TestPreflightBusinessOutcomeIsNotRetriedElsewhere(t *testing.T) {
	primary, backup := newFakeNode(t), newFakeNode(t)
	primary.spent = true
	r := failoverRelayer(t, testRPCPool(t, primary, backup))
	if _, err := r.prepareWithdrawWithFailover(context.Background(), testWithdrawRequest()); err == nil || !strings.Contains(err.Error(), "already spent") {
		t.Fatalf("want the spent decision from the first endpoint, got %v", err)
	}
	if backup.requests.Load() != 0 {
		t.Fatal("a business decision must not be shopped around to another endpoint")
	}
}

// countingProxy forwards to the shared fake chain and counts requests.
type countingProxy struct {
	next     http.Handler
	requests atomic.Int64
}

func (p *countingProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	p.requests.Add(1)
	p.next.ServeHTTP(w, req)
}

func TestReconcilePinsOneEndpointPerJobAndRotates(t *testing.T) {
	h := newOZHarness(t)
	a, b := &countingProxy{next: h.chain}, &countingProxy{next: h.chain}
	h.r.chain = testRPCPool(t, a, b)
	job, data := h.createSubmittedJob(t)
	hash := h.chain.mine(t, 7, testPool, data, types.ReceiptStatusFailed)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) { tx.Status = ozStatusFailed; tx.Hash = hash.Hex() })
	h.chain.setFinalizedBlock(10) // not final yet: every round reads, none resolves

	var first string
	for round := 0; round < 4; round++ {
		beforeA, beforeB := a.requests.Load(), b.requests.Load()
		h.reconcile(t, job.ID)
		usedA, usedB := a.requests.Load()-beforeA, b.requests.Load()-beforeB
		if (usedA == 0) == (usedB == 0) {
			t.Fatalf("round %d: one reconciliation must read from exactly one endpoint, got A=%d B=%d", round, usedA, usedB)
		}
		used := "A"
		if usedB > 0 {
			used = "B"
		}
		if round == 0 {
			first = used
		} else if (round%2 == 0) != (used == first) {
			t.Fatalf("round %d used %s; reconciliations must alternate endpoints", round, used)
		}
	}
	if got := h.job(t, job.ID); isTerminalStatus(got.Status) || !nullifierHeld(t, h) {
		t.Fatalf("an unfinalized revert must keep the job waiting: %+v", got)
	}
}

// A node that accepts connections but never answers must be marked down by
// both the request timeout and the probe, or failover never happens.
func TestHungEndpointIsMarkedDown(t *testing.T) {
	release := make(chan struct{})
	hung := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-release:
		case <-req.Context().Done():
		}
	})
	defer close(release)
	saved := rpcRequestTimeout
	rpcRequestTimeout = rpcSlowAfter + 500*time.Millisecond
	t.Cleanup(func() { rpcRequestTimeout = saved })

	pool := testRPCPool(t, newFakeNode(t), hung)
	if _, err := pool.endpoints[1].client.ChainID(context.Background()); err == nil {
		t.Fatal("a hung endpoint cannot answer")
	}
	if pool.endpoints[1].healthy(time.Now()) {
		t.Fatal("a request timeout must mark the endpoint down")
	}

	pool.endpoints[1].markUp()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // the readiness budget
	defer cancel()
	if results := pool.probe(ctx, testChainID); results[1].err == nil || pool.endpoints[1].healthy(time.Now()) || !pool.endpoints[0].healthy(time.Now()) {
		t.Fatalf("the probe must mark only the hung endpoint down: %+v", results)
	}

	pool.endpoints[1].markUp()
	short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	_, _ = pool.endpoints[1].client.ChainID(short)
	if !pool.endpoints[1].healthy(time.Now()) {
		t.Fatal("a caller's own short deadline is not the endpoint's fault")
	}
}

// An unknown root usually means the node lags: the other endpoint is asked.
func TestUnknownRootIsAskedOnTheOtherEndpoint(t *testing.T) {
	lagging, current := newFakeNode(t), newFakeNode(t)
	lagging.unknown = true
	r := failoverRelayer(t, testRPCPool(t, lagging, current))
	current.spent = true // a decision only the second endpoint produces
	if _, err := r.prepareWithdrawWithFailover(context.Background(), testWithdrawRequest()); err == nil || !strings.Contains(err.Error(), "already spent") {
		t.Fatalf("an unknown root must be retried on the other endpoint, got %v", err)
	}
	current.unknown = true
	r = failoverRelayer(t, testRPCPool(t, lagging, current))
	if _, err := r.prepareWithdrawWithFailover(context.Background(), testWithdrawRequest()); !errors.Is(err, errRootUnknown) || !failureRetryable(err) {
		t.Fatalf("unknown on every endpoint stays a retryable unknown root, got %v", err)
	}
}

// The base-fee cache is per endpoint: a preflight pinned to B never reuses
// the header cached from A.
func TestLatestHeaderCacheIsPerEndpoint(t *testing.T) {
	a, b := newFakeNode(t), newFakeNode(t)
	b.head.Store(105)
	r := failoverRelayer(t, testRPCPool(t, a, b))
	for i, want := range []uint64{100, 105, 100} {
		header, err := r.latestHeader(withRPCEndpoint(context.Background(), r.chain.endpoints[i%2]))
		if err != nil || header.Number.Uint64() != want {
			t.Fatalf("read %d: header %v err %v, want block %d", i, header, err, want)
		}
	}
}
