package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"
	sdktx "github.com/bsv-blockchain/go-sdk/transaction"
)

// testTxID is a valid 64-char hex txid used across all test files.
const testTxID = "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"
const testP2PKHWIF = "cNGwGSc7KRrTmdLUZ54fiSXWbhLNDc2Eg5zNucgQxyQCzuQ5YRDq"

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

func TestTxModeDefaultsToCodeSeparator(t *testing.T) {
	mode, err := NewTxModeFromConfig(&Config{})
	if err != nil {
		t.Fatalf("NewTxModeFromConfig: %v", err)
	}
	if mode.Name != txModeCodeSeparator {
		t.Fatalf("mode.Name = %q, want %q", mode.Name, txModeCodeSeparator)
	}
	if mode.SustainFee != sustainFee {
		t.Fatalf("mode.SustainFee = %d, want %d", mode.SustainFee, sustainFee)
	}
	if mode.Unlocker != nil {
		t.Fatal("OP_CODESEPARATOR mode should not have an unlocking template")
	}
	if got, want := scriptHash(mode.LockScript), scriptHash(buildLockScript()); got != want {
		t.Fatalf("script hash = %s, want %s", got, want)
	}
}

func TestDefaultLockScriptIsSingleCodeSeparator(t *testing.T) {
	lockScript := buildLockScript()
	if got := []byte(*lockScript); len(got) != 1 || got[0] != script.OpCODESEPARATOR {
		t.Fatalf("lock script = %x, want single OP_CODESEPARATOR %x", got, script.OpCODESEPARATOR)
	}
}

func TestTxModeUsesP2PKHWhenPrivateKeyIsSet(t *testing.T) {
	mode, err := NewTxModeFromConfig(&Config{PrivateKey: testP2PKHWIF})
	if err != nil {
		t.Fatalf("NewTxModeFromConfig: %v", err)
	}
	if mode.Name != txModeP2PKH {
		t.Fatalf("mode.Name = %q, want %q", mode.Name, txModeP2PKH)
	}
	if mode.SustainFee != p2pkhSustainFee {
		t.Fatalf("mode.SustainFee = %d, want %d", mode.SustainFee, p2pkhSustainFee)
	}
	if mode.Unlocker == nil {
		t.Fatal("P2PKH mode should have an unlocking template")
	}
	if mode.Address == "" {
		t.Fatal("P2PKH address is empty")
	}
	if !mode.LockScript.IsP2PKH() {
		t.Fatalf("lock script %x is not P2PKH", []byte(*mode.LockScript))
	}
	if got, want := scriptHash(mode.LockScript), scriptHash(buildLockScript()); got == want {
		t.Fatalf("P2PKH script hash should differ from default script hash %s", want)
	}
}

func TestTxModeRejectsInvalidPrivateKey(t *testing.T) {
	_, err := NewTxModeFromConfig(&Config{PrivateKey: "not-a-private-key"})
	if err == nil {
		t.Fatal("expected invalid private key error, got nil")
	}
	if !strings.Contains(err.Error(), "PRIVATE_KEY") {
		t.Fatalf("error = %q, want PRIVATE_KEY context", err.Error())
	}
}

func TestBuildSustainTxP2PKHSignsInput(t *testing.T) {
	mode, err := NewTxModeFromConfig(&Config{PrivateKey: testP2PKHWIF})
	if err != nil {
		t.Fatalf("NewTxModeFromConfig: %v", err)
	}
	utxo := UTXO{TxHash: testTxID, TxPos: 0, Value: 100}

	txid, ef, newUTXO, err := buildSustainTxWithMode(utxo, mode)
	if err != nil {
		t.Fatalf("buildSustainTxWithMode: %v", err)
	}
	if newUTXO.Value != 100-p2pkhSustainFee {
		t.Fatalf("newUTXO.Value = %d, want %d", newUTXO.Value, 100-p2pkhSustainFee)
	}
	tx, err := sdktx.NewTransactionFromBytes(ef)
	if err != nil {
		t.Fatalf("parse EF: %v", err)
	}
	if got := tx.TxID().String(); got != txid {
		t.Fatalf("txid = %s, want parsed txid %s", txid, got)
	}
	if len(tx.Inputs) != 1 {
		t.Fatalf("inputs = %d, want 1", len(tx.Inputs))
	}
	unlock := tx.Inputs[0].UnlockingScript
	if unlock == nil || len(*unlock) == 0 {
		t.Fatal("P2PKH input was not signed")
	}
	if len(tx.Outputs) != 1 || !tx.Outputs[0].LockingScript.IsP2PKH() {
		t.Fatalf("output locking script is not P2PKH")
	}
}

func TestLoadConfigDefaultsAndRedactsPrivateKey(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "secret")
	t.Setenv("PRIVATE_KEY", testP2PKHWIF)
	t.Setenv("ARCADE_CALLBACK_TOKEN", "callback-secret")
	t.Setenv("SUSTAIN_FEE", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.SustainFee != p2pkhSustainFee {
		t.Fatalf("SustainFee = %d, want %d", cfg.SustainFee, p2pkhSustainFee)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if strings.Contains(string(b), testP2PKHWIF) {
		t.Fatalf("Config MarshalJSON leaked private key: %s", b)
	}
	if strings.Contains(string(b), "callback-secret") {
		t.Fatalf("Config MarshalJSON leaked callback token: %s", b)
	}
	view, err := json.Marshal(redactedConfig(cfg))
	if err != nil {
		t.Fatalf("redactedConfig marshal: %v", err)
	}
	if strings.Contains(string(view), testP2PKHWIF) {
		t.Fatalf("redactedConfig leaked private key: %s", view)
	}
	if strings.Contains(string(view), "callback-secret") {
		t.Fatalf("redactedConfig leaked callback token: %s", view)
	}
}

func TestEnsureStateMatchesTxModeRejectsMismatchedState(t *testing.T) {
	store := newTestStore(t)
	if err := store.SetMeta(metaLockScriptHash, scriptHash(buildLockScript())); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	mode, err := NewTxModeFromConfig(&Config{PrivateKey: testP2PKHWIF})
	if err != nil {
		t.Fatalf("NewTxModeFromConfig: %v", err)
	}
	if err := ensureStateMatchesTxMode(store, mode, true); err == nil {
		t.Fatal("expected state mode mismatch error, got nil")
	}
}

func TestEnsureStateMatchesTxModeRejectsLegacyP2PKHState(t *testing.T) {
	store := newTestStore(t)
	if err := store.SaveUTXO(UTXO{TxHash: testTxID, TxPos: 0, Value: 100}); err != nil {
		t.Fatalf("SaveUTXO: %v", err)
	}
	mode, err := NewTxModeFromConfig(&Config{PrivateKey: testP2PKHWIF})
	if err != nil {
		t.Fatalf("NewTxModeFromConfig: %v", err)
	}
	err = ensureStateMatchesTxMode(store, mode, true)
	if err == nil || !strings.Contains(err.Error(), "without lock-script metadata") {
		t.Fatalf("ensureStateMatchesTxMode error = %v, want legacy P2PKH state refusal", err)
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
