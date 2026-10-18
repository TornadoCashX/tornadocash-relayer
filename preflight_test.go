package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

func TestSimulationRevertExposesOnlyRevertReason(t *testing.T) {
	estimateErr := errors.New("execution reverted: Invalid withdraw proof")
	callErr := errors.New(`Post "https://mainnet.infura.io/v3/SECRETKEY": context deadline exceeded`)
	err := classifyEstimateError(&simulationError{estimateErr: estimateErr, callErr: callErr})
	reason := publicFailureReason(err)
	if failureRetryable(err) || reason != "transaction simulation reverted: execution reverted: Invalid withdraw proof; not retryable" {
		t.Fatalf("unexpected public reason: %q", reason)
	}
	// Revert only visible in the eth_call diagnosis; the estimate error carries the URL.
	err = classifyEstimateError(&simulationError{
		estimateErr: errors.New(`Post "https://rpc.example/KEY": 502 Bad Gateway`),
		callErr:     errors.New("execution reverted: The note has been already spent"),
	})
	reason = publicFailureReason(err)
	if strings.Contains(reason, "rpc.example") || !strings.Contains(reason, "The note has been already spent") || failureRetryable(err) {
		t.Fatalf("unexpected public reason: %q", reason)
	}
	// Pure transport error: retryable and internal (no text exposed).
	err = classifyEstimateError(&simulationError{estimateErr: errors.New(`Post "https://rpc.example/KEY": timeout`)})
	if reason := publicFailureReason(err); !failureRetryable(err) || strings.Contains(reason, "rpc.example") {
		t.Fatalf("transport error leaked or misclassified: %q", reason)
	}
}

func samplePoolIndex() map[common.Address]InstanceMeta {
	return map[common.Address]InstanceMeta{
		common.HexToAddress(sampleWithdrawRequest().Contract): units(InstanceMeta{Currency: "eth", Amount: "1", Symbol: "ETH", Decimals: 18}),
	}
}

func TestValidateWithdrawRequiresPoolOfActiveChain(t *testing.T) {
	relayer := &Relayer{cfg: Config{
		ChainID:       big.NewInt(56),
		RewardAccount: common.HexToAddress("0x0000000000000000000000000000000000000055"),
	}}
	request := sampleWithdrawRequest()
	// An empty pool index must never mean "accept any contract".
	if _, err := relayer.validateWithdraw(request); err == nil || !strings.Contains(err.Error(), "not a configured pool for netId 56") {
		t.Fatalf("contract must be rejected without configured pools, got %v", err)
	}
	relayer.cfg.InstanceByAddress = samplePoolIndex()
	if _, err := relayer.validateWithdraw(request); err != nil {
		t.Fatalf("configured pool should be accepted, got %v", err)
	}
	// A real pool of another chain (mainnet 10 ETH) is not accepted on netId 56.
	request.Contract = "0x910Cbd523D972eb0a6f4cAe4618aD62622b39DbF"
	if _, err := relayer.validateWithdraw(request); err == nil || !strings.Contains(err.Error(), "not a configured pool") {
		t.Fatalf("pool from another chain must be rejected, got %v", err)
	}
}

// preflightChain describes what the mocked chain returns for the preflight batch.
type preflightChain struct {
	denomination  *big.Int
	token         *common.Address // nil = token() reverts (native pool)
	poolHasCode   bool
	knownRoot     bool
	spent         bool
	recipientCode string
}

