package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

type FileConfig struct {
	Instances ChainInstances `json:"instances"`
	Networks  ChainNetworks  `json:"networks"`
}

type ChainNetworks map[string]ChainNetworkConfig

type ChainNetworkConfig struct {
	Version           string                            `json:"version"`
	ServiceFeeBPS     *uint64                           `json:"serviceFeeBps"`
	MaxGasLimit       uint64                            `json:"maxGasLimit"`
	MaxGasPriceGwei   string                            `json:"maxGasPriceGwei"`
	OraclePriceTokens map[string]OraclePriceTokenConfig `json:"oraclePriceTokens,omitempty"`
}

type OraclePriceTokenConfig struct {
	TokenAddress string `json:"tokenAddress"`
	Decimals     uint8  `json:"decimals"`
}

// Chain IDs the relayer treats specially.
const (
	chainIDEthereumMainnet = 1
	chainIDOptimism        = 10
	chainIDEthereumClassic = 61
)

// Defaults for environment settings.
const (
	defaultGasLimitBufferPercent  = uint64(15)
	defaultGasFeeTolerancePercent = uint64(5)
	defaultPriceUpdateSeconds     = uint64(30)

	defaultAppPort                 = "8000"
	defaultAppHost                 = "127.0.0.1"
	defaultSQLitePath              = "relayer-oz.db"
	defaultClientIPHeader          = "CF-Connecting-IP"
	defaultOffchainOracleAddress   = "0x07D91f5fb9Bf7798734C3f606dB065549F6893bb"
	defaultOZTxValidMinutes        = uint64(10)
	defaultPollSeconds             = uint64(5)
	defaultJobTimeoutSeconds       = uint64(300)
	defaultMaxActiveJobs           = uint64(100)
	defaultMaxProtectedJobs        = uint64(1000)
	defaultMaxPreflightConcurrency = uint64(8)
	defaultIdempotencyTTLSeconds   = uint64(604800) // 7 days
	defaultRelayRatePerMinute      = uint64(60)
	defaultRelayRateBurst          = uint64(5)
	defaultRelayRateMaxClients     = uint64(10000)
)

// Bounds enforced by validateRuntimeConfig.
const (
	bpsPerPercent         = 100
	minGasLimitBufferBPS  = 11_000 // 10% buffer
	maxGasLimitBufferBPS  = 12_000 // 20% buffer
	maxGasFeeToleranceBPS = 2_000  // 20%
	minOZTxValidity       = 10 * time.Minute
	maxOZTxValidity       = 8 * time.Hour
	minSecretLength       = 32
)

// LoadConfig reads the JSON relayer config at path, combines it with the
// environment settings and returns the validated runtime Config.
func LoadConfig(path string) (Config, error) {
	if err := validateUint64Environment(); err != nil {
		return Config{}, err
	}
	fileCfg, err := loadFileConfig(path)
	if err != nil {
		return Config{}, err
	}
	netID, chainID, err := loadChainID()
	if err != nil {
		return Config{}, err
	}
	chainKey := fmt.Sprintf("netId%d", netID)
	networkCfg, maxGasPriceWei, err := loadNetworkConfig(fileCfg.Networks, chainKey, netID)
	if err != nil {
		return Config{}, err
	}
	rewardAccount, offchainOracle, err := loadAccountAddresses()
	if err != nil {
		return Config{}, err
	}
	instances, instanceByAddress, err := loadInstances(fileCfg.Instances, netID)
	if err != nil {
		return Config{}, err
	}
	oraclePriceTokens, err := mergeOraclePriceTokens(instances, networkCfg.OraclePriceTokens)
	if err != nil {
		return Config{}, fmt.Errorf("networks.%s.oraclePriceTokens: %w", chainKey, err)
	}
	rpcURLs, err := loadRPCURLs()
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		RPCURLs:           rpcURLs,
		ChainID:           chainID,
		RewardAccount:     rewardAccount,
		OffchainOracle:    offchainOracle,
		Instances:         instances,
		InstanceByAddress: instanceByAddress,
		OraclePriceTokens: oraclePriceTokens,
		MaxGasLimit:       networkCfg.MaxGasLimit,
		MaxGasPriceWei:    maxGasPriceWei,
		ServiceFeeBPS:     *networkCfg.ServiceFeeBPS,
		PriceUpdatePeriod: time.Duration(defaultPriceUpdateSeconds) * time.Second,
		Version:           networkCfg.Version,
	}
	if err := applyEnvSettings(&cfg); err != nil {
		return Config{}, err
	}
	if err := validateRuntimeConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loadChainID reads NET_ID and rejects missing or unsupported chains.
