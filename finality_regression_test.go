package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// overrideChainRPC preserves the normal fixture except for selected responses.
func overrideChainRPC(t *testing.T, h *ozHarness, override func(string, []json.RawMessage) (any, bool, bool)) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var call struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &call); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		result, handled, fail := override(call.Method, call.Params)
		if fail {
			http.Error(w, "injected RPC failure", http.StatusServiceUnavailable)
			return
		}
		if handled {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
			return
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		h.chain.ServeHTTP(w, req)
	}))
	t.Cleanup(server.Close)
	client, err := dialRPC("HTTP_RPC_URL", server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	h.r.chain = singleRPC(client)
}

func TestFallbackRejectsOrphanReceipt(t *testing.T) {
	for _, status := range []uint64{types.ReceiptStatusSuccessful, types.ReceiptStatusFailed} {
		for _, finalHeight := range []uint64{16, 20} {
			t.Run(big.NewInt(int64(status)).String()+"/finalized-"+big.NewInt(int64(finalHeight)).String(), func(t *testing.T) {
				h := newOZHarness(t)
				job, data := h.createSubmittedJob(t)
				hash := h.chain.mine(t, 7, testPool, data, status)
				h.chain.setFinalizedBlock(finalHeight)
				h.oz.set(job.OZTxID, func(tx *ozTransaction) { tx.Status = ozStatusFailed; tx.Hash = hash.Hex() })
				orphan := true
				overrideChainRPC(t, h, func(method string, params []json.RawMessage) (any, bool, bool) {
					if method != "eth_getTransactionReceipt" || !orphan {
						return nil, false, false
					}
					return &types.Receipt{Type: types.DynamicFeeTxType, Status: status, CumulativeGasUsed: 21000, GasUsed: 21000, TxHash: hash, BlockHash: common.HexToHash("0xdead"), BlockNumber: big.NewInt(16), Logs: []*types.Log{}}, true, false
				})
				if final, _, err := h.r.receiptFinalized(context.Background(), hash); err != nil || final {
					t.Fatalf("orphan accepted: final=%v err=%v", final, err)
				}
				got := h.reconcile(t, job.ID)
				if isTerminalStatus(got.Status) || !nullifierHeld(t, h) {
					t.Fatalf("orphan caused terminal state or release: %+v", got)
				}
				// Once the provider returns the canonical receipt, progress must resume.
				orphan = false
				got = h.reconcile(t, job.ID)
				if status == types.ReceiptStatusSuccessful {
					if got.Status != StatusConfirmed {
						t.Fatalf("canonical success not confirmed: %s", got.Status)
					}
				} else if got.Status != StatusFailed || got.Retryable {
					t.Fatalf("canonical revert not resolved: %+v", got)
				}
				if nullifierHeld(t, h) {
					t.Fatal("canonical finalized outcome should release")
				}
			})
		}
	}
}

func TestWrongTransactionReceiptRetainsNullifier(t *testing.T) {
	h := newOZHarness(t)
	job, data := h.createSubmittedJob(t)
	hash := h.chain.mine(t, 7, testPool, data, types.ReceiptStatusFailed)
	h.chain.setFinalizedBlock(16)
	h.oz.set(job.OZTxID, func(tx *ozTransaction) { tx.Status = ozStatusFailed; tx.Hash = hash.Hex() })
	overrideChainRPC(t, h, func(method string, params []json.RawMessage) (any, bool, bool) {
		if method != "eth_getTransactionReceipt" {
			return nil, false, false
		}
		return &types.Receipt{Type: types.DynamicFeeTxType, Status: types.ReceiptStatusFailed, CumulativeGasUsed: 21000, GasUsed: 21000,
			TxHash: common.HexToHash("0xdead"), BlockHash: fakeCanonicalHeader(16).Hash(), BlockNumber: big.NewInt(16), Logs: []*types.Log{}}, true, false
	})
	if final, _, err := h.r.receiptFinalized(context.Background(), hash); err == nil || final {
		t.Fatalf("wrong transaction receipt accepted: final=%v err=%v", final, err)
	}
	if got := h.reconcile(t, job.ID); isTerminalStatus(got.Status) || !nullifierHeld(t, h) {
		t.Fatalf("wrong transaction receipt released job: %+v", got)
	}
}

func TestCanonicalBlockUnavailableRetainsNullifier(t *testing.T) {
	for _, mode := range []string{"error", "missing", "wrong-height"} {
		t.Run(mode, func(t *testing.T) {
			h := newOZHarness(t)
			job, data := h.createSubmittedJob(t)
			hash := h.chain.mine(t, 7, testPool, data, types.ReceiptStatusFailed)
			h.chain.setFinalizedBlock(20)
			h.oz.set(job.OZTxID, func(tx *ozTransaction) { tx.Status = ozStatusFailed; tx.Hash = hash.Hex() })
			overrideChainRPC(t, h, func(method string, params []json.RawMessage) (any, bool, bool) {
				if method != "eth_getBlockByNumber" || string(params[0]) != `"0x10"` {
					return nil, false, false
				}
				switch mode {
				case "error":
					return nil, true, true
				case "missing":
					return nil, true, false
				default:
					return fakeCanonicalHeader(15), true, false
				}
			})
			if got := h.reconcile(t, job.ID); isTerminalStatus(got.Status) || !nullifierHeld(t, h) {
				t.Fatalf("unavailable canonical block released job: %+v", got)
			}
		})
	}
}

