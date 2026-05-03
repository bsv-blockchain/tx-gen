package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	sdktx "github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
)

// sustainFee: ceil(62 bytes × 100 sat/KB) = 7 sats.
const sustainFee = 7
const p2pkhSustainFee = 20

const (
	txModeOPNOP4 = "op_nop4"
	txModeP2PKH  = "p2pkh"
)

type TxMode struct {
	Name       string
	LockScript *script.Script
	Unlocker   sdktx.UnlockingScriptTemplate
	SustainFee uint64
	Address    string
}

func NewTxModeFromConfig(cfg *Config) (*TxMode, error) {
	if cfg != nil && strings.TrimSpace(cfg.PrivateKey) != "" {
		return newP2PKHTxMode(cfg.PrivateKey, cfg.SustainFee)
	}
	fee := uint64(sustainFee)
	if cfg != nil && cfg.SustainFee > 0 {
		fee = cfg.SustainFee
	}
	return newOPNOP4TxMode(buildLockScript(), fee), nil
}

func newOPNOP4TxMode(lockScript *script.Script, fee uint64) *TxMode {
	if lockScript == nil {
		lockScript = buildLockScript()
	}
	if fee == 0 {
		fee = sustainFee
	}
	return &TxMode{
		Name:       txModeOPNOP4,
		LockScript: lockScript,
		SustainFee: fee,
	}
}

func newP2PKHTxMode(privateKey string, fee uint64) (*TxMode, error) {
	priv, err := parsePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	address, err := script.NewAddressFromPublicKey(priv.PubKey(), true)
	if err != nil {
		return nil, fmt.Errorf("derive P2PKH address: %w", err)
	}
	lockScript, err := p2pkh.Lock(address)
	if err != nil {
		return nil, fmt.Errorf("build P2PKH lock script: %w", err)
	}
	unlocker, err := p2pkh.Unlock(priv, nil)
	if err != nil {
		return nil, fmt.Errorf("build P2PKH unlocker: %w", err)
	}
	if fee == 0 {
		fee = p2pkhSustainFee
	}
	return &TxMode{
		Name:       txModeP2PKH,
		LockScript: lockScript,
		Unlocker:   unlocker,
		SustainFee: fee,
		Address:    address.AddressString,
	}, nil
}

func parsePrivateKey(privateKey string) (*ec.PrivateKey, error) {
	key := strings.TrimSpace(privateKey)
	if key == "" {
		return nil, fmt.Errorf("PRIVATE_KEY is required for P2PKH mode")
	}
	if priv, err := ec.PrivateKeyFromWif(key); err == nil {
		return priv, nil
	}
	if priv, err := ec.PrivateKeyFromHex(key); err == nil {
		return priv, nil
	}
	return nil, fmt.Errorf("PRIVATE_KEY must be a valid WIF or 32-byte hex private key")
}

func (m *TxMode) inputSize() int {
	if m != nil && m.Unlocker != nil {
		scriptLen := int(m.Unlocker.EstimateLength(sdktx.NewTransaction(), 0))
		return 32 + 4 + varIntSize(scriptLen) + scriptLen + 4
	}
	scriptLen := len(*unlockScript())
	return 32 + 4 + varIntSize(scriptLen) + scriptLen + 4
}

func varIntSize(n int) int {
	switch {
	case n < 0xfd:
		return 1
	case n <= 0xffff:
		return 3
	case n <= 0xffffffff:
		return 5
	default:
		return 9
	}
}

func normalizedTxMode(mode *TxMode) *TxMode {
	if mode == nil {
		return newOPNOP4TxMode(buildLockScript(), sustainFee)
	}
	if mode.LockScript != nil && mode.SustainFee != 0 {
		return mode
	}
	copyMode := *mode
	if copyMode.LockScript == nil {
		copyMode.LockScript = buildLockScript()
	}
	if copyMode.SustainFee == 0 {
		copyMode.SustainFee = sustainFee
		if copyMode.Name == txModeP2PKH {
			copyMode.SustainFee = p2pkhSustainFee
		}
	}
	return &copyMode
}

