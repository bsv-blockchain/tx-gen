// Package txbuilder builds the transactions tx-gen broadcasts: fanouts that
// split one UTXO into many during bootstrap, and the one-in/one-out sustain
// hops that keep every chain running.
//
// Transactions are returned EF-encoded (BRC-30 Extended Format), which Arcade
// accepts without having to look up the parent transactions.
package txbuilder

import (
	"fmt"

	safeconversion "github.com/bsv-blockchain/go-safe-conversion"
	"github.com/bsv-blockchain/go-sdk/chainhash"
	sdktx "github.com/bsv-blockchain/go-sdk/transaction"
)

const (
	// MaxTxBytes is the largest fanout transaction BuildFanout will create.
	MaxTxBytes uint64 = 100_000_000

	// feeRateSatsPerKB is the fee rate paid by fanout transactions.
	feeRateSatsPerKB uint64 = 100

	// outputValueBytes is the serialized size of an output's satoshi amount.
	outputValueBytes uint64 = 8

	// txFrameBytes is the version (4) plus lock time (4) of a transaction.
	txFrameBytes uint64 = 8
)

// Tx is a built transaction that is ready to broadcast.
type Tx struct {
	// TxID is the hex transaction id.
	TxID string

	// EF is the transaction serialized in Extended Format (BRC-30).
	EF []byte
}

// BuildFanout spends utxo into n equal outputs locked by mode.
//
// The fee is 100 sat/KB of the serialized size, rounded up. What remains after
// the fee is split evenly across the outputs, and any remainder that cannot be
// split goes to the miner. The returned outputs carry the new transaction's id.
func BuildFanout(utxo UTXO, n int, mode *Mode) (Tx, []UTXO, error) {
	if mode == nil {
		return Tx{}, nil, ErrNilMode
	}
	if n <= 0 {
		return Tx{}, nil, ErrInvalidFanoutCount
	}
	count, err := safeconversion.IntToUint64(n)
	if err != nil {
		return Tx{}, nil, fmt.Errorf("fanout count %d: %w", n, err)
	}
	if count > MaxTxBytes {
		return Tx{}, nil, fmt.Errorf("%w: %d outputs exceed %d bytes", ErrTxTooLarge, n, MaxTxBytes)
	}
	vouts, err := safeconversion.Uint64ToUint32(count)
	if err != nil {
		return Tx{}, nil, fmt.Errorf("fanout count %d: %w", n, err)
	}

	lockScriptLen := uint64(len(*mode.LockScript))
	outputSize := outputValueBytes + varIntSize(lockScriptLen) + lockScriptLen
	rawSize := txFrameBytes + varIntSize(1) + mode.inputSize() + varIntSize(count) + count*outputSize
	if rawSize > MaxTxBytes {
		return Tx{}, nil, fmt.Errorf("%w: fanout size %d exceeds %d bytes", ErrTxTooLarge, rawSize, MaxTxBytes)
	}

	fee := (rawSize*feeRateSatsPerKB + 999) / 1000
	if utxo.Value <= fee {
		return Tx{}, nil, fmt.Errorf("%w: value %d cannot pay fanout fee %d", ErrInsufficientValue, utxo.Value, fee)
	}
	perOutput := (utxo.Value - fee) / count
	if perOutput == 0 {
		return Tx{}, nil, fmt.Errorf("%w: value %d leaves 0 sats for each of %d outputs", ErrInsufficientValue, utxo.Value, n)
	}

	tx := sdktx.NewTransaction()
	input, err := newInput(utxo, mode)
	if err != nil {
		return Tx{}, nil, err
	}
	tx.AddInput(input)
	for range count {
		tx.AddOutput(&sdktx.TransactionOutput{Satoshis: perOutput, LockingScript: mode.LockScript})
	}

	built, err := finalize(tx, mode, "fanout")
	if err != nil {
		return Tx{}, nil, err
	}

	outputs := make([]UTXO, 0, n)
	for vout := range vouts {
		outputs = append(outputs, UTXO{TxHash: built.TxID, TxPos: vout, Value: perOutput})
	}
	return built, outputs, nil
}

// BuildSustain spends utxo into a single output worth utxo.Value minus the
// mode's sustain fee, locked by mode.
//
// When utxo.Value is at most the fee, the output is worth zero: the chain has
// reached its end, and the returned UTXO has Value 0. The returned UTXO
// carries the new transaction's id.
func BuildSustain(utxo UTXO, mode *Mode) (Tx, UTXO, error) {
	if mode == nil {
		return Tx{}, UTXO{}, ErrNilMode
	}
	if utxo.Value == 0 {
		return Tx{}, UTXO{}, ErrZeroValueUTXO
	}

	var outValue uint64
	if utxo.Value > mode.SustainFee {
		outValue = utxo.Value - mode.SustainFee
	}

	tx := sdktx.NewTransaction()
	input, err := newInput(utxo, mode)
	if err != nil {
		return Tx{}, UTXO{}, err
	}
	tx.AddInput(input)
	tx.AddOutput(&sdktx.TransactionOutput{Satoshis: outValue, LockingScript: mode.LockScript})

	built, err := finalize(tx, mode, "sustain")
	if err != nil {
		return Tx{}, UTXO{}, err
	}
	return built, UTXO{TxHash: built.TxID, TxPos: 0, Value: outValue}, nil
}

// newInput returns an input spending utxo, with the source output attached so
// the transaction can be EF-encoded and signed.
func newInput(utxo UTXO, mode *Mode) (*sdktx.TransactionInput, error) {
	txid, err := chainhash.NewHashFromHex(utxo.TxHash)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrInvalidTxID, utxo.TxHash, err)
	}
	input := &sdktx.TransactionInput{
		SourceTXID:       txid,
		SourceTxOutIndex: utxo.TxPos,
		SequenceNumber:   sdktx.DefaultSequenceNumber,
	}
	if mode.Unlocker != nil {
		input.UnlockingScriptTemplate = mode.Unlocker
	} else {
		input.UnlockingScript = opTrueScript()
	}
	input.SetSourceTxOutput(&sdktx.TransactionOutput{
		Satoshis:      utxo.Value,
		LockingScript: mode.LockScript,
	})
	return input, nil
}

// finalize signs tx when mode requires it and encodes it as EF.
func finalize(tx *sdktx.Transaction, mode *Mode, kind string) (Tx, error) {
	if mode.Unlocker != nil {
		if err := tx.Sign(); err != nil {
			return Tx{}, fmt.Errorf("sign %s: %w", kind, err)
		}
	}
	ef, err := tx.EF()
	if err != nil {
		return Tx{}, fmt.Errorf("EF encode %s: %w", kind, err)
	}
	return Tx{TxID: tx.TxID().String(), EF: ef}, nil
}
