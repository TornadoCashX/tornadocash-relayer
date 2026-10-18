package main

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestNativeGasBudgetERC20Units(t *testing.T) {
	tests := []struct {
		name       string
		currency   string
		decimals   uint8
		amount     string
		priceWei   string
		wantMinFee string
	}{
		{
			name:       "DAI 18 decimals",
			currency:   "dai",
			decimals:   18,
			amount:     "100",
			priceWei:   "500000000000000",
			wantMinFee: "92000000000000000",
		},
		{
			name:       "USDC 6 decimals",
			currency:   "usdc",
			decimals:   6,
			amount:     "100",
			priceWei:   "500000000000000",
			wantMinFee: "92000",
		},
		{
			name:       "WBTC 8 decimals",
			currency:   "wbtc",
			decimals:   8,
			amount:     "1",
			priceWei:   "50000000000000000000",
			wantMinFee: "50042",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			price, ok := new(big.Int).SetString(tt.priceWei, 10)
			if !ok {
				t.Fatalf("invalid price fixture")
			}
			meta := units(InstanceMeta{
				Currency: tt.currency,
				Amount:   tt.amount,
				Decimals: tt.decimals,
				IsERC20:  true,
			})
			relayer := &Relayer{
				cfg:    Config{ServiceFeeBPS: 5},
				prices: freshPrices(map[string]*big.Int{tt.currency: price}),
			}
			gas := uint64(21000)
			gasPrice := big.NewInt(1_000_000_000)
			want, ok := new(big.Int).SetString(tt.wantMinFee, 10)
			if !ok {
				t.Fatalf("invalid min fee fixture")
			}

			budget, err := relayer.nativeGasBudget(meta, want)
			if err != nil {
				t.Fatalf("native gas budget: %v", err)
			}
			wantBudget := new(big.Int).Mul(new(big.Int).SetUint64(gas), gasPrice)
			if budget.Cmp(wantBudget) != 0 {
				t.Fatalf("native budget=%s, want %s", budget, wantBudget)
			}
		})
	}
}

func TestDecimalGweiToWeiIsExact(t *testing.T) {
	got, err := decimalGweiToWei("priorityFeeGwei", "0.000000001")
	if err != nil || got.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("one wei precision: got=%v err=%v", got, err)
	}
	if _, err := decimalGweiToWei("priorityFeeGwei", "0.0000000001"); err == nil {
		t.Fatal("sub-wei Gwei value should be rejected")
	}
}

func TestGasBufferAndBPSRoundUp(t *testing.T) {
	got, err := applyGasBuffer(100001, 11000)
	if err != nil || got != 110002 {
		t.Fatalf("buffered gas=%d err=%v, want 110002", got, err)
	}
	if got := applyBPSCeil(big.NewInt(1), 36); got.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("positive fractional service fee must round up, got %s", got)
	}
}

func TestFeeBudgetCoversExpectedCostOnly(t *testing.T) {
	relayer := &Relayer{cfg: Config{MaxGasPriceWei: big.NewInt(10_000_000_000)}}
	market := big.NewInt(1_000_000_000)
	estimate := uint64(300_000)
	expected := new(big.Int).Mul(market, new(big.Int).SetUint64(estimate))

	// No tolerance: estimate x market is enough (the gas-limit buffer is not charged).
	if err := relayer.checkFeeBudget(expected, estimate, market, "test"); err != nil {
		t.Fatalf("budget equal to expected cost must pass: %v", err)
	}
	short := new(big.Int).Sub(expected, big.NewInt(1))
	var tooLow *feeTooLowError
	if err := relayer.checkFeeBudget(short, estimate, market, "test"); !errors.As(err, &tooLow) || tooLow.requiredGasNative.Cmp(expected) != 0 {
		t.Fatalf("budget below expected cost must fail with required %s, got %v", expected, err)
	}
	huge := new(big.Int).Mul(expected, big.NewInt(100))
	if err := relayer.checkFeeBudget(huge, estimate, big.NewInt(11_000_000_000), "test"); err == nil || !failureRetryable(err) {
		t.Fatalf("market above the gas price limit must be refused as retryable, got %v", err)
	}

	// 5% tolerance: 95% of the expected cost is accepted, 1 wei less is not.
	relayer.cfg.GasFeeToleranceBPS = 500
	minimum := new(big.Int).Div(new(big.Int).Mul(expected, big.NewInt(95)), big.NewInt(100))
	if err := relayer.checkFeeBudget(minimum, estimate, market, "test"); err != nil {
		t.Fatalf("budget at 95%% of expected cost must pass with 5%% tolerance: %v", err)
	}
	if err := relayer.checkFeeBudget(new(big.Int).Sub(minimum, big.NewInt(1)), estimate, market, "test"); !errors.As(err, &tooLow) || tooLow.requiredGasNative.Cmp(expected) != 0 {
		t.Fatalf("budget below tolerance must fail reporting the full expected cost %s, got %v", expected, err)
	}
}

