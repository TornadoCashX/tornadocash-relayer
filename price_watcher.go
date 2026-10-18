package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

var cDAITokenAddress = common.HexToAddress("0x5d3a536E4D6DbD6114cc1Ead35777bAB948E3643")

const (
	oracleRequestTimeout  = 7 * time.Second
	oracleRequestAttempts = 2
	oracleRetryDelay      = 250 * time.Millisecond

	// oracleRateScale is the fixed-point scale of getRateToEth (1e18).
	oracleRateScale = 1_000_000_000_000_000_000
	// oracleMaxClockSkew tolerates oracle blocks slightly in the future.
	oracleMaxClockSkew = time.Minute
	// oracleMaxBlockAge is the oldest oracle head that is still trusted.
	oracleMaxBlockAge = 10 * time.Minute
	// priceStalePeriods and minPriceStaleAfter define priceStaleAfter.
	priceStalePeriods  = 3
	minPriceStaleAfter = 2 * time.Minute
)

type oraclePriceToken struct {
	Address  common.Address
	Decimals uint8
}

func oraclePriceTokensFromInstances(instances InstancesConfig) map[string]oraclePriceToken {
	tokens := make(map[string]oraclePriceToken)
	for currency, instance := range instances {
		if instance.TokenAddress == "" {
			continue
		}
		tokens[strings.ToLower(currency)] = oraclePriceToken{
			Address:  common.HexToAddress(instance.OracleTokenAddress),
			Decimals: instance.Decimals,
		}
	}
	return tokens
}

func (r *Relayer) shouldUseOraclePrices() bool {
	return len(r.cfg.OraclePriceTokens) > 0
}

func (r *Relayer) runPriceWatcher(ctx context.Context) {
	r.refreshPrices(ctx)
	ticker := time.NewTicker(r.cfg.PriceUpdatePeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refreshPrices(ctx)
		}
	}
}

func (r *Relayer) refreshPrices(ctx context.Context) {
	prices, err := r.fetchOraclePrices(ctx)
	if err != nil {
		logErrorf("price watcher error: %v", err)
		return
	}
	if len(prices) == 0 {
		return
	}

	now := time.Now()
	r.priceMu.Lock()
	next := make(map[string]oraclePrice, len(r.prices)+len(prices))
	maps.Copy(next, r.prices)
	for currency, price := range prices {
		next[currency] = oraclePrice{wei: new(big.Int).Set(price), updatedAt: now}
	}
	r.prices = next
	r.priceMu.Unlock()
	r.oracleHeartbeat.Store(time.Now().Unix())
	logInfof("updated ethPrices: %v", r.ethPriceStrings())
}

// fetchOraclePrices reads every price from one oracle endpoint whose head is
// fresh, moving to the next endpoint only when the head check fails.
func (r *Relayer) fetchOraclePrices(ctx context.Context) (map[string]*big.Int, error) {
	var lastErr error // the pool always has endpoints (dialRPCPool)
	for _, endpoint := range r.oracle.rotation() {
		pinned := withRPCEndpoint(ctx, endpoint)
		if err := r.checkOracleHead(pinned); err != nil {
			lastErr = fmt.Errorf("%s: %w", endpoint.name, err)
			continue
		}
		return r.fetchOraclePricesOn(pinned), nil
	}
	return nil, lastErr
}

func (r *Relayer) checkOracleHead(ctx context.Context) error {
	header, err := r.oracle.client(ctx).HeaderByNumber(ctx, nil)
	if err != nil {
		return fmt.Errorf("oracle latest block unavailable: %w", err)
	}
	if header == nil {
		return errors.New("oracle latest block unavailable: empty response")
	}
	if !oracleBlockIsFresh(header.Time, time.Now()) {
		return errors.New("oracle latest block is stale")
	}
	return nil
}

func (r *Relayer) fetchOraclePricesOn(ctx context.Context) map[string]*big.Int {
	currencies := orderedKeys(r.cfg.OraclePriceTokens, tokenOrder)
	prices := make(map[string]*big.Int)
	for _, currency := range currencies {
		token := r.cfg.OraclePriceTokens[currency]
		price, err := r.fetchTokenPriceWithRetry(ctx, token.Address, token.Decimals)
		if err != nil {
			if !(errors.Is(err, context.Canceled) && errors.Is(ctx.Err(), context.Canceled)) {
				logWarnf("cannot update price for %s %s: %v", currency, token.Address.Hex(), err)
			}
			continue
		}
		prices[currency] = price
	}
	return prices
}

