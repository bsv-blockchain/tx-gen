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
// EF adds 10 bytes per input (8 prevSatoshis + 1 scriptLen + 1 OP_NOP4) — transport only.
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

// buildFanoutTx creates a 1-input N-output EF transaction.
// Returns the txid (computed from raw bytes), EF bytes for broadcast, and the new outputs.
func buildFanoutTx(utxo UTXO, n int, lockScript []byte) (txid string, efBytes []byte, outputs []UTXO, err error) {
	// Fee from raw tx size (EF overhead excluded): 4+1+42+1+n*(8+1+len)+4
	rawSize := uint64(4 + 1 + 42 + 1 + n*(8+1+len(lockScript)) + 4)
	fee := (rawSize*100 + 999) / 1000

	if utxo.Value <= fee {
		return "", nil, nil, fmt.Errorf("value %d too small for fanout fee %d", utxo.Value, fee)
	}
	perOutput := (utxo.Value - fee) / uint64(n)
	if perOutput == 0 {
		return "", nil, nil, fmt.Errorf("per-output value is 0")
	}

	prevTxID := reversedTxID(utxo.TxHash)
	if prevTxID == nil {
		return "", nil, nil, fmt.Errorf("invalid txid: %s", utxo.TxHash)
	}

	var rawBuf, efBuf bytes.Buffer

	for _, buf := range []*bytes.Buffer{&rawBuf, &efBuf} {
		binary.Write(buf, binary.LittleEndian, uint32(1)) // version
		buf.WriteByte(0x01)                               // 1 input
	}
	writeRawInput(&rawBuf, prevTxID, utxo.TxPos)
	writeEFInput(&efBuf, prevTxID, utxo.TxPos, utxo.Value, lockScript)

	for _, buf := range []*bytes.Buffer{&rawBuf, &efBuf} {
		writeVarInt(buf, uint64(n))
		for i := 0; i < n; i++ {
			writeOutput(buf, perOutput, lockScript)
		}
		binary.Write(buf, binary.LittleEndian, uint32(0)) // locktime
	}

	out := make([]UTXO, n)
	for i := range out {
		out[i] = UTXO{TxPos: uint32(i), Value: perOutput}
	}
	return computeTxID(rawBuf.Bytes()), efBuf.Bytes(), out, nil
}

// buildSustainTx creates the next EF tx in a chain.
// Returns the txid (computed from raw bytes), EF bytes, and the new UTXO.
// newUTXO.Value == 0 signals the chain is finished.
func buildSustainTx(utxo UTXO, lockScript []byte) (txid string, efBytes []byte, newUTXO UTXO, err error) {
	if utxo.Value == 0 {
		return "", nil, UTXO{}, fmt.Errorf("zero value UTXO")
	}
	prevTxID := reversedTxID(utxo.TxHash)
	if prevTxID == nil {
		return "", nil, UTXO{}, fmt.Errorf("invalid txid: %s", utxo.TxHash)
	}

	var outValue uint64
	if utxo.Value > sustainFee {
		outValue = utxo.Value - sustainFee
	}

	var rawBuf, efBuf bytes.Buffer

	for _, buf := range []*bytes.Buffer{&rawBuf, &efBuf} {
		binary.Write(buf, binary.LittleEndian, uint32(1))
		buf.WriteByte(0x01)
	}
	writeRawInput(&rawBuf, prevTxID, utxo.TxPos)
	writeEFInput(&efBuf, prevTxID, utxo.TxPos, utxo.Value, lockScript)

	for _, buf := range []*bytes.Buffer{&rawBuf, &efBuf} {
		buf.WriteByte(0x01) // 1 output
		writeOutput(buf, outValue, lockScript)
		binary.Write(buf, binary.LittleEndian, uint32(0))
	}

	return computeTxID(rawBuf.Bytes()), efBuf.Bytes(), UTXO{TxPos: 0, Value: outValue}, nil
}

// writeRawInput writes standard (non-EF) input: prevTxID + vout + OP_TRUE + sequence.
func writeRawInput(buf *bytes.Buffer, reversedTxid []byte, vout uint32) {
	buf.Write(reversedTxid)
	binary.Write(buf, binary.LittleEndian, uint32(vout))
	buf.WriteByte(0x01) // script len
	buf.WriteByte(0x51) // OP_TRUE
	binary.Write(buf, binary.LittleEndian, uint32(0xFFFFFFFF))
}

// writeEFInput writes an EF input: standard fields + [prevSatoshis][prevScriptLen][prevScript].
func writeEFInput(buf *bytes.Buffer, reversedTxid []byte, vout uint32, prevSatoshis uint64, prevScript []byte) {
	writeRawInput(buf, reversedTxid, vout)
	binary.Write(buf, binary.LittleEndian, uint64(prevSatoshis))
	writeVarInt(buf, uint64(len(prevScript)))
	buf.Write(prevScript)
}

func writeOutput(buf *bytes.Buffer, value uint64, script []byte) {
	binary.Write(buf, binary.LittleEndian, uint64(value))
	writeVarInt(buf, uint64(len(script)))
	buf.Write(script)
}

// computeTxID returns the double-SHA256 of rawBytes, reversed to big-endian hex.
func computeTxID(rawBytes []byte) string {
	h1 := sha256.Sum256(rawBytes)
	h2 := sha256.Sum256(h1[:])
	for i, j := 0, len(h2)-1; i < j; i, j = i+1, j-1 {
		h2[i], h2[j] = h2[j], h2[i]
	}
	return hex.EncodeToString(h2[:])
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