func loadChainID() (uint64, *big.Int, error) {
	netID := uint64Setting("NET_ID", 0)
	chainID := new(big.Int).SetUint64(netID)
	if chainID.Sign() <= 0 {
		return 0, nil, errors.New("NET_ID must be a positive integer")
	}
	if chainID.Uint64() == chainIDOptimism {
		return 0, nil, errors.New("NET_ID=10 (Optimism) is not supported by this relayer")
	}
	if chainID.Uint64() == chainIDEthereumClassic {
		return 0, nil, errors.New("NET_ID=61 (Ethereum Classic) is not supported because this relayer requires EIP-1559 type-2 transactions")
	}
	return netID, chainID, nil
}

// loadNetworkConfig selects and validates networks.<chainKey> and parses its
// gas price cap.
func loadNetworkConfig(networks ChainNetworks, chainKey string, netID uint64) (ChainNetworkConfig, *big.Int, error) {
	networkCfg, ok := networks[chainKey]
	if !ok {
		return ChainNetworkConfig{}, nil, fmt.Errorf("networks.%s is required for NET_ID=%d", chainKey, netID)
	}
	if err := validateChainNetworkConfig(networkCfg); err != nil {
		return ChainNetworkConfig{}, nil, fmt.Errorf("networks.%s: %w", chainKey, err)
	}
	maxGasPriceWei, err := decimalGweiToWei("maxGasPriceGwei", networkCfg.MaxGasPriceGwei)
	if err != nil {
		return ChainNetworkConfig{}, nil, fmt.Errorf("networks.%s: %w", chainKey, err)
	}
	return networkCfg, maxGasPriceWei, nil
}

// loadAccountAddresses reads REWARD_ACCOUNT and OFFCHAIN_ORACLE_ADDRESS.
func loadAccountAddresses() (rewardAccount, offchainOracle common.Address, err error) {
	reward := stringSetting("REWARD_ACCOUNT", "")
	if reward == "" {
		return common.Address{}, common.Address{}, errors.New("REWARD_ACCOUNT is required")
	}
	if !common.IsHexAddress(reward) {
		return common.Address{}, common.Address{}, errors.New("REWARD_ACCOUNT must be an Ethereum address")
	}
	oracle := stringSetting("OFFCHAIN_ORACLE_ADDRESS", defaultOffchainOracleAddress)
	if !common.IsHexAddress(oracle) {
		return common.Address{}, common.Address{}, errors.New("OFFCHAIN_ORACLE_ADDRESS must be an Ethereum address")
	}
	return common.HexToAddress(reward), common.HexToAddress(oracle), nil
}

// loadRPCURLs rejects the removed RPC/signing variables and parses
// HTTP_RPC_URL.
func loadRPCURLs() ([]string, error) {
	if os.Getenv("HTTP_RPC_URLS") != "" || os.Getenv("PRIVATE_KEY") != "" {
		return nil, errors.New("HTTP_RPC_URLS and PRIVATE_KEY are no longer supported: signing and sending are done by OpenZeppelin Relayer; set HTTP_RPC_URL, OZ_RELAYER_URL, OZ_RELAYER_ID and OZ_API_KEY")
	}
	if os.Getenv("HTTP_RPC_URL_2") != "" {
		return nil, errors.New(`HTTP_RPC_URL_2 is no longer supported: list both endpoints in HTTP_RPC_URL as a JSON array, e.g. HTTP_RPC_URL=["https://a","https://b"]`)
	}
	return parseRPCURLs(os.Getenv("HTTP_RPC_URL"))
}