func TestMarketMaxFeeIsNotLoweredByBudget(t *testing.T) {
	relayer := &Relayer{cfg: Config{MaxGasPriceWei: big.NewInt(10_000_000_000)}}
	if got := relayer.marketMaxFee(big.NewInt(1_000_000_000), big.NewInt(100_000_000)); got.Cmp(big.NewInt(2_100_000_000)) != 0 {
		t.Fatalf("maxFee=%s, want 2 x base fee + tip", got)
	}
	if got := relayer.marketMaxFee(big.NewInt(6_000_000_000), big.NewInt(1_000_000_000)); got.Cmp(big.NewInt(10_000_000_000)) != 0 {
		t.Fatalf("maxFee=%s, want gas price limit", got)
	}
}

func TestERC20FeeCheckRejectsStaleTokenPrice(t *testing.T) {
	meta := units(InstanceMeta{Currency: "dai", Amount: "100", Decimals: 18, IsERC20: true})
	relayer := &Relayer{
		cfg:    Config{PriceUpdatePeriod: 30 * time.Second},
		prices: pricesAt(time.Now().Add(-3*time.Minute), map[string]*big.Int{"dai": big.NewInt(1)}),
	}
	if _, err := relayer.nativeGasBudget(meta, big.NewInt(1)); err == nil || !strings.Contains(err.Error(), "not available yet") {
		t.Fatalf("stale ERC20 price should be rejected, got %v", err)
	}
}

func TestNormalizeOracleRateRejectsNonPositiveValues(t *testing.T) {
	for _, rate := range []*big.Int{nil, new(big.Int), big.NewInt(-1)} {
		if _, err := normalizeOracleRate(rate, 18); err == nil {
			t.Fatalf("non-positive Oracle rate %v should fail", rate)
		}
	}
	if price, err := normalizeOracleRate(big.NewInt(1), 18); err != nil || price.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("positive Oracle rate failed: price=%v err=%v", price, err)
	}
}

func TestOracleBlockFreshness(t *testing.T) {
	now := time.Now()
	if !oracleBlockIsFresh(uint64(now.Add(-time.Minute).Unix()), now) {
		t.Fatal("recent Oracle block should be fresh")
	}
	if oracleBlockIsFresh(uint64(now.Add(-11*time.Minute).Unix()), now) {
		t.Fatal("stale Oracle block should be rejected")
	}
	if oracleBlockIsFresh(uint64(now.Add(2*time.Minute).Unix()), now) {
		t.Fatal("far-future Oracle block should be rejected")
	}
}

func TestPriorityFeeUsesLowerObservedTip(t *testing.T) {
	gwei := func(f float64) string { return "0x" + new(big.Int).SetUint64(uint64(f*1e9)).Text(16) }
	for _, tc := range []struct {
		name    string
		history any
		histErr bool
		want    *big.Int
	}{
		{
			name: "stale node floor, blocks pay less",
			history: map[string]any{"oldestBlock": "0x1", "baseFeePerGas": []string{"0x0", "0x0", "0x0", "0x0"}, "gasUsedRatio": []float64{0.1, 0.1, 0.1},
				"reward": [][]string{{gwei(0.05)}, {"0x0"}, {gwei(0.12)}, {gwei(0.08)}}},
			want: big.NewInt(80_000_000),
		},
		{
			name: "blocks pay more than suggestion",
			history: map[string]any{"oldestBlock": "0x1", "baseFeePerGas": []string{"0x0", "0x0"}, "gasUsedRatio": []float64{0.1},
				"reward": [][]string{{gwei(3)}}},
			want: big.NewInt(1_000_000_000),
		},
		{name: "fee history unsupported", histErr: true, want: big.NewInt(1_000_000_000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &testRPCServer{}
			client := server.client(t, func(method string, _ []json.RawMessage) (any, *testRPCError) {
				switch method {
				case "eth_maxPriorityFeePerGas":
					return gwei(1), nil
				case "eth_feeHistory":
					if tc.histErr {
						return nil, &testRPCError{Code: -32601, Message: "method not found"}
					}
					return tc.history, nil
				}
				t.Fatalf("unexpected RPC %s", method)
				return nil, nil
			})
			defer client.Close()
			got, err := (&Relayer{chain: singleRPC(client)}).priorityFee(context.Background())
			if err != nil || got.Cmp(tc.want) != 0 {
				t.Fatalf("tip=%v err=%v, want %s", got, err, tc.want)
			}
		})
	}
}