func runPreflight(t *testing.T, server *testRPCServer, chain preflightChain, meta InstanceMeta) error {
	t.Helper()
	poolABI, err := abi.JSON(strings.NewReader(tornadoPoolABI))
	if err != nil {
		t.Fatalf("parse pool ABI: %v", err)
	}
	selector := func(method string) string { return "0x" + common.Bytes2Hex(poolABI.Methods[method].ID) }
	word := func(b []byte) string { return "0x" + common.Bytes2Hex(common.LeftPadBytes(b, 32)) }
	boolWord := func(v bool) string {
		if v {
			return word([]byte{1})
		}
		return word(nil)
	}
	request := sampleWithdrawRequest()
	client := server.client(t, func(method string, params []json.RawMessage) (any, *testRPCError) {
		switch method {
		case "eth_getCode":
			return chain.recipientCode, nil
		case "eth_call":
			var call struct {
				Input string `json:"input"`
				Data  string `json:"data"`
			}
			_ = json.Unmarshal(params[0], &call)
			input := call.Input + call.Data
			if !chain.poolHasCode {
				return "0x", nil
			}
			switch {
			case strings.HasPrefix(input, selector("denomination")):
				return word(chain.denomination.Bytes()), nil
			case strings.HasPrefix(input, selector("token")):
				if chain.token == nil {
					return nil, &testRPCError{Code: 3, Message: "execution reverted"}
				}
				return word(chain.token.Bytes()), nil
			case strings.HasPrefix(input, selector("isKnownRoot")):
				return boolWord(chain.knownRoot), nil
			case strings.HasPrefix(input, selector("isSpent")):
				return boolWord(chain.spent), nil
			}
		}
		t.Fatalf("unexpected RPC %s", method)
		return nil, nil
	})
	defer client.Close()
	relayer := &Relayer{
		cfg: Config{
			ChainID:           big.NewInt(1),
			InstanceByAddress: map[common.Address]InstanceMeta{common.HexToAddress(request.Contract): units(meta)},
		},
		chain:   singleRPC(client),
		poolABI: poolABI,
	}
	return relayer.preflightWithdraw(context.Background(), validWithdraw{req: request, pool: common.HexToAddress(request.Contract), meta: units(meta)})
}

func TestPreflightIsSingleBatchAndVerifiesPoolIdentity(t *testing.T) {
	oneETH := big.NewInt(1e18)
	dai := common.HexToAddress("0x6B175474E89094C44Da98b954EedeAC495271d0F")
	usdc := common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48")
	ethMeta := InstanceMeta{Currency: "eth", Amount: "1", Decimals: 18}
	daiMeta := InstanceMeta{Currency: "dai", Amount: "1", Decimals: 18, IsERC20: true, TokenAddress: dai}
	good := preflightChain{denomination: oneETH, poolHasCode: true, knownRoot: true, recipientCode: "0x"}

	server := &testRPCServer{batches: true}
	if err := runPreflight(t, server, good, ethMeta); err != nil {
		t.Fatalf("valid native pool preflight failed: %v", err)
	}
	if server.httpCalls != 1 {
		t.Fatalf("native pool preflight used %d HTTP round trips, want 1 batch (methods %v)", server.httpCalls, server.methods)
	}

	erc20 := good
	erc20.token = &dai
	server = &testRPCServer{batches: true}
	if err := runPreflight(t, server, erc20, daiMeta); err != nil {
		t.Fatalf("valid ERC20 pool preflight failed: %v", err)
	}
	if server.httpCalls != 1 {
		t.Fatalf("ERC20 pool preflight used %d HTTP round trips, want 1", server.httpCalls)
	}

	for name, test := range map[string]struct {
		chain preflightChain
		meta  InstanceMeta
		want  string
	}{
		"no code":                  {chain: preflightChain{recipientCode: "0x"}, meta: ethMeta, want: "no code or is not a Tornado pool"},
		"wrong denomination":       {chain: func() preflightChain { c := good; c.denomination = big.NewInt(1e17); return c }(), meta: ethMeta, want: "denomination"},
		"ERC20 pool as native":     {chain: erc20, meta: ethMeta, want: "is an ERC20 pool"},
		"wrong ERC20 token":        {chain: func() preflightChain { c := good; c.token = &usdc; return c }(), meta: daiMeta, want: "does not match configured dai tokenAddress"},
		"ERC20 config, no token()": {chain: good, meta: daiMeta, want: "token()"},
	} {
		err := runPreflight(t, &testRPCServer{batches: true}, test.chain, test.meta)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: want error containing %q, got %v", name, test.want, err)
		}
		if test.want != "token()" && !errors.Is(err, errPoolMismatch) {
			t.Fatalf("%s: config mismatch must wrap errPoolMismatch, got %v", name, err)
		}
	}

	for name, test := range map[string]struct {
		chain preflightChain
		want  string
	}{
		"unknown root":       {chain: func() preflightChain { c := good; c.knownRoot = false; return c }(), want: "root is not known"},
		"spent nullifier":    {chain: func() preflightChain { c := good; c.spent = true; return c }(), want: "already spent"},
		"contract recipient": {chain: func() preflightChain { c := good; c.recipientCode = "0x6000"; return c }(), want: "only EOA"},
	} {
		if err := runPreflight(t, &testRPCServer{batches: true}, test.chain, ethMeta); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: want error containing %q, got %v", name, test.want, err)
		}
	}
}

