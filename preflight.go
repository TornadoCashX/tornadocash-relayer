package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// errPoolMismatch marks a pool that is reachable but does not match the local
// configuration. RPC failures are not wrapped with it.
var errPoolMismatch = errors.New("pool contract does not match relayer configuration")

// configuredPool resolves the request contract strictly against the pools
// configured for the active NET_ID (relayer.config.json instances.netIdX).
// It is a pure in-memory check and performs no RPC.
func (r *Relayer) configuredPool(contract string) (common.Address, InstanceMeta, error) {
	if !addressRe.MatchString(contract) {
		return common.Address{}, InstanceMeta{}, errors.New(".contract must be a valid Ethereum address")
	}
	pool := common.HexToAddress(contract)
	meta, ok := r.cfg.InstanceByAddress[pool]
	if !ok {
		return pool, InstanceMeta{}, fmt.Errorf(".contract %s is not a configured pool for netId %s in relayer.config.json instances", pool.Hex(), r.cfg.ChainID)
	}
	return pool, meta, nil
}

type preflightCall struct {
	data     []byte
	required bool
	result   hexutil.Bytes
	err      error
}

// preflightWithdraw runs all read-only pre-sign checks in one JSON-RPC batch:
// pool identity (denomination/token vs relayer.config.json), isKnownRoot,
// isSpent and the recipient EOA check. Providers without batch support fall
// back to sequential calls. isSpent is the chain truth for already-mined use;
// it intentionally complements, rather than replaces, the atomic database
// pending-nullifier lock that closes the pre-mine concurrency window.
func (r *Relayer) preflightWithdraw(ctx context.Context, valid validWithdraw) error {
	req, pool, meta := valid.req, valid.pool, valid.meta
	calls, err := r.preflightCalls(req, meta)
	if err != nil {
		return retryableInternalFailure(err)
	}
	// Every read runs on the endpoint pinned to ctx, so pool identity,
	// isKnownRoot, isSpent and recipient code come from one chain view.
	recipientCode, recipientErr := preflightOnEndpoint(ctx, r.eth(ctx), pool, calls.all(), common.HexToAddress(req.Args[argRecipient]))

	if err := r.checkPoolIdentity(pool, meta, calls.denomination, calls.token); err != nil {
		if errors.Is(err, errPoolMismatch) {
			// Relayer configuration problem; retrying after a config fix can succeed.
			return retryableFailure(err)
		}
		return retryableInternalFailure(err)
	}
	if err := r.checkRootAndNullifier(calls.knownRoot, calls.spent); err != nil {
		return err
	}
	if recipientErr != nil {
		return retryableInternalFailure(fmt.Errorf("recipient code check failed: %w", recipientErr))
	}
	if len(recipientCode) != 0 {
		return permanentFailure(errors.New("recipient cannot be a smart-contract, only EOA"))
	}
	return nil
}

// preflightCallSet is the pool eth_calls of one preflight.
type preflightCallSet struct {
	denomination, token, knownRoot, spent *preflightCall
}

func (c preflightCallSet) all() []*preflightCall {
	return []*preflightCall{c.denomination, c.token, c.knownRoot, c.spent}
}

// preflightCalls packs the pool calls; token() is only required for ERC20 pools.
func (r *Relayer) preflightCalls(req WithdrawRequest, meta InstanceMeta) (preflightCallSet, error) {
	var err error
	pack := func(method string, args ...any) []byte {
		data, packErr := r.poolABI.Pack(method, args...)
		if packErr != nil && err == nil {
			err = packErr
		}
		return data
	}
	calls := preflightCallSet{
		denomination: &preflightCall{data: pack("denomination"), required: true},
		token:        &preflightCall{data: pack("token"), required: meta.IsERC20},
		knownRoot:    &preflightCall{data: pack("isKnownRoot", common.HexToHash(req.Args[argRoot])), required: true},
		spent:        &preflightCall{data: pack("isSpent", common.HexToHash(req.Args[argNullifierHash])), required: true},
	}
	return calls, err
}

// checkRootAndNullifier classifies the isKnownRoot and isSpent results.
func (r *Relayer) checkRootAndNullifier(knownRoot, spent *preflightCall) error {
	if knownRoot.err != nil {
		return retryableInternalFailure(fmt.Errorf("isKnownRoot call failed: %w", knownRoot.err))
	}
	if ok, err := r.unpackPoolBool("isKnownRoot", knownRoot.result); err != nil {
		return retryableInternalFailure(err)
	} else if !ok {
		// Retryable: the RPC node may lag behind the root the client used, so
		// the other endpoint is asked too (the contract still checks the root).
		return retryableFailure(errRootUnknown)
	}
	if spent.err != nil {
		return retryableInternalFailure(fmt.Errorf("isSpent call failed: %w", spent.err))
	}
	if isSpent, err := r.unpackPoolBool("isSpent", spent.result); err != nil {
		return retryableInternalFailure(err)
	} else if isSpent {
		return permanentFailure(errNullifierSpent)
	}
	return nil
}

