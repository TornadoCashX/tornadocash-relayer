package main

import (
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

func TestUint64SettingRejectsSignedValues(t *testing.T) {
	t.Setenv("GAS_FEE_TOLERANCE_PERCENT", "+10")
	if _, _, err := parseUint64Env("GAS_FEE_TOLERANCE_PERCENT"); err == nil {
		t.Fatal("a signed value must be rejected, not parsed as 0")
	}
	if err := validateUint64Environment(); err == nil || !strings.Contains(err.Error(), "must be a uint64") {
		t.Fatalf("validateUint64Environment err=%v", err)
	}
	t.Setenv("GAS_FEE_TOLERANCE_PERCENT", "10")
	if got := uint64Setting("GAS_FEE_TOLERANCE_PERCENT", 0); got != 10 {
		t.Fatalf("uint64Setting=%d, want 10", got)
	}
}

func TestDecimalToUnitsRejectsEmptyParts(t *testing.T) {
	for _, input := range []string{".", "5.", ".5"} {
		if _, err := decimalToUnits(input, 18); err == nil {
			t.Fatalf("decimalToUnits(%q) must fail", input)
		}
	}
	got, err := decimalToUnits("0.1", 18)
	if err != nil || got.String() != "100000000000000000" {
		t.Fatalf("decimalToUnits(0.1)=%v, %v", got, err)
	}
}

func TestUnquoteDotEnvValueRequiresMatchingPair(t *testing.T) {
	cases := map[string]string{
		`"abc"`: "abc",
		`'abc'`: "abc",
		`abc"`:  `abc"`,
		`"abc'`: `"abc'`,
		`"`:     `"`,
	}
	for input, want := range cases {
		if got := unquoteDotEnvValue(input); got != want {
			t.Fatalf("unquoteDotEnvValue(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestConfigRejectsPlaceholderTokenAndHugeNumbers(t *testing.T) {
	cfg := productionRuntimeConfig()
	cfg.HealthToken = "replace-with-at-least-32-random-characters"
	if err := validateRuntimeConfig(cfg); err == nil {
		t.Fatal("the .env.example HEALTH_TOKEN placeholder must be rejected")
	}
	t.Setenv("GAS_LIMIT_BUFFER_PERCENT", "184467440737095527") // 10000+100*x wraps into range
	if err := validateUint64Environment(); err == nil {
		t.Fatal("a setting that overflows when scaled must be rejected")
	}
}

func TestLoadInstancesOnlyIndexesActiveChainPools(t *testing.T) {
	fileCfg, err := loadFileConfig("relayer.config.example.json")
	if err != nil {
		t.Fatalf("load relayer config: %v", err)
	}
	mainnetTenETH := common.HexToAddress("0x910Cbd523D972eb0a6f4cAe4618aD62622b39DbF")
	bscOneBNB := common.HexToAddress("0xd47438C816c9E7f2E2888E060936a499Af9582b3")
	_, mainnet, err := loadInstances(fileCfg.Instances, 1)
	if err != nil {
		t.Fatalf("load mainnet instances: %v", err)
	}
	if meta, ok := mainnet[mainnetTenETH]; !ok || meta.Amount != "10" || meta.IsERC20 {
		t.Fatalf("mainnet 10 ETH pool missing or wrong: %+v ok=%v", meta, ok)
	}
	if meta := mainnet[common.HexToAddress("0xD4B88Df4D29F5CedD6857912842cff3b20C8Cfa3")]; meta.TokenAddress != common.HexToAddress("0x6B175474E89094C44Da98b954EedeAC495271d0F") {
		t.Fatalf("mainnet DAI pool must carry its token address, got %+v", meta)
	}
	if _, ok := mainnet[bscOneBNB]; ok {
		t.Fatal("BSC pool must not be accepted on mainnet")
	}
	_, bsc, err := loadInstances(fileCfg.Instances, 56)
	if err != nil {
		t.Fatalf("load BSC instances: %v", err)
	}
	if _, ok := bsc[mainnetTenETH]; ok {
		t.Fatal("mainnet pool must not be accepted on BSC")
	}
	if meta, ok := bsc[bscOneBNB]; !ok || meta.Currency != "bnb" || meta.Amount != "1" {
		t.Fatalf("BSC 1 BNB pool missing or wrong: %+v ok=%v", meta, ok)
	}
}

func TestRelayerConfigContainsPerChainFeeSafetyPolicy(t *testing.T) {
	fileCfg, err := loadFileConfig("relayer.config.example.json")
	if err != nil {
		t.Fatalf("load relayer config: %v", err)
	}
	for _, netID := range []uint64{1, 56, 100, 137, 42161, 43114, 11155111} {
		key := fmt.Sprintf("netId%d", netID)
		network, ok := fileCfg.Networks[key]
		if !ok {
			t.Fatalf("missing %s", key)
		}
		if err := validateChainNetworkConfig(network); err != nil {
			t.Fatalf("invalid %s: %v", key, err)
		}
	}
	sepolia := fileCfg.Networks["netId11155111"]
	if sepolia.ServiceFeeBPS == nil || *sepolia.ServiceFeeBPS != 36 || sepolia.MaxGasLimit != 420000 {
		t.Fatalf("unexpected Sepolia policy: %+v", sepolia)
	}
	if fileCfg.Networks["netId42161"].MaxGasLimit != 1900000 {
		t.Fatalf("Arbitrum maxGasLimit must cover its L2 withdrawal estimate")
	}
}

func TestLoadConfigRejectsEthereumClassic(t *testing.T) {
	t.Setenv("NET_ID", "61")
	if _, err := LoadConfig("relayer.config.example.json"); err == nil || !strings.Contains(err.Error(), "EIP-1559 type-2") {
		t.Fatalf("NET_ID=61 must be rejected, got %v", err)
	}
}

func TestLoadConfigUsesPerChainPriceAndGasPolicy(t *testing.T) {
	t.Setenv("NET_ID", "11155111")
	t.Setenv("HTTP_RPC_URL", `["https://rpc-a.invalid","https://rpc-b.invalid"]`)
	t.Setenv("ORACLE_RPC_URL", "https://oracle.invalid")
	t.Setenv("OZ_RELAYER_URL", "http://127.0.0.1:8080")
	t.Setenv("OZ_RELAYER_ID", "sepolia")
	t.Setenv("OZ_API_KEY", strings.Repeat("k", 32))
	t.Setenv("REWARD_ACCOUNT", "0xda0b76cc92e94a6f972Ed29C104BBe8d73c2f753")
	t.Setenv("HEALTH_TOKEN", strings.Repeat("h", 32))
	t.Setenv("RELAYER_FEE", "99") // Obsolete environment settings must not override relayer.config.json.

	cfg, err := LoadConfig("relayer.config.example.json")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.RPCURLs) != 2 || cfg.RPCURLs[1] != "https://rpc-b.invalid" {
		t.Fatalf("HTTP_RPC_URL JSON array not loaded: %v", cfg.RPCURLs)
	}
	if len(cfg.OraclePriceTokens) != 1 {
		t.Fatalf("Sepolia should configure only DAI price, got %#v", cfg.OraclePriceTokens)
	}
	if got := cfg.OraclePriceTokens["dai"].Address; got != common.HexToAddress("0x6B175474E89094C44Da98b954EedeAC495271d0F") {
		t.Fatalf("Sepolia DAI oracle address=%s", got.Hex())
	}
	if cfg.ServiceFeeBPS != 36 || cfg.MaxGasLimit != 420000 || cfg.GasLimitBufferBPS != 11500 || cfg.GasFeeToleranceBPS != 500 {
		t.Fatalf("unexpected selected gas policy: %+v", cfg)
	}
	wantMaxGasPrice := big.NewInt(1_000_000_000_000)
	if cfg.MaxGasPriceWei.Cmp(wantMaxGasPrice) != 0 {
		t.Fatalf("max gas price=%s, want %s", cfg.MaxGasPriceWei, wantMaxGasPrice)
	}
}

func TestRelayerConfigVersionsMatchClassicUI(t *testing.T) {
	fileCfg, err := loadFileConfig("relayer.config.example.json")
	if err != nil {
		t.Fatalf("load relayer config: %v", err)
	}
	for network, cfg := range fileCfg.Networks {
		want := "5.2.1"
		if network == "netId1" {
			want = "4.1.6"
		}
		if cfg.Version != want {
			t.Fatalf("%s version=%q, want %q", network, cfg.Version, want)
		}
	}
}

func TestClassicUIVersionValidation(t *testing.T) {
	for _, test := range []struct {
		version string
		valid   bool
	}{
		{version: "5.2.0", valid: true},
		{version: "4.1.6", valid: true},
		{version: "5.2.1", valid: true},
		{version: "5.2.1-beta.1", valid: false},
	} {
		err := validateConfiguredVersion(test.version)
		if (err == nil) != test.valid {
			t.Fatalf("version=%s valid=%v err=%v", test.version, test.valid, err)
		}
	}
}

func TestConfigFileRejectsWritableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions are not available")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"networks":{},"instances":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFileConfig(path); err == nil || !strings.Contains(err.Error(), "must not be writable") {
		t.Fatalf("writable config error=%v", err)
	}
}

func TestPrivateFilesRejectBroadPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions are not available")
	}
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod secret: %v", err)
	}
	if err := requirePrivateFile(path, "test secret"); err == nil {
		t.Fatal("group/world-readable secret should be rejected")
	}
	if _, err := NewSQLiteStore(path, "1", time.Hour, 100, 1000); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("broad SQLite permissions should be rejected, got %v", err)
	}
}