// applyEnvSettings fills the Config fields that come straight from
// environment variables (or their defaults).
func applyEnvSettings(cfg *Config) error {
	trustedProxyCIDRs, err := cidrSetting("TRUSTED_PROXY_CIDRS", []string{"127.0.0.1/32", "::1/128"})
	if err != nil {
		return err
	}
	cfg.TrustedProxyCIDRs = trustedProxyCIDRs
	cfg.Host = stringSetting("APP_HOST", defaultAppHost)
	cfg.Port = stringSetting("APP_PORT", defaultAppPort)
	cfg.SQLitePath = stringSetting("SQLITE_PATH", defaultSQLitePath)
	cfg.OracleRPCURL = os.Getenv("ORACLE_RPC_URL")
	cfg.OZRelayerURL = strings.TrimRight(strings.TrimSpace(os.Getenv("OZ_RELAYER_URL")), "/")
	cfg.OZRelayerID = strings.TrimSpace(os.Getenv("OZ_RELAYER_ID"))
	cfg.OZAPIKey = strings.TrimSpace(os.Getenv("OZ_API_KEY"))
	cfg.OZTxValidity = time.Duration(uint64Setting("OZ_TX_VALID_MINUTES", defaultOZTxValidMinutes)) * time.Minute
	cfg.GasLimitBufferBPS = bpsDenominator + bpsPerPercent*uint64Setting("GAS_LIMIT_BUFFER_PERCENT", defaultGasLimitBufferPercent)
	cfg.GasFeeToleranceBPS = bpsPerPercent * uint64Setting("GAS_FEE_TOLERANCE_PERCENT", defaultGasFeeTolerancePercent)
	cfg.PollPeriod = time.Duration(uint64Setting("POLL_SECONDS", defaultPollSeconds)) * time.Second
	cfg.JobTimeout = time.Duration(uint64Setting("JOB_TIMEOUT_SECONDS", defaultJobTimeoutSeconds)) * time.Second
	cfg.MaxActiveJobs = uint64Setting("MAX_ACTIVE_JOBS", defaultMaxActiveJobs)
	cfg.MaxProtectedJobs = uint64Setting("MAX_PROTECTED_JOBS", defaultMaxProtectedJobs)
	cfg.MaxPreflightConcurrency = uint64Setting("MAX_PREFLIGHT_CONCURRENCY", defaultMaxPreflightConcurrency)
	cfg.IdempotencyTTL = time.Duration(uint64Setting("IDEMPOTENCY_TTL_SECONDS", defaultIdempotencyTTLSeconds)) * time.Second
	cfg.CORSOrigins = splitCSV(os.Getenv("CORS_ORIGINS"))
	cfg.ClientIPHeader = stringSetting("CLIENT_IP_HEADER", defaultClientIPHeader)
	cfg.ClientIPMode = strings.ToLower(stringSetting("CLIENT_IP_MODE", clientIPModeCloudflare))
	cfg.HealthToken = os.Getenv("HEALTH_TOKEN")
	cfg.RelayRatePerMinute = uint64Setting("RELAY_RATE_PER_MINUTE", defaultRelayRatePerMinute)
	cfg.RelayRateBurst = uint64Setting("RELAY_RATE_BURST", defaultRelayRateBurst)
	cfg.RelayRateMaxClients = uint64Setting("RELAY_RATE_MAX_CLIENTS", defaultRelayRateMaxClients)
	return nil
}

