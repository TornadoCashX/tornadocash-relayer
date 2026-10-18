package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

func TestFindTransactionEnforcesCreationBound(t *testing.T) {
	cutoff := time.Now().UTC()
	to := common.HexToAddress("0x1111111111111111111111111111111111111111")
	data := "0x1234"
	tests := []struct {
		name string
		txs  []ozTransaction
		want string
		err  string
	}{
		{name: "old match ignored", txs: []ozTransaction{{ID: "old", To: to.Hex(), Data: data, CreatedAt: cutoff.Add(-time.Minute).Format(time.RFC3339Nano)}}},
		{name: "new match selected", txs: []ozTransaction{{ID: "new", To: to.Hex(), Data: data, CreatedAt: cutoff.Add(time.Second).Format(time.RFC3339Nano)}}, want: "new"},
		{name: "invalid time rejected", txs: []ozTransaction{{ID: "bad", To: to.Hex(), Data: data, CreatedAt: "invalid"}}, err: "invalid created_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": tt.txs})
			}))
			defer server.Close()
			client := &ozClient{baseURL: server.URL, relayerID: "relayer", apiKey: strings.Repeat("k", 32), httpClient: server.Client()}
			tx, err := client.FindTransaction(context.Background(), to, data, cutoff)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("error=%v, want containing %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "" && tx != nil {
				t.Fatalf("old transaction matched: %+v", tx)
			}
			if tt.want != "" && (tx == nil || tx.ID != tt.want) {
				t.Fatalf("transaction=%+v, want %s", tx, tt.want)
			}
		})
	}
}

func TestRecordTxHashRejectsMalformedValues(t *testing.T) {
	job := &Job{}
	for _, value := range []string{"", "0x1", common.Hash{}.Hex(), "not-a-hash"} {
		recordTxHash(job, value)
		if job.TxHash != "" || len(job.TxHashes) != 0 {
			t.Fatalf("malformed hash %q was recorded: %+v", value, job)
		}
	}
	valid := common.HexToHash("0x1234").Hex()
	recordTxHash(job, valid)
	if job.TxHash != valid || len(job.TxHashes) != 1 || job.TxHashes[0] != valid {
		t.Fatalf("valid hash not recorded: %+v", job)
	}
	recordTxHash(job, valid)
	if len(job.TxHashes) != 1 {
		t.Fatalf("repeated hash was recorded twice: %+v", job)
	}
}
