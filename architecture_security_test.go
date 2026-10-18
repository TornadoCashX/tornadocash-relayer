package main

import (
	"context"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestFinalSimulationClassifiesRevertAndCarriesExactEnvelope(t *testing.T) {
	var checked bool
	server := &testRPCServer{batches: true}
	client := server.client(t, func(method string, params []json.RawMessage) (any, *testRPCError) {
		if method != "eth_call" {
			return nil, &testRPCError{Code: -32601, Message: "unexpected " + method}
		}
		var call map[string]string
		if err := json.Unmarshal(params[0], &call); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"from", "to", "gas", "maxFeePerGas", "maxPriorityFeePerGas", "value", "input"} {
			if _, ok := call[field]; !ok {
				t.Fatalf("final eth_call is missing %s: %#v", field, call)
			}
		}
		checked = true
		return nil, &testRPCError{Code: 3, Message: "execution reverted: invalid proof"}
	})
	defer client.Close()
	r := &Relayer{chain: singleRPC(client)}
	plan := &withdrawTxPlan{
		to: common.HexToAddress("0x910Cbd523D972eb0a6f4cAe4618aD62622b39DbF"), data: []byte{1, 2, 3, 4},
		gasLimit: 300000, tip: big.NewInt(2), maxFee: big.NewInt(20),
	}
	err := r.simulateWithdrawTx(context.Background(), common.HexToAddress("0x1111111111111111111111111111111111111111"), plan)
	if !checked || err == nil || failureRetryable(err) || !strings.Contains(err.Error(), "invalid proof") {
		t.Fatalf("expected permanent final simulation revert, checked=%v err=%v", checked, err)
	}
}

func TestProxyWithdrawMethodIDConstant(t *testing.T) {
	selector := crypto.Keccak256([]byte("withdraw(address,bytes,bytes32,bytes32,address,address,uint256,uint256)"))[:4]
	if got := "0x" + common.Bytes2Hex(selector); got != proxyWithdrawMethodID {
		t.Fatalf("proxy withdraw selector=%s want %s", got, proxyWithdrawMethodID)
	}
}