func TestOraclePricesFollowConfiguredInstancesAndNetworkExtras(t *testing.T) {
	fileCfg, err := loadFileConfig("relayer.config.example.json")
	if err != nil {
		t.Fatalf("load relayer config: %v", err)
	}
	mainnet, err := mergeOraclePriceTokens(
		fileCfg.Instances["netId1"],
		fileCfg.Networks["netId1"].OraclePriceTokens,
	)
	if err != nil {
		t.Fatalf("merge mainnet oracle tokens: %v", err)
	}
	if len(mainnet) != 6 {
		t.Fatalf("mainnet oracle token count=%d, want six classic-ui prices", len(mainnet))
	}
	if mainnet["torn"].Address != common.HexToAddress("0x77777FeDdddFfC19Ff86DB637967013e6C6A116C") {
		t.Fatalf("unexpected mainnet TORN oracle token: %#v", mainnet["torn"])
	}
	sepolia, err := mergeOraclePriceTokens(
		fileCfg.Instances["netId11155111"],
		fileCfg.Networks["netId11155111"].OraclePriceTokens,
	)
	if err != nil {
		t.Fatalf("merge Sepolia oracle tokens: %v", err)
	}
	if len(sepolia) != 1 || sepolia["dai"].Address != common.HexToAddress("0x6B175474E89094C44Da98b954EedeAC495271d0F") {
		t.Fatalf("unexpected Sepolia oracle tokens: %#v", sepolia)
	}
	for _, netID := range []uint64{56, 61, 100, 137, 42161, 43114} {
		key := fmt.Sprintf("netId%d", netID)
		tokens, err := mergeOraclePriceTokens(fileCfg.Instances[key], fileCfg.Networks[key].OraclePriceTokens)
		if err != nil {
			t.Fatalf("merge %s oracle tokens: %v", key, err)
		}
		if len(tokens) != 0 {
			t.Fatalf("%s should not fetch prices without configured ERC20 tokens", key)
		}
	}
}