func loadFileConfig(path string) (FileConfig, error) {
	if strings.TrimSpace(path) == "" {
		return FileConfig{}, errors.New("config path is required")
	}

	if err := requireReadOnlyConfigFile(path); err != nil {
		return FileConfig{}, fmt.Errorf("config %q is not secure: %w", path, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg FileConfig
	if err := decodeStrictJSON(bytes.NewReader(body), &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	return cfg, nil
}

var uint64EnvironmentSettings = []string{
	"NET_ID", "OZ_TX_VALID_MINUTES", "GAS_LIMIT_BUFFER_PERCENT", "GAS_FEE_TOLERANCE_PERCENT",
	"POLL_SECONDS", "JOB_TIMEOUT_SECONDS", "MAX_ACTIVE_JOBS", "MAX_PROTECTED_JOBS",
	"MAX_PREFLIGHT_CONCURRENCY", "IDEMPOTENCY_TTL_SECONDS", "RELAY_RATE_PER_MINUTE",
	"RELAY_RATE_BURST", "RELAY_RATE_MAX_CLIENTS",
}

const maxNumericSetting = 100_000_000

// validateUint64Environment checks that every set numeric environment
// setting is a plain base-10 uint64 and, except NET_ID, at most
// maxNumericSetting.
func validateUint64Environment() error {
	for _, name := range uint64EnvironmentSettings {
		n, set, err := parseUint64Env(name)
		if !set {
			continue
		}
		if err != nil {
			return fmt.Errorf("%s must be a uint64", name)
		}
		// Settings are scaled (percent*100, seconds/minutes to Duration,
		// int conversion); a bound keeps that from wrapping around and
		// slipping past the range checks.
		if name != "NET_ID" && n > maxNumericSetting {
			return fmt.Errorf("%s must be at most %d", name, maxNumericSetting)
		}
	}
	return nil
}

// parseUint64Env parses environment variable name as a base-10 uint64
// (no sign). set is false when the variable is unset or empty.
func parseUint64Env(name string) (value uint64, set bool, err error) {
	raw := os.Getenv(name)
	if raw == "" {
		return 0, false, nil
	}
	value, err = strconv.ParseUint(raw, 10, 64)
	return value, true, err
}

// uint64Setting returns numeric setting name, or defaultValue when it is
// unset. Every name is listed in uint64EnvironmentSettings, so
// validateUint64Environment has already rejected invalid values.
func uint64Setting(name string, defaultValue uint64) uint64 {
	n, set, _ := parseUint64Env(name)
	if !set {
		return defaultValue
	}
	return n
}

func stringSetting(name string, defaultValue string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return defaultValue
}

func cidrSetting(name string, defaults []string) ([]*net.IPNet, error) {
	values := defaults
	if raw := os.Getenv(name); raw != "" {
		values = splitCSV(raw)
	}
	result := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("%s contains invalid CIDR %q", name, value)
		}
		result = append(result, network)
	}
	return result, nil
}

// validateRuntimeConfig checks a fully assembled Config. Checks run in a
// fixed order so the first failing one is reported.
func validateRuntimeConfig(cfg Config) error {
	checks := []func(Config) error{
		validateCoreConfig,
		validateGasConfig,
		validatePollingConfig,
		validateOZConfig,
		validateLimits,
		validateHealthToken,
		validateHTTPConfig,
		validateClientIPConfig,
		validateStorageConfig,
		validateRPCConfig,
	}
	for _, check := range checks {
		if err := check(cfg); err != nil {
			return err
		}
	}
	if slices.Contains(cfg.CORSOrigins, "*") {
		logWarnf("corsOrigins contains *; restrict it for production deployments")
	}
	return nil
}

// validateCoreConfig checks the chain ID and the configured addresses.
func validateCoreConfig(cfg Config) error {
	if cfg.ChainID == nil || cfg.ChainID.Sign() <= 0 {
		return errors.New("NET_ID must be a positive integer")
	}
	if cfg.RewardAccount == (common.Address{}) {
		return errors.New("REWARD_ACCOUNT must not be the zero address")
	}
	if cfg.OffchainOracle == (common.Address{}) {
		return errors.New("OFFCHAIN_ORACLE_ADDRESS must not be the zero address")
	}
	return nil
}

// validateGasConfig checks the gas limit, gas price cap, buffer and tolerance.
func validateGasConfig(cfg Config) error {
	if cfg.MaxGasLimit == 0 {
		return errors.New("maxGasLimit must be greater than 0")
	}
	if cfg.MaxGasPriceWei == nil || cfg.MaxGasPriceWei.Sign() <= 0 {
		return errors.New("maxGasPriceGwei must be greater than 0")
	}
	if cfg.GasLimitBufferBPS < minGasLimitBufferBPS || cfg.GasLimitBufferBPS > maxGasLimitBufferBPS {
		return errors.New("GAS_LIMIT_BUFFER_PERCENT must be between 10 and 20")
	}
	if cfg.GasFeeToleranceBPS > maxGasFeeToleranceBPS {
		return errors.New("GAS_FEE_TOLERANCE_PERCENT must be between 0 and 20")
	}
	return nil
}

// validatePollingConfig checks the poll period and job timeout.
func validatePollingConfig(cfg Config) error {
	if cfg.PollPeriod <= 0 {
		return errors.New("POLL_SECONDS must be greater than 0")
	}
	if cfg.JobTimeout <= 0 {
		return errors.New("JOB_TIMEOUT_SECONDS must be greater than 0")
	}
	return nil
}

// validateOZConfig checks the OpenZeppelin Relayer connection settings.
func validateOZConfig(cfg Config) error {
	if cfg.OZTxValidity < minOZTxValidity || cfg.OZTxValidity > maxOZTxValidity {
		return errors.New("OZ_TX_VALID_MINUTES must be between 10 and 480")
	}
	if err := validateRPCURL("OZ_RELAYER_URL", cfg.OZRelayerURL); err != nil {
		return err
	}
	if cfg.OZRelayerID == "" {
		return errors.New("OZ_RELAYER_ID is required")
	}
	if len(cfg.OZAPIKey) < minSecretLength {
		return errors.New("OZ_API_KEY is required and must contain at least 32 characters")
	}
	if u, err := url.Parse(cfg.OZRelayerURL); err == nil && strings.EqualFold(u.Scheme, "http") {
		if ip := net.ParseIP(u.Hostname()); (ip == nil || !ip.IsLoopback()) && !strings.EqualFold(u.Hostname(), "localhost") {
			return errors.New("OZ_RELAYER_URL must use https unless it targets localhost or a loopback IP")
		}
	}
	return nil
}

// validateLimits checks periods, job capacity and rate-limit settings.
func validateLimits(cfg Config) error {
	if cfg.PriceUpdatePeriod <= 0 {
		return errors.New("internal price update period must be greater than 0")
	}
	if cfg.IdempotencyTTL <= 0 {
		return errors.New("IDEMPOTENCY_TTL_SECONDS must be greater than 0")
	}
	if cfg.MaxActiveJobs == 0 {
		return errors.New("MAX_ACTIVE_JOBS must be greater than 0")
	}
	if cfg.MaxProtectedJobs < cfg.MaxActiveJobs {
		return errors.New("MAX_PROTECTED_JOBS must be greater than or equal to MAX_ACTIVE_JOBS")
	}
	if cfg.MaxPreflightConcurrency == 0 {
		return errors.New("MAX_PREFLIGHT_CONCURRENCY must be greater than 0")
	}
	if cfg.RelayRatePerMinute == 0 || cfg.RelayRateBurst == 0 {
		return errors.New("RELAY_RATE_PER_MINUTE and RELAY_RATE_BURST must be greater than 0")
	}
	if cfg.RelayRateMaxClients == 0 {
		return errors.New("RELAY_RATE_MAX_CLIENTS must be greater than 0")
	}
	return nil
}

// validateHealthToken requires a long, non-placeholder HEALTH_TOKEN.
func validateHealthToken(cfg Config) error {
	if len(cfg.HealthToken) < minSecretLength {
		return errors.New("HEALTH_TOKEN is required and must contain at least 32 characters")
	}
	if strings.HasPrefix(strings.ToLower(cfg.HealthToken), "replace-") {
		return errors.New("HEALTH_TOKEN still holds the .env.example placeholder; generate a random value")
	}
	return nil
}

// validateClientIPConfig checks the client IP header and extraction mode.
func validateClientIPConfig(cfg Config) error {
	if strings.TrimSpace(cfg.ClientIPHeader) == "" {
		return errors.New("CLIENT_IP_HEADER must not be empty")
	}
	switch cfg.ClientIPMode {
	case clientIPModeCloudflare, clientIPModeXFF, clientIPModeNone:
	default:
		return errors.New("CLIENT_IP_MODE must be cloudflare, xff or none")
	}
	return nil
}

// validateStorageConfig checks the SQLite path and the network version.
func validateStorageConfig(cfg Config) error {
	if strings.TrimSpace(cfg.SQLitePath) == "" {
		return errors.New("SQLITE_PATH is required")
	}
	if cfg.SQLitePath == ":memory:" {
		return errors.New("SQLITE_PATH=:memory: is not supported; a persistent database is required")
	}
	if strings.TrimSpace(cfg.Version) == "" {
		return errors.New("networks.netIdX.version is required")
	}
	return validateConfiguredVersion(cfg.Version)
}

// validateRPCConfig checks HTTP_RPC_URL and, when oracle price tokens are
// configured, ORACLE_RPC_URL.
func validateRPCConfig(cfg Config) error {
	// On mainnet the oracle reads reuse HTTP_RPC_URL; other chains need a
	// mainnet ORACLE_RPC_URL.
	oracleOnMainRPC := cfg.ChainID != nil && cfg.ChainID.Uint64() == chainIDEthereumMainnet && cfg.OracleRPCURL == ""
	if len(cfg.OraclePriceTokens) > 0 && cfg.OracleRPCURL == "" && !oracleOnMainRPC {
		return errors.New("ORACLE_RPC_URL is required when oracle price tokens are configured")
	}
	if len(cfg.RPCURLs) == 0 || len(cfg.RPCURLs) > maxRPCEndpoints {
		return fmt.Errorf("HTTP_RPC_URL must contain 1 to %d URLs", maxRPCEndpoints)
	}
	names, _ := cfg.rpcEndpoints()
	for i, raw := range cfg.RPCURLs {
		if err := validateRPCURL(names[i], raw); err != nil {
			return err
		}
		for _, earlier := range cfg.RPCURLs[:i] {
			if strings.EqualFold(strings.TrimRight(raw, "/"), strings.TrimRight(earlier, "/")) {
				return errors.New("HTTP_RPC_URL entries must differ")
			}
		}
	}
	if len(cfg.OraclePriceTokens) > 0 && !oracleOnMainRPC {
		if err := validateRPCURL("ORACLE_RPC_URL", cfg.OracleRPCURL); err != nil {
			return err
		}
	}
	return nil
}

const maxRPCEndpoints = 2

// parseRPCURLs reads HTTP_RPC_URL: a single URL, or a JSON array of URLs
// (["https://a","https://b"]) for round robin with failover.
func parseRPCURLs(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "[") {
		if raw == "" {
			return nil, nil
		}
		return []string{raw}, nil
	}
	var urls []string
	if err := json.Unmarshal([]byte(raw), &urls); err != nil {
		return nil, errors.New(`HTTP_RPC_URL must be a URL or a JSON array of URLs, e.g. ["https://a","https://b"]`)
	}
	for i := range urls {
		urls[i] = strings.TrimSpace(urls[i])
	}
	return urls, nil
}