func buildLockScript() *script.Script {
	s := script.Script{script.OpNOP4} // 0xB4
	return &s
}

func unlockScript() *script.Script {
	s := script.Script{script.OpTRUE} // 0x51
	return &s
}

func makeInput(utxo UTXO, mode *TxMode) (*sdktx.TransactionInput, error) {
	mode = normalizedTxMode(mode)
	txidHash, err := chainhash.NewHashFromHex(utxo.TxHash)
	if err != nil {
		return nil, fmt.Errorf("invalid txid %s: %w", utxo.TxHash, err)
	}
	in := &sdktx.TransactionInput{
		SourceTXID:       txidHash,
		SourceTxOutIndex: utxo.TxPos,
		SequenceNumber:   sdktx.DefaultSequenceNumber,
	}
	if mode != nil && mode.Unlocker != nil {
		in.UnlockingScriptTemplate = mode.Unlocker
	} else {
		in.UnlockingScript = unlockScript()
	}
	in.SetSourceTxOutput(&sdktx.TransactionOutput{
		Satoshis:      utxo.Value,
		LockingScript: mode.LockScript,
	})
	return in, nil
}

// buildFanoutTx creates a 1-input N-output EF transaction.
func buildFanoutTx(utxo UTXO, n int, lockScript *script.Script) (txid string, efBytes []byte, outputs []UTXO, err error) {
	return buildFanoutTxWithMode(utxo, n, newOPNOP4TxMode(lockScript, sustainFee))
}

func buildFanoutTxWithMode(utxo UTXO, n int, mode *TxMode) (txid string, efBytes []byte, outputs []UTXO, err error) {
	mode = normalizedTxMode(mode)
	if n <= 0 {
		return "", nil, nil, fmt.Errorf("fanout count must be positive")
	}
	lockScriptLen := len(*mode.LockScript)
	rawSize := uint64(4 + varIntSize(1) + mode.inputSize() + varIntSize(n) + n*(8+varIntSize(lockScriptLen)+lockScriptLen) + 4)
	fee := (rawSize*100 + 999) / 1000

	if utxo.Value <= fee {
		return "", nil, nil, fmt.Errorf("value %d too small for fanout fee %d", utxo.Value, fee)
	}
	perOutput := (utxo.Value - fee) / uint64(n)
	if perOutput == 0 {
		return "", nil, nil, fmt.Errorf("per-output value is 0")
	}

	tx := sdktx.NewTransaction()
	in, err := makeInput(utxo, mode)
	if err != nil {
		return "", nil, nil, err
	}
	tx.AddInput(in)

	for i := 0; i < n; i++ {
		tx.AddOutput(&sdktx.TransactionOutput{
			Satoshis:      perOutput,
			LockingScript: mode.LockScript,
		})
	}
	if mode.Unlocker != nil {
		if err := tx.Sign(); err != nil {
			return "", nil, nil, fmt.Errorf("sign fanout: %w", err)
		}
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
	return buildSustainTxWithMode(utxo, newOPNOP4TxMode(lockScript, sustainFee))
}

func buildSustainTxWithMode(utxo UTXO, mode *TxMode) (txid string, efBytes []byte, newUTXO UTXO, err error) {
	mode = normalizedTxMode(mode)
	if utxo.Value == 0 {
		return "", nil, UTXO{}, fmt.Errorf("zero value UTXO")
	}

	var outValue uint64
	if utxo.Value > mode.SustainFee {
		outValue = utxo.Value - mode.SustainFee
	}

	tx := sdktx.NewTransaction()
	in, err := makeInput(utxo, mode)
	if err != nil {
		return "", nil, UTXO{}, err
	}
	tx.AddInput(in)
	tx.AddOutput(&sdktx.TransactionOutput{
		Satoshis:      outValue,
		LockingScript: mode.LockScript,
	})
	if mode.Unlocker != nil {
		if err := tx.Sign(); err != nil {
			return "", nil, UTXO{}, fmt.Errorf("sign sustain: %w", err)
		}
	}

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