func TestConfiguredOraclePriceTokensRejectInvalidValues(t *testing.T) {
	_, err := configuredOraclePriceTokens(map[string]OraclePriceTokenConfig{
		"torn": {TokenAddress: "not-an-address", Decimals: 18},
	})
	if err == nil || !strings.Contains(err.Error(), "torn.tokenAddress") {
		t.Fatalf("expected invalid token address error, got %v", err)
	}

	_, err = configuredOraclePriceTokens(map[string]OraclePriceTokenConfig{
		"torn": {TokenAddress: "0x77777FeDdddFfC19Ff86DB637967013e6C6A116C"},
	})
	if err == nil || !strings.Contains(err.Error(), "torn.decimals") {
		t.Fatalf("expected invalid decimals error, got %v", err)
	}
}

func TestParseDotEnvLine(t *testing.T) {
	tests := []struct {
		line  string
		key   string
		value string
		ok    bool
	}{
		{line: "TORNADO_PROXY=0xabc", key: "TORNADO_PROXY", value: "0xabc", ok: true},
		{line: "export NET_ID=1", key: "NET_ID", value: "1", ok: true},
		{line: `HTTP_RPC_URL="https://mainnet.infura.io/v3/key"`, key: "HTTP_RPC_URL", value: "https://mainnet.infura.io/v3/key", ok: true},
		{line: "APP_PORT=8000 # http port", key: "APP_PORT", value: "8000", ok: true},
		{line: "# comment", ok: false},
	}

	for _, tt := range tests {
		key, value, ok := parseDotEnvLine(tt.line)
		if ok != tt.ok || key != tt.key || value != tt.value {
			t.Fatalf("parseDotEnvLine(%q)=(%q,%q,%v), want (%q,%q,%v)", tt.line, key, value, ok, tt.key, tt.value, tt.ok)
		}
	}
}