// rpcEndpoints lists the configured read-only chain RPCs with their log and
// metric names. Names never contain the URL (URLs often embed API keys).
func (cfg Config) rpcEndpoints() (names, urls []string) {
	if len(cfg.RPCURLs) == 1 {
		return []string{"HTTP_RPC_URL"}, cfg.RPCURLs
	}
	for i := range cfg.RPCURLs {
		names = append(names, fmt.Sprintf("HTTP_RPC_URL[%d]", i+1))
	}
	return names, cfg.RPCURLs
}

func validateHTTPConfig(cfg Config) error {
	port, err := strconv.ParseUint(cfg.Port, 10, 16)
	if err != nil || port == 0 {
		return errors.New("APP_PORT must be an integer between 1 and 65535")
	}
	ip := net.ParseIP(strings.TrimSpace(cfg.Host))
	if ip == nil {
		return errors.New("APP_HOST must be an IP address")
	}
	if !ip.IsLoopback() {
		return errors.New("APP_HOST must be a loopback address")
	}
	return nil
}

func decodeStrictJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func validateChainNetworkConfig(cfg ChainNetworkConfig) error {
	if strings.TrimSpace(cfg.Version) == "" {
		return errors.New("version is required")
	}
	if cfg.ServiceFeeBPS == nil {
		return errors.New("serviceFeeBps is required")
	}
	if *cfg.ServiceFeeBPS >= bpsDenominator {
		return errors.New("serviceFeeBps must be less than 10000 so withdrawals retain a gas budget")
	}
	if cfg.MaxGasLimit == 0 {
		return errors.New("maxGasLimit must be greater than 0")
	}
	if strings.TrimSpace(cfg.MaxGasPriceGwei) == "" {
		return errors.New("maxGasPriceGwei is required")
	}
	if _, err := configuredOraclePriceTokens(cfg.OraclePriceTokens); err != nil {
		return fmt.Errorf("oraclePriceTokens: %w", err)
	}
	return nil
}

