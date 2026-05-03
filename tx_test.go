package main

import (
	"testing"
)

// testTxID is a valid 64-char hex txid used across all test files.
const testTxID = "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"

func TestBuildFanoutTx(t *testing.T) {
	ls := buildLockScript()
	tests := []struct {
		name    string
		value   uint64
		n       int
		wantErr bool
	}{
		{"normal_4out", 10_000, 4, false},
		{"minimal_2out", 100, 2, false},
		{"too_small_fee", 5, 2, true},   // 5 <= fee(8)
		{"zero_per_output", 9, 2, true}, // (9-8)/2 == 0
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			utxo := UTXO{TxHash: testTxID, TxPos: 0, Value: tc.value}
			txid, ef, outs, err := buildFanoutTx(utxo, tc.n, ls)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(txid) != 64 {
				t.Errorf("txid length %d, want 64", len(txid))
			}
			if len(ef) == 0 {
				t.Error("EF bytes empty")
			}
			if len(outs) != tc.n {
				t.Errorf("got %d outputs, want %d", len(outs), tc.n)
			}
			totalOut := uint64(0)
			for _, o := range outs {
				totalOut += o.Value
			}
			if totalOut >= tc.value {
				t.Errorf("total output %d >= input %d (no fee deducted)", totalOut, tc.value)
			}
		})
	}
}

func TestBuildFanoutTxInvalidTxID(t *testing.T) {
	ls := buildLockScript()
	utxo := UTXO{TxHash: "not-a-txid", TxPos: 0, Value: 10_000}
	_, _, _, err := buildFanoutTx(utxo, 2, ls)
	if err == nil {
		t.Fatal("expected error for invalid txid, got nil")
	}
}

func TestBuildSustainTx(t *testing.T) {
	ls := buildLockScript()
	tests := []struct {
		name      string
		value     uint64
		wantErr   bool
		wantValue uint64
	}{
		{"normal", 100, false, 100 - sustainFee},
		{"chain_end_equal", sustainFee, false, 0},    // value == fee → output 0
		{"chain_end_less", sustainFee - 1, false, 0}, // value < fee → output 0
		{"zero_value", 0, true, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			utxo := UTXO{TxHash: testTxID, TxPos: 0, Value: tc.value}
			txid, ef, newUTXO, err := buildSustainTx(utxo, ls)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(txid) != 64 {
				t.Errorf("txid length %d, want 64", len(txid))
			}
			if len(ef) == 0 {
				t.Error("EF bytes empty")
			}
			if newUTXO.Value != tc.wantValue {
				t.Errorf("newUTXO.Value = %d, want %d", newUTXO.Value, tc.wantValue)
			}
		})
	}
}

func TestBuildSustainTxInvalidTxID(t *testing.T) {
	ls := buildLockScript()
	utxo := UTXO{TxHash: "not-a-txid", TxPos: 0, Value: 100}
	_, _, _, err := buildSustainTx(utxo, ls)
	if err == nil {
		t.Fatal("expected error for invalid txid, got nil")
	}
}

func FuzzBuildSustainTx(f *testing.F) {
	ls := buildLockScript()
	f.Add(testTxID, uint32(0), uint64(1000))
	f.Add(testTxID, uint32(0), uint64(sustainFee))
	f.Add(testTxID, uint32(0), uint64(1))
	f.Fuzz(func(t *testing.T, txid string, pos uint32, value uint64) {
		if value == 0 {
			return
		}
		utxo := UTXO{TxHash: txid, TxPos: pos, Value: value}
		_, ef, newUTXO, err := buildSustainTx(utxo, ls)
		if err != nil {
			return
		}
		if len(ef) == 0 {
			t.Error("EF bytes empty on success")
		}
		if newUTXO.Value > value {
			t.Errorf("newUTXO.Value %d > input value %d", newUTXO.Value, value)
		}
	})
}