func TestPreflightFallsBackWhenBatchDisabled(t *testing.T) {
	server := &testRPCServer{batches: false}
	chain := preflightChain{denomination: big.NewInt(1e18), poolHasCode: true, knownRoot: true, recipientCode: "0x"}
	if err := runPreflight(t, server, chain, InstanceMeta{Currency: "eth", Amount: "1", Decimals: 18}); err != nil {
		t.Fatalf("sequential fallback preflight failed: %v", err)
	}
	// 1 rejected batch + 4 eth_call + 1 eth_getCode.
	if server.httpCalls != 6 {
		t.Fatalf("fallback used %d HTTP calls, want 6 (methods %v)", server.httpCalls, server.methods)
	}
	chain.spent = true
	if err := runPreflight(t, &testRPCServer{batches: false}, chain, InstanceMeta{Currency: "eth", Amount: "1", Decimals: 18}); err == nil || !strings.Contains(err.Error(), "already spent") {
		t.Fatalf("fallback must still enforce isSpent, got %v", err)
	}
}

func TestValidateWithdrawRejectsNonZeroRefund(t *testing.T) {
	relayer := &Relayer{cfg: Config{
		RewardAccount:     common.HexToAddress("0x0000000000000000000000000000000000000055"),
		InstanceByAddress: samplePoolIndex(),
	}}
	request := sampleWithdrawRequest()
	request.Args[3] = relayer.cfg.RewardAccount.Hex()
	request.Args[5] = "0x" + strings.Repeat("00", 31) + "01"
	if _, err := relayer.validateWithdraw(request); err == nil || !strings.Contains(err.Error(), "refund must be zero") {
		t.Fatalf("non-zero refund should be rejected, got %v", err)
	}
	request.Args[5] = "0x" + strings.Repeat("00", 32)
	if _, err := relayer.validateWithdraw(request); err != nil {
		t.Fatalf("zero refund should be accepted by request validation, got %v", err)
	}
}

func TestValidatePlanRequiresConfiguredPool(t *testing.T) {
	parsed, err := abi.JSON(strings.NewReader(tornadoPoolABI))
	if err != nil {
		t.Fatalf("parse ABI: %v", err)
	}
	pool := common.HexToAddress("0x12D66f87A04A9E220743712cE6d9bB1B5616B8Fc")
	data := append([]byte(nil), parsed.Methods["withdraw"].ID...)
	plan := &withdrawTxPlan{to: pool, data: data, tip: big.NewInt(1), maxFee: big.NewInt(2)}
	relayer := &Relayer{poolABI: parsed}
	if err := relayer.validatePlan(plan); err == nil || !strings.Contains(err.Error(), "not a configured pool") {
		t.Fatalf("unconfigured target error=%v", err)
	}
	relayer.cfg.InstanceByAddress = map[common.Address]InstanceMeta{pool: units(InstanceMeta{Currency: "eth", Amount: "0.1", Symbol: "ETH", Decimals: 18})}
	if err := relayer.validatePlan(plan); err != nil {
		t.Fatalf("configured target rejected: %v", err)
	}
}