func configuredOraclePriceTokens(configured map[string]OraclePriceTokenConfig) (map[string]oraclePriceToken, error) {
	tokens := make(map[string]oraclePriceToken, len(configured))
	for currency, token := range configured {
		currency = strings.ToLower(strings.TrimSpace(currency))
		if currency == "" {
			return nil, errors.New("currency key must not be empty")
		}
		if !common.IsHexAddress(token.TokenAddress) {
			return nil, fmt.Errorf("%s.tokenAddress must be an Ethereum address", currency)
		}
		address := common.HexToAddress(token.TokenAddress)
		if address == (common.Address{}) {
			return nil, fmt.Errorf("%s.tokenAddress must not be the zero address", currency)
		}
		if token.Decimals == 0 {
			return nil, fmt.Errorf("%s.decimals must be greater than 0", currency)
		}
		if _, exists := tokens[currency]; exists {
			return nil, fmt.Errorf("duplicate normalized currency %s", currency)
		}
		tokens[currency] = oraclePriceToken{
			Address:  address,
			Decimals: token.Decimals,
		}
	}
	return tokens, nil
}

func mergeOraclePriceTokens(instances InstancesConfig, configured map[string]OraclePriceTokenConfig) (map[string]oraclePriceToken, error) {
	tokens := oraclePriceTokensFromInstances(instances)
	extra, err := configuredOraclePriceTokens(configured)
	if err != nil {
		return nil, err
	}
	for currency, token := range extra {
		if _, exists := tokens[currency]; exists {
			return nil, fmt.Errorf("configured oracle token %s duplicates an instance price source", currency)
		}
		tokens[currency] = token
	}
	return tokens, nil
}