func TestLowLevelDotEnvParserDoesNotEnforcePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions are not available")
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("DOTENV_BROAD_PERMISSION_TEST=loaded\n"), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	t.Setenv("DOTENV_BROAD_PERMISSION_TEST", "")
	loaded, err := loadDotEnvFile(path)
	if err != nil || !loaded || os.Getenv("DOTENV_BROAD_PERMISSION_TEST") != "loaded" {
		t.Fatalf("load broad-permission .env: loaded=%v value=%q err=%v", loaded, os.Getenv("DOTENV_BROAD_PERMISSION_TEST"), err)
	}
}

func TestLoadInstancesRequiresLowercaseTokenKeys(t *testing.T) {
	poolA := "0x1111111111111111111111111111111111111111"
	poolB := "0x2222222222222222222222222222222222222222"
	chains := ChainInstances{"netId1": {
		"dai": {InstanceAddress: map[string]string{"1": poolA}, Symbol: "DAI", Decimals: 18, TokenAddress: "0x3333333333333333333333333333333333333333", OracleTokenAddress: "0x4444444444444444444444444444444444444444"},
		"DAI": {InstanceAddress: map[string]string{"10": poolB}, Symbol: "DAI", Decimals: 18, TokenAddress: "0x3333333333333333333333333333333333333333", OracleTokenAddress: "0x4444444444444444444444444444444444444444"},
	}}
	if _, _, err := loadInstances(chains, 1); err == nil || !strings.Contains(err.Error(), "must be lowercase") {
		t.Fatalf("uppercase token key error=%v", err)
	}
}

func TestLoadInstancesSelectsNetID(t *testing.T) {
	chains := ChainInstances{
		"netId1": {
			"eth": {
				InstanceAddress: map[string]string{"1": "0x47CE0C6eD5B0Ce3d3A51fdb1C52DC66a7c3c2936"},
				Symbol:          "ETH",
				Decimals:        18,
			},
		},
		"netId56": {
			"bnb": {
				InstanceAddress: map[string]string{"1": "0xd47438C816c9E7f2E2888E060936a499Af9582b3"},
				Symbol:          "BNB",
				Decimals:        18,
			},
		},
		"netId11155111": {
			"dai": {
				InstanceAddress:    map[string]string{"100": "0x6921fd1a97441dd603a997ED6DDF388658daf754"},
				TokenAddress:       "0xFF34B3d4Aee8ddCd6F9AFFFB6Fe49bD371b8a357",
				OracleTokenAddress: "0x6B175474E89094C44Da98b954EedeAC495271d0F",
				Symbol:             "DAI",
				Decimals:           18,
			},
		},
	}

	instances, metadata, err := loadInstances(chains, 56)
	if err != nil {
		t.Fatalf("load netId56 instances: %v", err)
	}
	if _, ok := instances["bnb"]; !ok {
		t.Fatalf("expected bnb instances for netId56, got %+v", instances)
	}
	if _, ok := instances["eth"]; ok {
		t.Fatalf("netId56 should not include netId1 eth instances")
	}
	if _, ok := metadata[common.HexToAddress("0xd47438C816c9E7f2E2888E060936a499Af9582b3")]; !ok {
		t.Fatalf("expected bnb pool to be allowed")
	}

	if _, _, err := loadInstances(chains, 137); err == nil {
		t.Fatalf("missing netId137 should fail")
	}

	_, _, err = loadInstances(chains, 11155111)
	if err != nil {
		t.Fatalf("load netId11155111 instances: %v", err)
	}
}