func TestRequiredFeeForGasRoundsTokenGasUp(t *testing.T) {
	// 1 USDC (6 decimals) = 3 wei: 10 wei of gas is 3.33 USDC, so 3_333_334 base units.
	usdc := units(InstanceMeta{Currency: "usdc", Symbol: "USDC", Amount: "100", Decimals: 6, IsERC20: true})
	relayer := &Relayer{
		cfg:    Config{ServiceFeeBPS: 10, PriceUpdatePeriod: time.Minute},
		prices: freshPrices(map[string]*big.Int{"usdc": big.NewInt(3)}),
	}
	required, serviceFee, err := relayer.requiredFeeForGas(usdc, big.NewInt(10))
	if err != nil {
		t.Fatal(err)
	}
	// 0.1% of 100 USDC = 100_000 base units.
	if serviceFee.Int64() != 100_000 || required.Int64() != 100_000+3_333_334 {
		t.Fatalf("required=%s serviceFee=%s", required, serviceFee)
	}
	// Paying exactly the required fee must buy at least the gas it was quoted for.
	budget, err := relayer.nativeGasBudget(usdc, required)
	if err != nil || budget.Cmp(big.NewInt(10)) < 0 {
		t.Fatalf("budget for required fee = %s, %v", budget, err)
	}
	// Native pools need no conversion.
	eth := units(InstanceMeta{Currency: "eth", Amount: "1", Decimals: 18})
	if required, _, err := relayer.requiredFeeForGas(eth, big.NewInt(10)); err != nil || required.String() != "1000000000000010" {
		t.Fatalf("native required=%s, %v", required, err)
	}
	// Without a fresh price the quote is impossible.
	relayer.prices = nil
	if _, _, err := relayer.requiredFeeForGas(usdc, big.NewInt(10)); err == nil {
		t.Fatal("missing price must fail")
	}
}

func TestFeeShortfallErrorQuotesRequiredFee(t *testing.T) {
	tooLow := &feeTooLowError{
		requiredGasNative: big.NewInt(21_000 * 2),
		budgetNative:      big.NewInt(1),
		estimatedGas:      21_000,
		marketPrice:       big.NewInt(2),
		reference:         "base fee 1 + tip 1",
	}
	dai := units(InstanceMeta{Currency: "dai", Symbol: "DAI", Amount: "100", Decimals: 18, IsERC20: true})
	relayer := &Relayer{
		cfg:    Config{PriceUpdatePeriod: time.Minute},
		prices: freshPrices(map[string]*big.Int{"dai": big.NewInt(1_000_000)}), // 1 DAI = 1e6 wei
	}
	err := relayer.feeShortfallError(dai, big.NewInt(5), tooLow)
	want := "provided 5, required at least 42000000000000000 DAI base units (service fee 0 + 21000 estimated gas at 2 wei/gas; base fee 1 + tip 1)"
	if !failureDeferrable(err) || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v", err)
	}
	eth := units(InstanceMeta{Currency: "eth", Amount: "1", Decimals: 18})
	if err := relayer.feeShortfallError(eth, big.NewInt(5), tooLow); !strings.Contains(err.Error(), "required at least 42000 wei") {
		t.Fatalf("native: %v", err)
	}
	// No price: the generic shortfall is still reported, still deferrable.
	relayer.prices = nil
	if err := relayer.feeShortfallError(dai, big.NewInt(5), tooLow); !failureDeferrable(err) || !errors.As(err, new(*feeTooLowError)) {
		t.Fatalf("without price: %v", err)
	}
}

func TestRejectsServiceFeeThatConsumesEntireDenomination(t *testing.T) {
	fee := uint64(10_000)
	err := validateChainNetworkConfig(ChainNetworkConfig{
		Version:         "1.0.0",
		ServiceFeeBPS:   &fee,
		MaxGasLimit:     420000,
		MaxGasPriceGwei: "100",
	})
	if err == nil || !strings.Contains(err.Error(), "less than 10000") {
		t.Fatalf("100%% service fee creates an unusable relayer and must be rejected: %v", err)
	}
}