func TestWithdrawCalldataTargetsPoolContractDirectly(t *testing.T) {
	parsed, err := abi.JSON(strings.NewReader(tornadoPoolABI))
	if err != nil {
		t.Fatalf("parse ABI: %v", err)
	}
	relayer := &Relayer{poolABI: parsed}
	req := WithdrawRequest{
		Proof:    "0x" + strings.Repeat("11", 256),
		Contract: "0x12D66f87A04A9E220743712cE6d9bB1B5616B8Fc",
		Args: []string{
			"0x" + strings.Repeat("22", 32),
			"0x" + strings.Repeat("33", 32),
			"0x000000000000000000000000000000000000dEaD",
			"0x9999999F360BE2c1c12f920F16ce25d93C603303",
			"0x" + strings.Repeat("00", 32),
			"0x" + strings.Repeat("00", 32),
		},
	}
	data, _, err := relayer.withdrawCallData(req)
	if err != nil {
		t.Fatalf("pack withdraw: %v", err)
	}
	// Pool withdraw(bytes,bytes32,bytes32,address,address,uint256,uint256) selector 0x21a0adb6.
	want := "0x21a0adb6" +
		"00000000000000000000000000000000000000000000000000000000000000e0" +
		strings.Repeat("22", 32) +
		strings.Repeat("33", 32) +
		"000000000000000000000000000000000000000000000000000000000000dead" +
		"0000000000000000000000009999999f360be2c1c12f920f16ce25d93c603303" +
		strings.Repeat("00", 32) +
		strings.Repeat("00", 32) +
		"0000000000000000000000000000000000000000000000000000000000000100" +
		strings.Repeat("11", 256)
	if selector := parsed.Methods["withdraw"].ID; common.Bytes2Hex(selector) != "21a0adb6" {
		t.Fatalf("pool withdraw selector=%x, want 21a0adb6", selector)
	}
	if got := "0x" + common.Bytes2Hex(data); got != want {
		t.Fatalf("calldata mismatch:\n got: %s\nwant: %s", got, want)
	}
}

type testRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type testRPCRequest struct {
	ID     json.RawMessage   `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

// testRPCServer is a JSON-RPC mock that supports batches. When batches is
// false it rejects batch requests like providers with batching disabled.
type testRPCServer struct {
	mu        sync.Mutex // counters are shared by concurrent receipt/nonce observers
	batches   bool
	httpCalls int
	methods   []string
}

func (s *testRPCServer) client(t *testing.T, result func(method string, params []json.RawMessage) (any, *testRPCError)) *ethclient.Client {
	t.Helper()
	answer := func(rpcReq testRPCRequest) map[string]any {
		s.mu.Lock()
		s.methods = append(s.methods, rpcReq.Method)
		s.mu.Unlock()
		value, rpcErr := result(rpcReq.Method, rpcReq.Params)
		response := map[string]any{"jsonrpc": "2.0", "id": rpcReq.ID}
		if rpcErr != nil {
			response["error"] = rpcErr
		} else {
			response["result"] = value
		}
		return response
	}
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		s.mu.Lock()
		s.httpCalls++
		s.mu.Unlock()
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var payload any
		if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '[' {
			if !s.batches {
				payload = map[string]any{"jsonrpc": "2.0", "id": nil, "error": testRPCError{Code: -32600, Message: "batch requests are disabled"}}
			} else {
				var batch []testRPCRequest
				if err := json.Unmarshal(raw, &batch); err != nil {
					return nil, err
				}
				responses := make([]map[string]any, 0, len(batch))
				for _, item := range batch {
					responses = append(responses, answer(item))
				}
				payload = responses
			}
		} else {
			var single testRPCRequest
			if err := json.Unmarshal(raw, &single); err != nil {
				return nil, err
			}
			payload = answer(single)
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    req,
		}, nil
	})
	rpcClient, err := rpc.DialOptions(context.Background(), "http://rpc.test", rpc.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatalf("dial test RPC: %v", err)
	}
	return ethclient.NewClient(rpcClient)
}