// Two hashes have the same sender, nonce, target and calldata, differing only
// in fees. The older hash can be stale/pending/error while the newer is final.
func replacementAttempts(t *testing.T, h *ozHarness, oldStatus string, newStatus uint64) (*Job, common.Hash, common.Hash) {
	t.Helper()
	job, data := h.createSubmittedJob(t)
	var old common.Hash
	if oldStatus == "pending" || oldStatus == "receipt-error" || oldStatus == "transaction-error" {
		old = h.chain.broadcast(t, 7, testPool, data)
	} else {
		old = h.chain.mine(t, 7, testPool, data, types.ReceiptStatusFailed)
	}
	replacement, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{ChainID: testChainID, Nonce: 7, GasTipCap: big.NewInt(2), GasFeeCap: big.NewInt(3), Gas: 500000, To: &testPool, Value: new(big.Int), Data: data}), types.LatestSignerForChainID(testChainID), h.chain.key)
	if err != nil {
		t.Fatal(err)
	}
	h.chain.mu.Lock()
	h.chain.txs[replacement.Hash()] = replacement
	h.chain.receipts[replacement.Hash()] = newStatus
	h.chain.spent = newStatus == types.ReceiptStatusSuccessful
	h.chain.finalizedBlock = 20
	h.chain.mu.Unlock()
	if err := h.r.updateJobCritical(job.ID, func(j *Job) { recordTxHash(j, old.Hex()); recordTxHash(j, replacement.Hash().Hex()) }); err != nil {
		t.Fatal(err)
	}
	h.oz.set(job.OZTxID, func(tx *ozTransaction) {
		tx.Status = ozStatusExpired
		tx.Hash = replacement.Hash().Hex()
		tx.From = testOZSender.Hex()
		tx.Nonce = "7"
	})
	h.chain.setFinalizedNonce(8) // even a nonce proof must not override uncertainty
	if oldStatus != "pending" {
		overrideChainRPC(t, h, func(method string, params []json.RawMessage) (any, bool, bool) {
			if len(params) == 0 || string(params[0]) != `"`+old.Hex()+`"` {
				return nil, false, false
			}
			if (oldStatus == "receipt-error" && method == "eth_getTransactionReceipt") || (oldStatus == "transaction-error" && method == "eth_getTransactionByHash") {
				return nil, true, true
			}
			if method == "eth_getTransactionReceipt" && (oldStatus == "unfinalized" || oldStatus == "orphan") {
				number := big.NewInt(21)
				blockHash := fakeCanonicalHeader(21).Hash()
				if oldStatus == "orphan" {
					number = big.NewInt(16)
					blockHash = common.HexToHash("0xdead")
				}
				return &types.Receipt{Type: types.DynamicFeeTxType, Status: types.ReceiptStatusFailed, CumulativeGasUsed: 21000, GasUsed: 21000, TxHash: old, BlockHash: blockHash, BlockNumber: number, Logs: []*types.Log{}}, true, false
			}
			return nil, false, false
		})
	}
	return job, old, replacement.Hash()
}

func TestFinalizedSuccessWinsOverUncertainOlderAttempt(t *testing.T) {
	for _, mode := range []string{"pending", "receipt-error", "transaction-error", "unfinalized", "orphan"} {
		t.Run(mode, func(t *testing.T) {
			h := newOZHarness(t)
			job, _, replacement := replacementAttempts(t, h, mode, types.ReceiptStatusSuccessful)
			got := h.reconcile(t, job.ID)
			if got.Status != StatusConfirmed || got.TxHash != replacement.Hex() || nullifierHeld(t, h) {
				t.Fatalf("finalized replacement hidden by %s: %+v", mode, got)
			}
		})
	}
}

func TestUncertainOlderAttemptStillBlocksFailureRelease(t *testing.T) {
	for _, mode := range []string{"pending", "receipt-error", "transaction-error", "unfinalized", "orphan"} {
		t.Run(mode, func(t *testing.T) {
			h := newOZHarness(t)
			job, _, _ := replacementAttempts(t, h, mode, types.ReceiptStatusFailed)
			got := h.reconcile(t, job.ID)
			if isTerminalStatus(got.Status) || !nullifierHeld(t, h) {
				t.Fatalf("uncertain %s allowed revert/nonce release: %+v", mode, got)
			}
		})
	}
}