// preflightOnEndpoint sends every pool eth_call plus the recipient eth_getCode
// in a single batch. Failed elements are retried sequentially on the same
// endpoint so transient batch errors are not mistaken for contract reverts.
func preflightOnEndpoint(ctx context.Context, client *ethclient.Client, pool common.Address, calls []*preflightCall, recipient common.Address) (hexutil.Bytes, error) {
	var recipientCode hexutil.Bytes
	batch := make([]rpc.BatchElem, 0, len(calls)+1)
	for _, call := range calls {
		call.result, call.err = nil, nil
		batch = append(batch, rpc.BatchElem{
			Method: "eth_call",
			Args:   []any{map[string]any{"to": pool, "input": hexutil.Bytes(call.data)}, "latest"},
			Result: &call.result,
		})
	}
	batch = append(batch, rpc.BatchElem{Method: "eth_getCode", Args: []any{recipient, "latest"}, Result: &recipientCode})
	batchErr := client.Client().BatchCallContext(ctx, batch)

	for i, call := range calls {
		if batchErr == nil && batch[i].Error == nil {
			continue
		}
		if batchErr == nil && !call.required {
			// Optional call (token() on a native pool) reverted: expected.
			call.err = batch[i].Error
			continue
		}
		call.result, call.err = client.CallContract(ctx, ethereum.CallMsg{To: &pool, Data: call.data}, nil)
	}
	if last := batch[len(calls)]; batchErr == nil && last.Error == nil {
		return recipientCode, nil
	}
	return client.CodeAt(ctx, recipient, nil)
}

// checkPoolIdentity verifies that pool's denomination() and token() match the
// configured instance. Mismatches wrap errPoolMismatch; RPC failures do not.
func (r *Relayer) checkPoolIdentity(pool common.Address, meta InstanceMeta, denomination, token *preflightCall) error {
	if err := r.checkPoolDenomination(pool, meta, denomination); err != nil {
		return err
	}
	if meta.IsERC20 {
		return r.checkPoolToken(pool, meta, token)
	}
	return r.checkPoolIsNative(pool, meta, token)
}

func (r *Relayer) checkPoolDenomination(pool common.Address, meta InstanceMeta, denomination *preflightCall) error {
	if denomination.err != nil {
		return fmt.Errorf("pool verification RPC failed for %s denomination(): %w", pool.Hex(), denomination.err)
	}
	if len(denomination.result) == 0 {
		return fmt.Errorf("%w: pool contract %s has no code or is not a Tornado pool on the connected chain", errPoolMismatch, pool.Hex())
	}
	values, err := r.poolABI.Unpack("denomination", denomination.result)
	if err != nil || len(values) != 1 {
		return fmt.Errorf("%w: pool contract %s returned an invalid denomination", errPoolMismatch, pool.Hex())
	}
	if got, ok := values[0].(*big.Int); !ok || got == nil || got.Cmp(meta.Denomination) != 0 {
		return fmt.Errorf("%w: pool contract %s denomination %v does not match configured %s %s (%v units)",
			errPoolMismatch, pool.Hex(), values[0], meta.Amount, meta.Currency, meta.Denomination)
	}
	return nil
}

func (r *Relayer) checkPoolToken(pool common.Address, meta InstanceMeta, token *preflightCall) error {
	if token.err != nil {
		return fmt.Errorf("pool verification RPC failed for %s token(): %w", pool.Hex(), token.err)
	}
	values, err := r.poolABI.Unpack("token", token.result)
	if err != nil || len(values) != 1 {
		return fmt.Errorf("%w: pool contract %s returned an invalid token for configured %s", errPoolMismatch, pool.Hex(), meta.Currency)
	}
	if address, ok := values[0].(common.Address); !ok || address != meta.TokenAddress {
		return fmt.Errorf("%w: pool contract %s token %v does not match configured %s tokenAddress %s",
			errPoolMismatch, pool.Hex(), values[0], meta.Currency, meta.TokenAddress.Hex())
	}
	return nil
}

// checkPoolIsNative: native pools have no token(); a non-zero answer means an
// ERC20 pool was configured as a native-currency pool.
func (r *Relayer) checkPoolIsNative(pool common.Address, meta InstanceMeta, token *preflightCall) error {
	if token.err != nil || len(token.result) == 0 {
		return nil
	}
	values, err := r.poolABI.Unpack("token", token.result)
	if err != nil || len(values) != 1 {
		return nil
	}
	if address, ok := values[0].(common.Address); ok && address != (common.Address{}) {
		return fmt.Errorf("%w: pool contract %s is an ERC20 pool (token %s) but %s is configured as native currency",
			errPoolMismatch, pool.Hex(), address.Hex(), meta.Currency)
	}
	return nil
}

// unpackPoolBool decodes the single bool returned by a pool view method.
func (r *Relayer) unpackPoolBool(method string, result []byte) (bool, error) {
	values, err := r.poolABI.Unpack(method, result)
	if err != nil {
		return false, fmt.Errorf("%s returned unexpected result: %w", method, err)
	}
	if len(values) != 1 {
		return false, fmt.Errorf("%s returned unexpected result", method)
	}
	value, ok := values[0].(bool)
	if !ok {
		return false, fmt.Errorf("%s returned unexpected result", method)
	}
	return value, nil
}