func productionRuntimeConfig() Config {
	return Config{
		Host:                    "127.0.0.1",
		RPCURLs:                 []string{"https://rpc-a.invalid"},
		Port:                    "8000",
		MaxGasLimit:             420000,
		MaxGasPriceWei:          big.NewInt(1_000_000_000_000),
		GasLimitBufferBPS:       11500,
		PollPeriod:              5 * time.Second,
		JobTimeout:              time.Minute,
		PriceUpdatePeriod:       time.Minute,
		IdempotencyTTL:          time.Hour,
		MaxActiveJobs:           100,
		MaxProtectedJobs:        1000,
		MaxPreflightConcurrency: 8,
		SQLitePath:              "relayer.db",
		OZRelayerURL:            "http://127.0.0.1:8080",
		OZRelayerID:             "mainnet",
		OZAPIKey:                strings.Repeat("k", 32),
		OZTxValidity:            time.Hour,
		InstanceByAddress:       map[common.Address]InstanceMeta{},
		CORSOrigins:             []string{"http://127.0.0.1:3000"},
		ChainID:                 big.NewInt(1),
		RewardAccount:           common.HexToAddress("0x0000000000000000000000000000000000000002"),
		OffchainOracle:          common.HexToAddress("0x0000000000000000000000000000000000000003"),
		RelayRatePerMinute:      60,
		RelayRateBurst:          5,
		RelayRateMaxClients:     10000,
		HealthToken:             strings.Repeat("h", 32),
		ClientIPHeader:          "CF-Connecting-IP",
		ClientIPMode:            "cloudflare",
		OracleRPCURL:            "https://oracle.invalid",
		Version:                 "5.2.0",
	}
}

func TestValidateRuntimeConfigAllowsMainnetV5GasPolicy(t *testing.T) {
	if err := validateRuntimeConfig(productionRuntimeConfig()); err != nil {
		t.Fatalf("production gas policy should pass validation: %v", err)
	}
}

func TestValidateRuntimeConfigRequiresOZSettings(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"OZ_RELAYER_ID":  func(c *Config) { c.OZRelayerID = "" },
		"OZ_API_KEY":     func(c *Config) { c.OZAPIKey = "short" },
		"OZ_RELAYER_URL": func(c *Config) { c.OZRelayerURL = "ftp://x" },
		"HTTP_RPC_URL":   func(c *Config) { c.RPCURLs = nil },
	} {
		cfg := productionRuntimeConfig()
		mutate(&cfg)
		if err := validateRuntimeConfig(cfg); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s: want validation error, got %v", name, err)
		}
	}
}

func TestHTTPConfigRequiresLoopbackBind(t *testing.T) {
	cfg := productionRuntimeConfig()
	cfg.Host = "0.0.0.0"
	if err := validateRuntimeConfig(cfg); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("non-loopback bind should fail, got %v", err)
	}
	cfg.Host = "127.0.0.1"
	if err := validateRuntimeConfig(cfg); err != nil {
		t.Fatalf("loopback bind should pass: %v", err)
	}
}

func TestHTTPConfigRejectsZeroPortWithLeadingZeros(t *testing.T) {
	cfg := productionRuntimeConfig()
	cfg.Port = "00"
	if err := validateRuntimeConfig(cfg); err == nil || !strings.Contains(err.Error(), "APP_PORT") {
		t.Fatalf("zero port with leading zeros should fail, got %v", err)
	}
}

func TestRuntimeConfigRejectsPlainHTTPRemoteOZ(t *testing.T) {
	cfg := productionRuntimeConfig()
	cfg.OZRelayerURL = "http://10.0.0.2:8080"
	if err := validateRuntimeConfig(cfg); err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("remote plain HTTP OZ URL should fail, got %v", err)
	}
	cfg.OZRelayerURL = "http://127.0.0.1:8080"
	if err := validateRuntimeConfig(cfg); err != nil {
		t.Fatalf("loopback plain HTTP OZ URL should be accepted: %v", err)
	}
}
