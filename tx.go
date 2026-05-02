package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// sustainFee: ceil(62 bytes × 100 sat/KB) = 7 sats.
// Raw tx: 4 ver + 1 in_count + 42 input + 1 out_count + 10 output + 4 locktime = 62
// output = 8 value + 1 script_len + 1 OP_NOP4
// EF adds 10 bytes per input (8 prevSatoshis + 1 scriptLen + 1 OP_NOP4) — transport only, not counted for fee.
const sustainFee = 7

func buildLockScript() []byte {
	return []byte{0xB9} // OP_NOP4
}

func scriptToHash(script []byte) string {
	h := sha256.Sum256(script)
	for i, j := 0, len(h)-1; i < j; i, j = i+1, j-1 {
		h[i], h[j] = h[j], h[i]
	}
	return hex.EncodeToString(h[:])
}

// buildFanoutTx creates a 1-input N-output EF transaction splitting value evenly.
// Fee computed from raw tx size at 100 sat/KB rounded up (EF overhead excluded).
func buildFanoutTx(utxo UTXO, n int, lockScript []byte) ([]byte, []UTXO, error) {
	// Raw tx size (no EF overhead): 4+1+42+1 + n*(8+1+len) + 4
	rawSize := uint64(4 + 1 + 42 + 1 + n*(8+1+len(lockScript)) + 4)
	fee := (rawSize*100 + 999) / 1000

	if utxo.Value <= fee {
		return nil, nil, fmt.Errorf("value %d too small for fanout fee %d", utxo.Value, fee)
	}
	perOutput := (utxo.Value - fee) / uint64(n)
	if perOutput == 0 {
		return nil, nil, fmt.Errorf("per-output value is 0")
	}

	txid := reversedTxID(utxo.TxHash)
	if txid == nil {
		return nil, nil, fmt.Errorf("invalid txid: %s", utxo.TxHash)
	}

	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint32(1)) // version
	buf.WriteByte(0x01)                                // 1 input
	writeEFInput(&buf, txid, utxo.TxPos, utxo.Value, lockScript)
	writeVarInt(&buf, uint64(n))
	for i := 0; i < n; i++ {
		binary.Write(&buf, binary.LittleEndian, uint64(perOutput))
		writeVarInt(&buf, uint64(len(lockScript)))
		buf.Write(lockScript)
	}
	binary.Write(&buf, binary.LittleEndian, uint32(0)) // locktime

	outputs := make([]UTXO, n)
	for i := range outputs {
		outputs[i] = UTXO{TxPos: uint32(i), Value: perOutput}
	}
	return buf.Bytes(), outputs, nil
}

// buildSustainTx creates the next EF tx in a chain.
// outValue = max(0, value−sustainFee). Caller checks newUTXO.Value == 0 for chain end.
func buildSustainTx(utxo UTXO, lockScript []byte) ([]byte, UTXO, error) {
	if utxo.Value == 0 {
		return nil, UTXO{}, fmt.Errorf("zero value UTXO")
	}
	txid := reversedTxID(utxo.TxHash)
	if txid == nil {
		return nil, UTXO{}, fmt.Errorf("invalid txid: %s", utxo.TxHash)
	}

	var outValue uint64
	if utxo.Value > sustainFee {
		outValue = utxo.Value - sustainFee
	}

	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint32(1))
	buf.WriteByte(0x01)
	writeEFInput(&buf, txid, utxo.TxPos, utxo.Value, lockScript)
	buf.WriteByte(0x01) // 1 output
	binary.Write(&buf, binary.LittleEndian, uint64(outValue))
	writeVarInt(&buf, uint64(len(lockScript)))
	buf.Write(lockScript)
	binary.Write(&buf, binary.LittleEndian, uint32(0))

	return buf.Bytes(), UTXO{TxPos: 0, Value: outValue}, nil
}

// writeEFInput writes one input in Extended Format:
// [prevTxID][vout][scriptLen][OP_TRUE][sequence][prevSatoshis][prevScriptLen][prevScript]
func writeEFInput(buf *bytes.Buffer, reversedTxid []byte, vout uint32, prevSatoshis uint64, prevScript []byte) {
	buf.Write(reversedTxid)
	binary.Write(buf, binary.LittleEndian, uint32(vout))
	buf.WriteByte(0x01) // unlock script len
	buf.WriteByte(0x51) // OP_TRUE
	binary.Write(buf, binary.LittleEndian, uint32(0xFFFFFFFF)) // sequence
	// EF extension
	binary.Write(buf, binary.LittleEndian, uint64(prevSatoshis))
	writeVarInt(buf, uint64(len(prevScript)))
	buf.Write(prevScript)
}

func reversedTxID(hash string) []byte {
	b, err := hex.DecodeString(hash)
	if err != nil || len(b) != 32 {
		return nil
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return b
}

func writeVarInt(buf *bytes.Buffer, n uint64) {
	switch {
	case n < 0xFD:
		buf.WriteByte(byte(n))
	case n <= 0xFFFF:
		buf.Write([]byte{0xFD, byte(n), byte(n >> 8)})
	case n <= 0xFFFFFFFF:
		buf.Write([]byte{0xFE, byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)})
	default:
		b := make([]byte, 9)
		b[0] = 0xFF
		binary.LittleEndian.PutUint64(b[1:], n)
		buf.Write(b)
	}
}
