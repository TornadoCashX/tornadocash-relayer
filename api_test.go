package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Regression tests for the code-audit fixes. Each covers one defect that the
// existing suite did not exercise.

func TestDecodeWithdrawRequestReportsOversizedBody(t *testing.T) {
	body := bytes.Repeat([]byte(" "), maxRequestBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/tornadoWithdraw", bytes.NewReader(body))
	req.Body = http.MaxBytesReader(httptest.NewRecorder(), req.Body, maxRequestBodyBytes)
	var decoded WithdrawRequest
	if err := decodeWithdrawRequest(req, &decoded); !errors.Is(err, errRequestBodyTooLarge) {
		t.Fatalf("err=%v, want errRequestBodyTooLarge", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/tornadoWithdraw", strings.NewReader("{"))
	if err := decodeWithdrawRequest(req, &decoded); err == nil || err.Error() != "invalid JSON body" {
		t.Fatalf("malformed body err=%v, want invalid JSON body", err)
	}
}

func TestRateLimitBucketsIPv6Per64(t *testing.T) {
	l := newIPRateLimiter(60, 5, 10000)
	now, allowed := time.Now(), 0
	for i := 0; i < 100; i++ {
		if ok, _ := l.allow(rateLimitKey(net.ParseIP(fmt.Sprintf("2001:db8::%x", i+1))), now); ok {
			allowed++
		}
	}
	if allowed != 5 {
		t.Fatalf("one /64 must share a bucket, allowed %d", allowed)
	}
	if rateLimitKey(net.ParseIP("192.0.2.1")) == rateLimitKey(net.ParseIP("192.0.2.2")) {
		t.Fatal("IPv4 addresses keep separate buckets")
	}
}

// /status and admission must not trust health data that stopped refreshing.
func TestStaleHealthIsNotReportedHealthy(t *testing.T) {
	h := newOZHarness(t)
	h.r.reconcilerHeartbeat.Store(time.Now().Unix())
	h.r.refreshOZStatus(context.Background())
	h.r.setReadinessErrors(nil)
	if reason := h.r.ozAdmissionError(); reason != "" {
		t.Fatalf("fresh state must admit, got %q", reason)
	}
	if errs := h.r.cachedReadinessErrors(); len(errs) != 0 {
		t.Fatalf("fresh readiness must be clean, got %v", errs)
	}

	old := time.Now().Add(-2 * h.r.statusStaleAfter())
	h.r.readinessCachedAt.Store(old.Unix())
	if errs := h.r.cachedReadinessErrors(); !slices.Contains(errs, "health status stale") {
		t.Fatalf("stale readiness cache must be reported, got %v", errs)
	}
	h.r.ozMu.Lock()
	h.r.ozStatusAt = old
	h.r.ozMu.Unlock()
	if reason := h.r.ozAdmissionError(); !strings.Contains(reason, "unavailable") {
		t.Fatalf("stale OZ status must refuse admission, got %q", reason)
	}
	h.r.refreshOZStatus(context.Background())
	h.r.reconcilerHeartbeat.Store(old.Unix())
	if reason := h.r.ozAdmissionError(); !strings.Contains(reason, "reconciler") {
		t.Fatalf("stopped reconciler must refuse admission, got %q", reason)
	}
}

// With no reachable RPC the request is refused up front instead of becoming a
// job that fails with "internal error".
func TestWithdrawRefusedWhileNoRPCIsHealthy(t *testing.T) {
	h := newOZHarness(t)
	h.r.setReadinessErrors([]string{"rpc unavailable"})
	rec := httptest.NewRecorder()
	h.r.handleWithdraw(rec, httptest.NewRequest(http.MethodPost, "/v1/tornadoWithdraw", strings.NewReader("{}")))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
}

// A missing price of one token must not mark the whole relayer unhealthy:
// only ERC20 pools of that currency need it.
func TestMissingTokenPriceKeepsStatusHealthy(t *testing.T) {
	r := &Relayer{
		cfg: Config{
			ChainID: big.NewInt(1),
			OraclePriceTokens: map[string]oraclePriceToken{
				"dai":  {Address: common.HexToAddress("0x6B175474E89094C44Da98b954EedeAC495271d0F"), Decimals: 18},
				"torn": {Address: common.HexToAddress("0x77777FeDdddFfC19Ff86DB637967013e6C6A116C"), Decimals: 18},
			},
		},
		store:  newTestSQLiteStore(t),
		prices: freshPrices(map[string]*big.Int{"dai": big.NewInt(1)}),
	}
	rec := httptest.NewRecorder()
	r.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var body struct {
		Health    healthResponse    `json:"health"`
		EthPrices map[string]string `json:"ethPrices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Health.Status != "true" || !slices.Contains(body.Health.ErrorsLog, "price oracle unavailable: torn") {
		t.Fatalf("health = %+v", body.Health)
	}
	if _, ok := body.EthPrices["torn"]; ok || body.EthPrices["dai"] != "1" {
		t.Fatalf("ethPrices = %v", body.EthPrices)
	}
	if errs := r.priceErrors(); !slices.Contains(errs, "price oracle unavailable: torn") {
		t.Fatalf("/ready must still report the missing price: %v", errs)
	}
}

func TestCloudflareClientIPRequiresTrustedPeer(t *testing.T) {
	_, loopback, _ := net.ParseCIDR("127.0.0.1/32")
	relayer := &Relayer{cfg: Config{TrustedProxyCIDRs: []*net.IPNet{loopback}, ClientIPHeader: "CF-Connecting-IP"}}

	trusted := httptest.NewRequest(http.MethodGet, "/status", nil)
	trusted.RemoteAddr = "127.0.0.1:1234"
	trusted.Header.Set("CF-Connecting-IP", "203.0.113.8")
	if got := relayer.clientIP(trusted); got == nil || got.String() != "203.0.113.8" {
		t.Fatalf("trusted tunnel client IP=%v", got)
	}

	untrusted := httptest.NewRequest(http.MethodGet, "/status", nil)
	untrusted.RemoteAddr = "198.51.100.7:4321"
	untrusted.Header.Set("CF-Connecting-IP", "203.0.113.8")
	if got := relayer.clientIP(untrusted); got == nil || got.String() != "198.51.100.7" {
		t.Fatalf("untrusted peer spoofed client IP=%v", got)
	}
}

func TestRequestSourceDomainAndCloudflareRay(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Origin", "https://Tornado.CP0X.com")
	req.Header.Set("Referer", "https://ignored.example/private?token=secret")
	req.Header.Set("CF-Ray", "a326930e4ef77fba-WAW")
	if got := requestSourceDomain(req); got != "tornado.cp0x.com" {
		t.Fatalf("source domain=%q", got)
	}
	if got := cloudflareRay(req); got != "a326930e4ef77fba-WAW" {
		t.Fatalf("CF-Ray=%q", got)
	}
	req.Header.Del("Origin")
	if got := requestSourceDomain(req); got != "ignored.example" {
		t.Fatalf("referer fallback domain=%q", got)
	}
	req.Header.Del("Referer")
	if got := requestSourceDomain(req); got != "direct" {
		t.Fatalf("direct source=%q", got)
	}
}

func TestRelayRateLimitUsesCloudflareClientIP(t *testing.T) {
	_, loopback, _ := net.ParseCIDR("127.0.0.1/32")
	relayer := &Relayer{
		cfg:         Config{TrustedProxyCIDRs: []*net.IPNet{loopback}, ClientIPHeader: "CF-Connecting-IP"},
		rateLimiter: newIPRateLimiter(1, 1, 100),
	}
	handler := relayer.withRelayRateLimit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request := func(ip string) int {
		req := httptest.NewRequest(http.MethodPost, "/relay", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("CF-Connecting-IP", ip)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := request("203.0.113.1"); got != http.StatusNoContent {
		t.Fatalf("first request status=%d", got)
	}
	if got := request("203.0.113.1"); got != http.StatusTooManyRequests {
		t.Fatalf("same client should be limited, status=%d", got)
	}
	if got := request("203.0.113.2"); got != http.StatusNoContent {
		t.Fatalf("different client shared rate bucket, status=%d", got)
	}
}

func TestRateLimiterCapsClientBucketsWithLRUEviction(t *testing.T) {
	limiter := newIPRateLimiter(1, 1, 2) // 1 per minute, burst 1
	now := time.Now()
	allowedA, _ := limiter.allow("client-a", now)
	allowedB, _ := limiter.allow("client-b", now.Add(time.Second))
	if !allowedA || !allowedB {
		t.Fatal("initial clients should be admitted")
	}
	// At capacity a new client is still admitted (via the shared overflow
	// bucket); the table never exceeds the cap.
	if allowed, _ := limiter.allow("client-c", now.Add(2*time.Second)); !allowed {
		t.Fatal("new client must not be locked out at bucket capacity")
	}
	if len(limiter.index) != 2 || limiter.lru.Len() != 2 {
		t.Fatalf("bucket count=%d/%d, want 2", len(limiter.index), limiter.lru.Len())
	}
	// The least recently used bucket (client-a) still limits its client, so it
	// must not be evicted: rotating keys must not reset a client's limit.
	if _, ok := limiter.index["client-a"]; !ok {
		t.Fatal("still-limited bucket must not be evicted")
	}
	if allowed, _ := limiter.allow("client-a", now.Add(2*time.Second)); allowed {
		t.Fatal("client-a must keep its rate limit")
	}
	// client-b keeps its (empty) bucket and is still limited.
	if allowed, _ := limiter.allow("client-b", now.Add(2*time.Second)); allowed {
		t.Fatal("existing client must keep its rate limit")
	}
	// A second new key at capacity shares the overflow bucket whose single
	// token client-c already consumed.
	if allowed, _ := limiter.allow("client-d", now.Add(2*time.Second)); allowed {
		t.Fatal("new keys at capacity must share one overflow bucket")
	}
	if len(limiter.index) != 2 {
		t.Fatalf("bucket count=%d, want 2", len(limiter.index))
	}
}

func TestHealthAuthorizationRequiresBearerScheme(t *testing.T) {
	token := strings.Repeat("h", 32)
	relayer := &Relayer{cfg: Config{HealthToken: token}}
	for header, want := range map[string]bool{
		"Bearer " + token:       true,
		token:                   false,
		"bearer " + token:       false,
		"Basic " + token:        false,
		"Bearer " + token + "x": false,
		"Bearer  " + token:      false,
		"":                      false, // no Authorization header
	} {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.Header.Set("Authorization", header)
		if got := relayer.authorizeHealth(req); got != want {
			t.Fatalf("Authorization %q: got %v want %v", header, got, want)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Authorization", "Bearer "+token)
	if relayer.authorizeHealth(req) {
		t.Fatal("duplicate Authorization headers must be rejected")
	}
}

func TestRateLimiterReportsActualRetryDelay(t *testing.T) {
	limiter := newIPRateLimiter(1, 1, 10)
	now := time.Now()
	if allowed, _ := limiter.allow("client", now); !allowed {
		t.Fatal("first request should be allowed")
	}
	allowed, retryAfter := limiter.allow("client", now)
	if allowed || retryAfter < 59*time.Second {
		t.Fatalf("allowed=%v retryAfter=%s, want about one minute", allowed, retryAfter)
	}
}

func TestMetricsRequireHealthToken(t *testing.T) {
	store := newTestSQLiteStore(t)
	relayer := &Relayer{cfg: Config{HealthToken: strings.Repeat("h", 32)}, store: store}
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	recorder := httptest.NewRecorder()
	relayer.handleMetrics(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("metrics without token status=%d", recorder.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("h", 32))
	recorder = httptest.NewRecorder()
	relayer.handleMetrics(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "relayer_jobs_current") {
		t.Fatalf("authorized metrics status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestStatusMatchesOfficialShape(t *testing.T) {
	store := newTestSQLiteStore(t)
	relayer := &Relayer{
		cfg: Config{
			ChainID:       big.NewInt(1),
			RewardAccount: common.HexToAddress("0xda0b76cc92e94a6f972Ed29C104BBe8d73c2f753"),
			Instances:     InstancesConfig{"dai": {InstanceAddress: map[string]string{"100": "0xD4B88Df4D29F5CedD6857912842cff3b20C8Cfa3"}, TokenAddress: "0x6B175474E89094C44Da98b954EedeAC495271d0F", Symbol: "DAI", Decimals: 18}, "eth": {InstanceAddress: map[string]string{"1": "0x47CE0C6eD5B0Ce3d3A51fdb1C52DC66a7c3c2936", "0.1": "0x12D66f87A04A9E220743712cE6d9bB1B5616B8Fc"}, Symbol: "ETH", Decimals: 18}},
			OraclePriceTokens: map[string]oraclePriceToken{
				"dai": {Address: common.HexToAddress("0x6B175474E89094C44Da98b954EedeAC495271d0F"), Decimals: 18},
			},
			ServiceFeeBPS: 40,
			Version:       "4.1.6",
		},
		store:  store,
		prices: freshPrices(map[string]*big.Int{"dai": big.NewInt(2)}),
	}

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	relayer.handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status code %d", rec.Code)
	}
	raw := rec.Body.String()
	expectedOrder := []string{
		`"rewardAccount"`,
		`"instances"`,
		`"netId"`,
		`"ethPrices"`,
		`"tornadoServiceFee"`,
		`"miningServiceFee"`,
		`"version"`,
		`"health"`,
		`"currentQueue"`,
	}
	assertFieldOrder(t, raw, expectedOrder)
	if !strings.Contains(raw, `"health":{"status":"true","error":"","errorsLog":[]}`) {
		t.Fatalf("health should match official-compatible string format, got %s", raw)
	}
	assertFieldOrder(t, raw, []string{`"instances":{"eth"`, `"dai"`})
	assertFieldOrder(t, raw, []string{`"instanceAddress":{"1"`, `"0.1"`})
	if !strings.Contains(raw, `"ethPrices":{"dai":"2"}`) {
		t.Fatalf("status should expose only configured ERC20 prices: %s", raw)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if body["rewardAccount"] != "0xda0b76cc92e94a6f972Ed29C104BBe8d73c2f753" {
		t.Fatalf("unexpected rewardAccount %v", body["rewardAccount"])
	}
	if body["netId"] != float64(1) {
		t.Fatalf("netId should be numeric, got %T %v", body["netId"], body["netId"])
	}
	if body["tornadoServiceFee"] != 0.4 {
		t.Fatalf("unexpected tornadoServiceFee %v", body["tornadoServiceFee"])
	}
	if body["miningServiceFee"] != 0.05 {
		t.Fatalf("unexpected miningServiceFee %v", body["miningServiceFee"])
	}
	prices, ok := body["ethPrices"].(map[string]any)
	wantCurrencies := []string{"dai"}
	if !ok || len(prices) != len(wantCurrencies) {
		t.Fatalf("mainnet ethPrices must contain configured token prices: %#v", body["ethPrices"])
	}
	for _, currency := range wantCurrencies {
		if _, ok := prices[currency]; !ok {
			t.Fatalf("mainnet ethPrices missing %s", currency)
		}
	}
	if _, ok := body["dryRun"]; ok {
		t.Fatalf("status should not expose dryRun in official-compatible shape")
	}
	health, ok := body["health"].(map[string]any)
	if !ok {
		t.Fatalf("health should be an object")
	}
	if health["status"] != "true" || health["error"] != "" {
		t.Fatalf("unexpected health: %#v", health)
	}
	if _, ok := health["errorsLog"].([]any); !ok {
		t.Fatalf("health.errorsLog should be an array, got %T", health["errorsLog"])
	}
}

func TestStatusUsesCachedReadiness(t *testing.T) {
	store := newTestSQLiteStore(t)
	relayer := &Relayer{
		cfg:   Config{ChainID: big.NewInt(1), RewardAccount: common.Address{}, Version: "1.0.0"},
		store: store,
	}
	relayer.readinessMu.Lock()
	relayer.cachedReadyErrors = []string{"rpc unavailable"}
	relayer.readinessMu.Unlock()
	recorder := httptest.NewRecorder()
	relayer.handleStatus(recorder, httptest.NewRequest(http.MethodGet, "/status", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"false"`) || !strings.Contains(recorder.Body.String(), "rpc unavailable") {
		t.Fatalf("status did not use cached readiness: code=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestConfiguredMainnetVersionIsReturnedUnchanged(t *testing.T) {
	store := newTestSQLiteStore(t)
	relayer := &Relayer{
		cfg: Config{
			ChainID:       big.NewInt(1),
			RewardAccount: common.HexToAddress("0xda0b76cc92e94a6f972Ed29C104BBe8d73c2f753"),
			Instances:     InstancesConfig{"eth": {InstanceAddress: map[string]string{"1": "0x47CE0C6eD5B0Ce3d3A51fdb1C52DC66a7c3c2936"}, Symbol: "ETH", Decimals: 18}},
			ServiceFeeBPS: 40,
			Version:       "4.1.6",
		},
		store: store,
		prices: freshPrices(map[string]*big.Int{
			"torn": big.NewInt(1), "dai": big.NewInt(2), "cdai": big.NewInt(3), "usdc": big.NewInt(4), "usdt": big.NewInt(5), "wbtc": big.NewInt(6),
		}),
	}
	rec := httptest.NewRecorder()
	relayer.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

	want := `{"rewardAccount":"0xda0b76cc92e94a6f972Ed29C104BBe8d73c2f753","instances":{"eth":{"instanceAddress":{"1":"0x47CE0C6eD5B0Ce3d3A51fdb1C52DC66a7c3c2936"},"symbol":"ETH","decimals":18}},"netId":1,"tornadoServiceFee":0.4,"version":"4.1.6","health":{"status":"true","error":"","errorsLog":[]},"currentQueue":0}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("status golden mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestStatusMatchesBNBChainShape(t *testing.T) {
	store := newTestSQLiteStore(t)
	relayer := &Relayer{
		cfg: Config{
			ChainID:       big.NewInt(56),
			RewardAccount: common.HexToAddress("0xda0b76cc92e94a6f972Ed29C104BBe8d73c2f753"),
			Instances: InstancesConfig{"bnb": {InstanceAddress: map[string]string{
				"1":   "0xd47438C816c9E7f2E2888E060936a499Af9582b3",
				"10":  "0x330bdFADE01eE9bF63C209Ee33102DD334618e0a",
				"100": "0x1E34A77868E19A6647b1f2F47B51ed72dEDE95DD",
				"0.1": "0x84443CFd09A48AF6eF360C6976C5392aC5023a1F",
			}, Symbol: "BNB", Decimals: 18}},
			ServiceFeeBPS: 10,
			Version:       "5.2.1",
		},
		store: store,
	}

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	relayer.handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status code %d", rec.Code)
	}
	raw := rec.Body.String()
	expectedOrder := []string{
		`"rewardAccount"`,
		`"instances"`,
		`"netId"`,
		`"tornadoServiceFee"`,
		`"version"`,
		`"health"`,
		`"currentQueue"`,
	}
	assertFieldOrder(t, raw, expectedOrder)
	for _, forbidden := range []string{`"ethPrices"`, `"miningServiceFee"`, `"dryRun"`} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("BNB status should not expose %s in %s", forbidden, raw)
		}
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode BNB status: %v", err)
	}
	if body["rewardAccount"] != "0xda0b76cc92e94a6f972Ed29C104BBe8d73c2f753" {
		t.Fatalf("unexpected rewardAccount %v", body["rewardAccount"])
	}
	if body["netId"] != float64(56) {
		t.Fatalf("netId should be 56, got %T %v", body["netId"], body["netId"])
	}
	if body["tornadoServiceFee"] != 0.1 {
		t.Fatalf("unexpected tornadoServiceFee %v", body["tornadoServiceFee"])
	}
	if body["version"] != "5.2.1" {
		t.Fatalf("unexpected version %v", body["version"])
	}
}

func TestStatusExposesEthPricesForSepolia(t *testing.T) {
	store := newTestSQLiteStore(t)
	relayer := &Relayer{
		cfg: Config{
			ChainID:       big.NewInt(11155111),
			RewardAccount: common.HexToAddress("0xda0b76cc92e94a6f972Ed29C104BBe8d73c2f753"),
			Instances:     InstancesConfig{"dai": {InstanceAddress: map[string]string{"100": "0x6921fd1a97441dd603a997ED6DDF388658daf754"}, TokenAddress: "0xFF34B3d4Aee8ddCd6F9AFFFB6Fe49bD371b8a357", OracleTokenAddress: "0x6B175474E89094C44Da98b954EedeAC495271d0F", Symbol: "DAI", Decimals: 18}, "eth": {InstanceAddress: map[string]string{"0.1": "0x8C4A04d872a6C1BE37964A21ba3a138525dFF50b"}, Symbol: "ETH", Decimals: 18}},
			OraclePriceTokens: oraclePriceTokensFromInstances(InstancesConfig{
				"dai": {TokenAddress: "0xFF34B3d4Aee8ddCd6F9AFFFB6Fe49bD371b8a357", OracleTokenAddress: "0x6B175474E89094C44Da98b954EedeAC495271d0F", Decimals: 18},
			}),
			ServiceFeeBPS: 40,
			Version:       "5.2.1",
		},
		store:  store,
		prices: freshPrices(map[string]*big.Int{"dai": big.NewInt(1)}),
	}

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	relayer.handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status code %d", rec.Code)
	}
	raw := rec.Body.String()
	for _, field := range []string{`"netId":11155111`, `"ethPrices":{"dai":"1"}`, `"miningServiceFee"`, `"version":"5.2.1"`} {
		if !strings.Contains(raw, field) {
			t.Fatalf("missing Sepolia status field %s in %s", field, raw)
		}
	}
}

func TestConfiguredStatusShapesMatchClassicUIChains(t *testing.T) {
	fileCfg, err := loadFileConfig("relayer.config.example.json")
	if err != nil {
		t.Fatalf("load relayer config: %v", err)
	}
	requiredToken := map[uint64]string{
		1: "eth", 56: "bnb", 100: "xdai", 137: "matic",
		42161: "eth", 43114: "avax", 11155111: "eth",
	}
	for netID, token := range requiredToken {
		t.Run(fmt.Sprintf("netId%d", netID), func(t *testing.T) {
			instances, _, err := loadInstances(fileCfg.Instances, netID)
			if err != nil {
				t.Fatalf("load instances: %v", err)
			}
			network := fileCfg.Networks[fmt.Sprintf("netId%d", netID)]
			priceTokens, err := mergeOraclePriceTokens(instances, network.OraclePriceTokens)
			if err != nil {
				t.Fatalf("merge oracle price tokens: %v", err)
			}
			relayer := &Relayer{
				cfg: Config{
					ChainID:           new(big.Int).SetUint64(netID),
					RewardAccount:     common.HexToAddress("0xda0b76cc92e94a6f972Ed29C104BBe8d73c2f753"),
					Instances:         instances,
					OraclePriceTokens: priceTokens,
					ServiceFeeBPS:     40,
					Version:           network.Version,
				},
				store: newTestSQLiteStore(t),
			}
			if len(priceTokens) > 0 {
				prices := make(map[string]*big.Int)
				for currency := range priceTokens {
					prices[currency] = big.NewInt(1)
				}
				relayer.prices = freshPrices(prices)
			}
			req := httptest.NewRequest(http.MethodGet, "/status", nil)
			rec := httptest.NewRecorder()
			relayer.handleStatus(rec, req)
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode status: %v", err)
			}
			if body["version"] != network.Version {
				t.Fatalf("version=%v, want %s", body["version"], network.Version)
			}
			statusInstances := body["instances"].(map[string]any)
			if _, ok := statusInstances[token]; !ok {
				t.Fatalf("instances missing classic UI token %s", token)
			}
			health := body["health"].(map[string]any)
			if health["status"] != "true" {
				t.Fatalf("status health=%v", health)
			}
			_, hasPrices := body["ethPrices"]
			if hasPrices != (len(priceTokens) > 0) {
				t.Fatalf("ethPrices presence=%v", hasPrices)
			}
		})
	}
}

func assertFieldOrder(t *testing.T, raw string, fields []string) {
	t.Helper()
	last := -1
	for _, field := range fields {
		relativePos := strings.Index(raw[last+1:], field)
		if relativePos < 0 {
			t.Fatalf("missing or out-of-order JSON fragment %s in %s", field, raw)
		}
		last += 1 + relativePos
	}
}

func TestClientIPModes(t *testing.T) {
	_, loopback, _ := net.ParseCIDR("127.0.0.1/32")
	relayer := &Relayer{cfg: Config{TrustedProxyCIDRs: []*net.IPNet{loopback}, ClientIPHeader: "CF-Connecting-IP"}}
	request := func(remote string, headers map[string][]string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remote
		for name, values := range headers {
			for _, value := range values {
				req.Header.Add(name, value)
			}
		}
		return req
	}
	for _, c := range []struct {
		mode    string
		remote  string
		headers map[string][]string
		want    string
	}{
		{"cloudflare", "127.0.0.1:1", map[string][]string{"CF-Connecting-IP": {"1.1.1.1"}}, "1.1.1.1"},
		{"cloudflare", "8.8.8.8:1", map[string][]string{"CF-Connecting-IP": {"1.1.1.1"}}, "8.8.8.8"},
		{"cloudflare", "127.0.0.1:1", map[string][]string{"CF-Connecting-IP": {"1.1.1.1", "2.2.2.2"}}, "127.0.0.1"},
		{"none", "127.0.0.1:1", map[string][]string{"CF-Connecting-IP": {"1.1.1.1"}}, "127.0.0.1"},
		{"xff", "127.0.0.1:1", map[string][]string{"X-Forwarded-For": {"6.6.6.6, 3.3.3.3"}}, "3.3.3.3"},
		{"xff", "127.0.0.1:1", map[string][]string{"X-Forwarded-For": {"3.3.3.3, 127.0.0.1"}}, "3.3.3.3"},
		{"xff", "127.0.0.1:1", map[string][]string{"CF-Connecting-IP": {"1.1.1.1"}}, "127.0.0.1"},
		{"xff", "127.0.0.1:1", map[string][]string{"X-Forwarded-For": {"garbage"}}, "127.0.0.1"},
	} {
		relayer.cfg.ClientIPMode = c.mode
		if got := relayer.clientIP(request(c.remote, c.headers)); got.String() != c.want {
			t.Fatalf("mode=%s remote=%s headers=%v: got %s want %s", c.mode, c.remote, c.headers, got, c.want)
		}
	}
	cfg := productionRuntimeConfig()
	cfg.ClientIPMode = "nginx"
	if err := validateRuntimeConfig(cfg); err == nil {
		t.Fatal("unknown CLIENT_IP_MODE must be rejected")
	}
}

func TestOfficialJobStatusMapsActiveInternalStates(t *testing.T) {
	for _, internal := range []JobStatus{StatusSubmitting, StatusSubmitted} {
		status := officialJobStatus(&Job{ID: "job", Type: "TORNADO_WITHDRAW", Status: internal})
		if status.Status != StatusSent || status.InternalStatus != internal {
			t.Fatalf("internal=%s public=%s exposed=%s", internal, status.Status, status.InternalStatus)
		}
	}
	for _, public := range []JobStatus{StatusQueued, StatusPreparing, StatusMined, StatusConfirmed, StatusFailed} {
		status := officialJobStatus(&Job{ID: "job", Status: public})
		if status.Status != public || status.InternalStatus != "" {
			t.Fatalf("status %s must be exposed unchanged, got %+v", public, status)
		}
	}
}

// httpCall sends one request through the full routes() middleware chain.
func httpCall(t *testing.T, h http.Handler, method, path, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.10:40000"
	for key, values := range header {
		req.Header[key] = values
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func requestJSON(t *testing.T, req WithdrawRequest) string {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestHTTPWithdrawLifecycle drives admission, validation, dedup, the worker
// and job lookup through routes(), i.e. with CORS, the body limit, the rate
// limiter and the access log in front of the handlers.
func TestHTTPWithdrawLifecycle(t *testing.T) {
	h := newOZHarness(t)
	configureWorkers(t, h)
	newRecoveryChain(t, h)
	h.r.cfg.CORSOrigins = []string{"https://app.example"}
	h.r.rateLimiter = newIPRateLimiter(600, 100, 10)
	routes := h.r.routes()

	t.Run("CORS preflight", func(t *testing.T) {
		rec := httpCall(t, routes, http.MethodOptions, "/v1/tornadoWithdraw", "", http.Header{"Origin": {"https://app.example"}})
		if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example" ||
			rec.Header().Get("Vary") != "Origin" || rec.Header().Get("X-Frame-Options") != "DENY" {
			t.Fatalf("OPTIONS: %d %v", rec.Code, rec.Header())
		}
		rec = httpCall(t, routes, http.MethodOptions, "/v1/tornadoWithdraw", "", http.Header{"Origin": {"https://evil.example"}})
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("foreign origin allowed: %v", rec.Header())
		}
	})

	t.Run("rejected bodies", func(t *testing.T) {
		badRelayer := preflightableRequest("c1")
		badRelayer.Args[argRelayer] = "0x0000000000000000000000000000000000000077"
		for name, tc := range map[string]struct {
			body string
			code int
			want string
		}{
			"too large":     {`{"proof":"` + strings.Repeat("a", maxRequestBodyBytes) + `"}`, http.StatusRequestEntityTooLarge, errRequestBodyTooLarge.Error()},
			"invalid JSON":  {`{"proof":`, http.StatusBadRequest, "invalid JSON body"},
			"wrong relayer": {requestJSON(t, badRelayer), http.StatusBadRequest, "rewardAccount"},
		} {
			rec := httpCall(t, routes, http.MethodPost, "/v1/tornadoWithdraw", tc.body, nil)
			if rec.Code != tc.code || !strings.Contains(decodeBody[errorResponse](t, rec).Error, tc.want) {
				t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
			}
		}
		if rec := httpCall(t, routes, http.MethodGet, "/relay", "", nil); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET /relay: %d", rec.Code)
		}
	})

	body := requestJSON(t, preflightableRequest("c2"))
	rec := httpCall(t, routes, http.MethodPost, "/v1/tornadoWithdraw", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("withdraw: %d %s", rec.Code, rec.Body)
	}
	id := decodeBody[idResponse](t, rec).ID
	// The legacy path is the same endpoint; an identical request is deduplicated.
	if rec := httpCall(t, routes, http.MethodPost, "/relay", body, nil); rec.Code != http.StatusOK || decodeBody[idResponse](t, rec).ID != id {
		t.Fatalf("duplicate: %d %s", rec.Code, rec.Body)
	}
	waitWorkers(t, h)
	if got := h.submitCount(); got != 1 {
		t.Fatalf("OZ submits = %d, want 1", got)
	}

	rec = httpCall(t, routes, http.MethodGet, "/v1/jobs/"+id, "", nil)
	status := decodeBody[jobStatusResponse](t, rec)
	if rec.Code != http.StatusOK || status.ID != id || status.Status != StatusSent || status.InternalStatus != StatusSubmitted {
		t.Fatalf("job: %d %+v", rec.Code, status)
	}
	for path, code := range map[string]int{"/v1/jobs/": http.StatusBadRequest, "/v1/jobs/missing": http.StatusBadRequest} {
		if rec := httpCall(t, routes, http.MethodGet, path, "", nil); rec.Code != code {
			t.Fatalf("GET %s: %d", path, rec.Code)
		}
	}
	if rec := httpCall(t, routes, http.MethodPost, "/v1/jobs/"+id, "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST job: %d", rec.Code)
	}

	t.Run("rate limit", func(t *testing.T) {
		h.r.rateLimiter = newIPRateLimiter(1, 1, 10)
		httpCall(t, routes, http.MethodPost, "/relay", "{", nil)
		rec := httpCall(t, routes, http.MethodPost, "/relay", "{", nil)
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("second request: %d %v", rec.Code, rec.Header())
		}
	})

	if got := h.r.httpRequests.Load(); got < 10 {
		t.Fatalf("access log counted %d requests", got)
	}
}

func TestHTTPReadinessRequiresToken(t *testing.T) {
	h := newOZHarness(t)
	configureWorkers(t, h)
	newRecoveryChain(t, h)
	h.r.cfg.HealthToken = strings.Repeat("h", 32)
	routes := h.r.routes()

	for name, header := range map[string]http.Header{
		"missing": nil,
		"wrong":   {"Authorization": {"Bearer " + strings.Repeat("x", 32)}},
		"twice":   {"Authorization": {"Bearer " + h.r.cfg.HealthToken, "Bearer " + h.r.cfg.HealthToken}},
	} {
		if rec := httpCall(t, routes, http.MethodGet, "/ready", "", header); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s token: %d", name, rec.Code)
		}
	}
	auth := http.Header{"Authorization": {"Bearer " + h.r.cfg.HealthToken}}
	if rec := httpCall(t, routes, http.MethodPost, "/ready", "", auth); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /ready: %d", rec.Code)
	}
	rec := httpCall(t, routes, http.MethodGet, "/ready", "", auth)
	ready := decodeBody[readinessResponse](t, rec)
	if rec.Code != http.StatusOK || ready.Status != "ready" || len(ready.Errors) != 0 {
		t.Fatalf("ready: %d %+v", rec.Code, ready)
	}

	h.oz.mu.Lock()
	h.oz.paused = true
	h.oz.mu.Unlock()
	h.r.refreshOZStatus(t.Context())
	rec = httpCall(t, routes, http.MethodGet, "/ready", "", auth)
	if ready := decodeBody[readinessResponse](t, rec); rec.Code != http.StatusServiceUnavailable || ready.Status != "not_ready" || len(ready.Errors) == 0 {
		t.Fatalf("paused OZ: %d %+v", rec.Code, ready)
	}
}

func TestWriteCreateJobErrorStatus(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
		want string
	}{
		{fmt.Errorf("create: %w", ErrQueueFull), http.StatusServiceUnavailable, ErrQueueFull.Error()},
		{fmt.Errorf("create: %w", ErrNullifierReverted), http.StatusBadRequest, ErrNullifierReverted.Error()},
		{errors.New("disk I/O error at /var/lib/relayer"), http.StatusInternalServerError, "internal server error"},
	} {
		rec := httptest.NewRecorder()
		writeCreateJobError(rec, tc.err)
		if got := decodeBody[errorResponse](t, rec).Error; rec.Code != tc.code || got != tc.want {
			t.Fatalf("%v: %d %q", tc.err, rec.Code, got)
		}
	}
}