func decimalGweiToWei(name, value string) (*big.Int, error) {
	rate, ok := new(big.Rat).SetString(strings.TrimSpace(value))
	if !ok || rate.Sign() < 0 {
		return nil, fmt.Errorf("%s must be a non-negative decimal string", name)
	}
	rate.Mul(rate, big.NewRat(1_000_000_000, 1))
	if !rate.IsInt() {
		return nil, fmt.Errorf("%s supports at most 9 decimal places", name)
	}
	return new(big.Int).Set(rate.Num()), nil
}

// stableSemverPattern matches MAJOR.MINOR.PATCH without leading zeros, signs
// or pre-release/build suffixes.
var stableSemverPattern = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`)

// validateConfiguredVersion requires a stable semantic version (e.g. 5.2.0).
func validateConfiguredVersion(version string) error {
	if !stableSemverPattern.MatchString(version) {
		return errors.New("network version must be a stable semantic version")
	}
	return nil
}

// loadInstances validates instances.netId<netID> and indexes every pool
// instance address to its metadata.
func loadInstances(allInstances ChainInstances, netID uint64) (InstancesConfig, map[common.Address]InstanceMeta, error) {
	chainKey := fmt.Sprintf("netId%d", netID)
	instances, ok := allInstances[chainKey]
	if !ok {
		return nil, nil, fmt.Errorf("instances.%s is required for NET_ID=%d", chainKey, netID)
	}
	if len(instances) == 0 {
		return nil, nil, fmt.Errorf("instances.%s in relayer config must contain at least one pool", chainKey)
	}

	metaByAddress := make(map[common.Address]InstanceMeta)
	seen := make(map[common.Address]string)
	for token, instance := range instances {
		if err := validateInstanceConfig(token, instance); err != nil {
			return nil, nil, err
		}
		if err := addInstanceAddresses(token, instance, seen, metaByAddress); err != nil {
			return nil, nil, err
		}
	}
	return instances, metaByAddress, nil
}

// validateInstanceConfig checks the token-level fields of one instance entry.
func validateInstanceConfig(token string, instance InstanceConfig) error {
	// Token keys are lowercase so oracle price maps (keyed lowercase) can
	// never collide; this also rules out keys differing only by case.
	normalizedToken := strings.ToLower(strings.TrimSpace(token))
	if normalizedToken == "" {
		return fmt.Errorf("relayer config contains an empty token key")
	}
	if normalizedToken != token {
		return fmt.Errorf("relayer config token key %q must be lowercase without surrounding whitespace", token)
	}
	if len(instance.InstanceAddress) == 0 {
		return fmt.Errorf("relayer config token %s must contain instanceAddress map", token)
	}
	if instance.Symbol == "" {
		return fmt.Errorf("relayer config token %s must contain symbol", token)
	}
	if instance.Decimals == 0 {
		return fmt.Errorf("relayer config token %s must contain positive decimals", token)
	}
	if instance.TokenAddress != "" && !common.IsHexAddress(instance.TokenAddress) {
		return fmt.Errorf("invalid tokenAddress for %s: %s", token, instance.TokenAddress)
	}
	if instance.OracleTokenAddress != "" && !common.IsHexAddress(instance.OracleTokenAddress) {
		return fmt.Errorf("invalid oracleTokenAddress for %s: %s", token, instance.OracleTokenAddress)
	}
	if instance.TokenAddress != "" && instance.OracleTokenAddress == "" {
		return fmt.Errorf("relayer config ERC20 token %s must contain oracleTokenAddress", token)
	}
	if instance.OracleTokenAddress != "" && instance.TokenAddress == "" {
		return fmt.Errorf("oracleTokenAddress for %s requires tokenAddress", token)
	}
	if instance.TokenAddress != "" {
		if common.HexToAddress(instance.TokenAddress) == (common.Address{}) {
			return fmt.Errorf("tokenAddress for %s must not be the zero address", token)
		}
	}
	return nil
}

// addInstanceAddresses validates each denomination of one token and records
// its pool address in metaByAddress; seen detects duplicate addresses across
// tokens.
func addInstanceAddresses(token string, instance InstanceConfig, seen map[common.Address]string, metaByAddress map[common.Address]InstanceMeta) error {
	isERC20 := instance.TokenAddress != ""
	for amount, address := range instance.InstanceAddress {
		if amount == "" {
			return fmt.Errorf("relayer config contains an empty denomination key for %s", token)
		}
		if !common.IsHexAddress(address) {
			return fmt.Errorf("invalid instanceAddress for %s %s: %s", token, amount, address)
		}
		checksum := common.HexToAddress(address)
		if checksum == (common.Address{}) {
			return fmt.Errorf("instanceAddress for %s %s must not be the zero address", token, amount)
		}
		if previous, ok := seen[checksum]; ok {
			return fmt.Errorf("duplicate instanceAddress %s in %s %s and %s", checksum.Hex(), token, amount, previous)
		}
		seen[checksum] = fmt.Sprintf("%s %s", token, amount)
		meta := InstanceMeta{
			Currency: token,
			Amount:   amount,
			Symbol:   instance.Symbol,
			Decimals: instance.Decimals,
			IsERC20:  isERC20,
		}
		if isERC20 {
			meta.TokenAddress = common.HexToAddress(instance.TokenAddress)
		}
		meta, err := meta.withUnits()
		if err != nil {
			return fmt.Errorf("invalid denomination %s for %s: %w", amount, token, err)
		}
		metaByAddress[checksum] = meta
	}
	return nil
}
