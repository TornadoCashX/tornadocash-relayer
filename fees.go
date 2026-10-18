package main

import (
	"errors"
	"fmt"
	"math/big"
)

// nativeGasBudget converts the proof fee into the maximum native currency
// amount that this relayer may spend on transaction execution.
func (r *Relayer) nativeGasBudget(meta InstanceMeta, fee *big.Int) (*big.Int, error) {
	serviceFee := r.serviceFee(meta)
	if fee.Cmp(serviceFee) < 0 {
		return nil, permanentFailure(fmt.Errorf("provided fee is below configured service fee: got %s, service fee %s", fee, serviceFee))
	}
	available := new(big.Int).Sub(fee, serviceFee)
	if meta.IsERC20 {
		price, err := r.tokenPrice(meta)
		if err != nil {
			return nil, deferrableFailure(err)
		}
		available = tokenToWei(available, price, meta.TokenUnit)
	}
	if available.Sign() <= 0 {
		return nil, permanentFailure(errors.New("provided fee leaves no transaction gas budget"))
	}
	return available, nil
}

// serviceFee is the relayer service fee of one withdrawal from the pool, in the
// pool's base units, rounded up.
func (r *Relayer) serviceFee(meta InstanceMeta) *big.Int {
	return applyBPSCeil(meta.Denomination, r.cfg.ServiceFeeBPS)
}

// tokenPrice is the oracle price of one whole pool token in wei.
func (r *Relayer) tokenPrice(meta InstanceMeta) (*big.Int, error) {
	price, ok := r.ethPrice(meta.Currency)
	if !ok {
		return nil, fmt.Errorf("oracle price for %s is not available yet", meta.Currency)
	}
	return price, nil
}

// tokenToWei converts token base units to wei, rounding down:
// floor(amount * price / unit).
func tokenToWei(amount, price, unit *big.Int) *big.Int {
	wei := new(big.Int).Mul(amount, price)
	return wei.Div(wei, unit)
}

// weiToTokenCeil converts wei to token base units, rounding up:
// ceil(wei * unit / price) = (wei*unit + price - 1) / price, so a required fee
// never undercounts the gas cost by a rounding unit.
func weiToTokenCeil(wei, price, unit *big.Int) *big.Int {
	tokens := new(big.Int).Mul(wei, unit)
	tokens.Add(tokens, price)
	tokens.Sub(tokens, big.NewInt(1))
	return tokens.Div(tokens, price)
}

// feeTooLowError reports that the proof fee cannot cover service fee plus the
// expected gas cost (estimateGas x current market price). Amounts are in wei.
type feeTooLowError struct {
	requiredGasNative *big.Int
	budgetNative      *big.Int
	estimatedGas      uint64
	marketPrice       *big.Int
	reference         string // "base fee X + tip Y"
}

func (e *feeTooLowError) Error() string {
	return fmt.Sprintf("provided fee gas budget %s wei is below expected gas cost %s wei (estimated gas %d x %s wei/gas; %s)",
		e.budgetNative, e.requiredGasNative, e.estimatedGas, e.marketPrice, e.reference)
}

// requiredFeeForGas converts a native gas amount into the fee the proof must
// carry: service fee + gas, in the pool's base units (gas rounded up).
func (r *Relayer) requiredFeeForGas(meta InstanceMeta, gasNative *big.Int) (required, serviceFee *big.Int, err error) {
	serviceFee = r.serviceFee(meta)
	gasInFeeUnits := gasNative
	if meta.IsERC20 {
		price, err := r.tokenPrice(meta)
		if err != nil {
			return nil, nil, err
		}
		gasInFeeUnits = weiToTokenCeil(gasNative, price, meta.TokenUnit)
	}
	return new(big.Int).Add(serviceFee, gasInFeeUnits), serviceFee, nil
}

// feeShortfallError turns a feeTooLowError into a client-facing message in the
// pool's fee unit.
func (r *Relayer) feeShortfallError(meta InstanceMeta, fee *big.Int, tooLow *feeTooLowError) error {
	required, serviceFee, err := r.requiredFeeForGas(meta, tooLow.requiredGasNative)
	if err != nil {
		return deferrableFailure(tooLow)
	}
	unit := "wei"
	if meta.IsERC20 {
		unit = meta.Symbol + " base units"
	}
	return deferrableFailure(fmt.Errorf(
		"provided fee is below service fee + estimated gas: provided %s, required at least %s %s (service fee %s + %d estimated gas at %s wei/gas; %s)",
		fee, required, unit, serviceFee, tooLow.estimatedGas, tooLow.marketPrice, tooLow.reference))
}
