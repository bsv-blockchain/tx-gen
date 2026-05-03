package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	sdktx "github.com/bsv-blockchain/go-sdk/transaction"
)

// sustainFee: ceil(62 bytes × 100 sat/KB) = 7 sats.
const sustainFee = 7

func buildLockScript() *script.Script {
	s := script.Script{script.OpNOP10} // 0xB9
	return &s
}

func unlockScript() *script.Script {
	s := script.Script{script.OpTRUE} // 0x51
	return &s
}

func makeInput(utxo UTXO, lockingScript *script.Script) (*sdktx.TransactionInput, error) {
	txidHash, err := chainhash.NewHashFromHex(utxo.TxHash)
	if err != nil {
		return nil, fmt.Errorf("invalid txid %s: %w", utxo.TxHash, err)
	}
	in := &sdktx.TransactionInput{
		SourceTXID:       txidHash,
		SourceTxOutIndex: utxo.TxPos,
		SequenceNumber:   sdktx.DefaultSequenceNumber,
		UnlockingScript:  unlockScript(),
	}
	in.SetSourceTxOutput(&sdktx.TransactionOutput{
		Satoshis:      utxo.Value,
		LockingScript: lockingScript,
	})
	return in, nil
}

// buildFanoutTx creates a 1-input N-output EF transaction.
func buildFanoutTx(utxo UTXO, n int, lockScript *script.Script) (txid string, efBytes []byte, outputs []UTXO, err error) {
	rawSize := uint64(4 + 1 + 42 + 1 + n*(8+1+len(*lockScript)) + 4)
	fee := (rawSize*100 + 999) / 1000

	if utxo.Value <= fee {
		return "", nil, nil, fmt.Errorf("value %d too small for fanout fee %d", utxo.Value, fee)
	}
	perOutput := (utxo.Value - fee) / uint64(n)
	if perOutput == 0 {
		return "", nil, nil, fmt.Errorf("per-output value is 0")
	}

	tx := sdktx.NewTransaction()
	in, err := makeInput(utxo, lockScript)
	if err != nil {
		return "", nil, nil, err
	}
	tx.AddInput(in)

	for i := 0; i < n; i++ {
		tx.AddOutput(&sdktx.TransactionOutput{
			Satoshis:      perOutput,
			LockingScript: lockScript,
		})
	}

	ef, err := tx.EF()
	if err != nil {
		return "", nil, nil, fmt.Errorf("EF encode: %w", err)
	}

	txidStr := tx.TxID().String()
	outs := make([]UTXO, n)
	for i := range outs {
		outs[i] = UTXO{TxPos: uint32(i), Value: perOutput}
	}
	return txidStr, ef, outs, nil
}

// buildSustainTx creates the next EF tx in a chain.
// newUTXO.Value == 0 signals the chain is finished.
func buildSustainTx(utxo UTXO, lockScript *script.Script) (txid string, efBytes []byte, newUTXO UTXO, err error) {
	if utxo.Value == 0 {
		return "", nil, UTXO{}, fmt.Errorf("zero value UTXO")
	}

	var outValue uint64
	if utxo.Value > sustainFee {
		outValue = utxo.Value - sustainFee
	}

	tx := sdktx.NewTransaction()
	in, err := makeInput(utxo, lockScript)
	if err != nil {
		return "", nil, UTXO{}, err
	}
	tx.AddInput(in)
	tx.AddOutput(&sdktx.TransactionOutput{
		Satoshis:      outValue,
		LockingScript: lockScript,
	})

	ef, err := tx.EF()
	if err != nil {
		return "", nil, UTXO{}, fmt.Errorf("EF encode: %w", err)
	}

	return tx.TxID().String(), ef, UTXO{TxPos: 0, Value: outValue}, nil
}

func scriptHash(s *script.Script) string {
	h := sha256.Sum256(*s)
	for i, j := 0, len(h)-1; i < j; i, j = i+1, j-1 {
		h[i], h[j] = h[j], h[i]
	}
	return hex.EncodeToString(h[:])
}