func (r *Relayer) fetchTokenPriceWithRetry(ctx context.Context, token common.Address, decimals uint8) (*big.Int, error) {
	var lastErr error
	for attempt := 1; attempt <= oracleRequestAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		requestCtx, cancel := context.WithTimeout(ctx, oracleRequestTimeout)
		price, err := r.fetchTokenPrice(requestCtx, token, decimals)
		cancel()
		if err == nil {
			return price, nil
		}
		lastErr = err
		if attempt < oracleRequestAttempts {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(oracleRetryDelay):
			}
		}
	}
	return nil, lastErr
}

func (r *Relayer) fetchTokenPrice(ctx context.Context, token common.Address, decimals uint8) (*big.Int, error) {
	data, err := r.oracleABI.Pack("getRateToEth", token, token == cDAITokenAddress)
	if err != nil {
		return nil, err
	}
	result, err := r.oracle.client(ctx).CallContract(ctx, ethereum.CallMsg{To: &r.cfg.OffchainOracle, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, errors.New("getRateToEth returned empty data")
	}
	values, err := r.oracleABI.Unpack("getRateToEth", result)
	if err != nil {
		return nil, err
	}
	rate, ok := values[0].(*big.Int)
	if !ok || rate.Sign() <= 0 {
		return nil, errors.New("getRateToEth returned unexpected result")
	}
	return normalizeOracleRate(rate, decimals)
}

// normalizeOracleRate converts a 1inch OffchainOracle getRateToEth rate into
// the price of one whole token in wei. The oracle returns
// wei-per-token-base-unit scaled by 1e18 (oracleRateScale), so
// price = rate * 10^decimals / 1e18 (truncated). The result is the wei value
// of 10^decimals base units, i.e. one whole token.
func normalizeOracleRate(rate *big.Int, decimals uint8) (*big.Int, error) {
	if rate == nil || rate.Sign() <= 0 {
		return nil, errors.New("getRateToEth returned a non-positive rate")
	}
	price := new(big.Int).Mul(rate, tokenUnit(decimals))
	price.Div(price, big.NewInt(oracleRateScale))
	if price.Sign() <= 0 {
		return nil, errors.New("getRateToEth returned a non-positive price")
	}
	return price, nil
}

func oracleBlockIsFresh(timestamp uint64, now time.Time) bool {
	if timestamp == 0 {
		return false
	}
	blockTime := time.Unix(int64(timestamp), 0)
	return !blockTime.After(now.Add(oracleMaxClockSkew)) && now.Sub(blockTime) <= oracleMaxBlockAge
}

// oraclePrice is the wei price of one whole token and when it was read.
type oraclePrice struct {
	wei       *big.Int
	updatedAt time.Time
}

// freshPriceLocked returns the price of currency unless it is missing,
// non-positive or older than priceStaleAfter. Callers hold priceMu.
func (r *Relayer) freshPriceLocked(currency string) (*big.Int, bool) {
	price, ok := r.prices[currency]
	if !ok || price.wei.Sign() <= 0 || time.Since(price.updatedAt) > r.priceStaleAfter() {
		return nil, false
	}
	return price.wei, true
}

// ethPrice returns the fresh oracle price of one whole currency token in wei.
func (r *Relayer) ethPrice(currency string) (*big.Int, bool) {
	r.priceMu.RLock()
	defer r.priceMu.RUnlock()
	price, ok := r.freshPriceLocked(strings.ToLower(currency))
	if !ok {
		return nil, false
	}
	return new(big.Int).Set(price), true
}

func (r *Relayer) ethPriceStrings() map[string]string {
	r.priceMu.RLock()
	defer r.priceMu.RUnlock()
	out := make(map[string]string, len(r.cfg.OraclePriceTokens))
	for currency := range r.cfg.OraclePriceTokens {
		if price, ok := r.freshPriceLocked(currency); ok {
			out[currency] = price.String()
		}
	}
	return out
}

// priceStaleAfter is the age after which an oracle price is no longer used.
func (r *Relayer) priceStaleAfter() time.Duration {
	return max(priceStalePeriods*r.cfg.PriceUpdatePeriod, minPriceStaleAfter)
}
