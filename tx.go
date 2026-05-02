package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// sustainFee: ceil(62 bytes × 100 sat/KB) = 7 sats.
// 62 = 4 ver + 1 in_count + 42 input + 1 out_count + 10 output + 4 locktime
// output = 8 value + 1 script_len + 1 OP_NOP4
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

// buildFanoutTx creates a 1-input N-output transaction splitting value evenly.
// Fee is computed from actual tx size at 100 sat/KB rounded up.
func buildFanoutTx(utxo UTXO, n int, lockScript []byte) (string, []UTXO, error) {
	// 4 ver + 1 in_count + 42 input + 1 out_count + n*(8+1+len(script)) + 4 locktime
	txSize := uint64(4 + 1 + 42 + 1 + n*(8+1+len(lockScript)) + 4)
	fee := (txSize*100 + 999) / 1000 // ceil at 100 sat/KB

	if utxo.Value <= fee {
		return "", nil, fmt.Errorf("value %d too small for fanout fee %d", utxo.Value, fee)
	}
	perOutput := (utxo.Value - fee) / uint64(n)
	if perOutput == 0 {
		return "", nil, fmt.Errorf("per-output value is 0")
	}

	txid := reversedTxID(utxo.TxHash)
	if txid == nil {
		return "", nil, fmt.Errorf("invalid txid: %s", utxo.TxHash)
	}

	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint32(1))
	buf.WriteByte(0x01)
	buf.Write(txid)
	binary.Write(&buf, binary.LittleEndian, uint32(utxo.TxPos))
	buf.WriteByte(0x01)
	buf.WriteByte(0x51)
	binary.Write(&buf, binary.LittleEndian, uint32(0xFFFFFFFF))

	writeVarInt(&buf, uint64(n))
	for i := 0; i < n; i++ {
		binary.Write(&buf, binary.LittleEndian, uint64(perOutput))
		writeVarInt(&buf, uint64(len(lockScript)))
		buf.Write(lockScript)
	}
	binary.Write(&buf, binary.LittleEndian, uint32(0))

	outputs := make([]UTXO, n)
	for i := range outputs {
		outputs[i] = UTXO{TxPos: uint32(i), Value: perOutput}
	}
	return hex.EncodeToString(buf.Bytes()), outputs, nil
}

// buildSustainTx creates the next tx in a chain.
// output = max(0, value - sustainFee). Caller checks newUTXO.Value == 0 to detect chain end.
func buildSustainTx(utxo UTXO, lockScript []byte) (string, UTXO, error) {
	if utxo.Value == 0 {
		return "", UTXO{}, fmt.Errorf("UTXO has 0 value")
	}
	txid := reversedTxID(utxo.TxHash)
	if txid == nil {
		return "", UTXO{}, fmt.Errorf("invalid txid: %s", utxo.TxHash)
	}

	var outValue uint64
	if utxo.Value > sustainFee {
		outValue = utxo.Value - sustainFee
	}

	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint32(1))
	buf.WriteByte(0x01)
	buf.Write(txid)
	binary.Write(&buf, binary.LittleEndian, uint32(utxo.TxPos))
	buf.WriteByte(0x01)
	buf.WriteByte(0x51)
	binary.Write(&buf, binary.LittleEndian, uint32(0xFFFFFFFF))

	buf.WriteByte(0x01)
	binary.Write(&buf, binary.LittleEndian, uint64(outValue))
	writeVarInt(&buf, uint64(len(lockScript)))
	buf.Write(lockScript)

	binary.Write(&buf, binary.LittleEndian, uint32(0))

	return hex.EncodeToString(buf.Bytes()), UTXO{TxPos: 0, Value: outValue}, nil
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
