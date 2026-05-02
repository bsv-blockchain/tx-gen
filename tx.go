package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

const (
	sustainFee  = 100           // sats: 1-in 1-out tx (~66 bytes)
	fanoutFee   = 2000          // sats: 1-in 100-out tx (~1452 bytes)
	terminalMin = 2 * sustainFee // spend to OP_RETURN when value drops to this
)

func buildLockScript() []byte {
	// "acade2 OP_DROP" = 03 AC AD E2 75
	return []byte{0x03, 0xAC, 0xAD, 0xE2, 0x75}
}

func scriptToHash(script []byte) string {
	h := sha256.Sum256(script)
	for i, j := 0, len(h)-1; i < j; i, j = i+1, j-1 {
		h[i], h[j] = h[j], h[i]
	}
	return hex.EncodeToString(h[:])
}

// buildFanoutTx creates a 1-input N-output transaction splitting value evenly.
func buildFanoutTx(utxo UTXO, n int, lockScript []byte) (string, []UTXO, error) {
	if utxo.Value <= fanoutFee {
		return "", nil, fmt.Errorf("value %d too small for fanout fee %d", utxo.Value, fanoutFee)
	}
	perOutput := (utxo.Value - uint64(fanoutFee)) / uint64(n)
	if perOutput == 0 {
		return "", nil, fmt.Errorf("per-output value is 0")
	}

	txid := reversedTxID(utxo.TxHash)
	if txid == nil {
		return "", nil, fmt.Errorf("invalid txid: %s", utxo.TxHash)
	}

	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint32(1)) // version
	buf.WriteByte(0x01)                                // 1 input
	buf.Write(txid)
	binary.Write(&buf, binary.LittleEndian, uint32(utxo.TxPos))
	buf.WriteByte(0x01) // script len
	buf.WriteByte(0x51) // OP_TRUE
	binary.Write(&buf, binary.LittleEndian, uint32(0xFFFFFFFF))

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
	return hex.EncodeToString(buf.Bytes()), outputs, nil
}

// buildSustainTx creates a 1-in 1-out chain-continuation transaction.
func buildSustainTx(utxo UTXO, lockScript []byte) (string, UTXO, error) {
	if utxo.Value <= sustainFee {
		return "", UTXO{}, fmt.Errorf("value %d <= fee %d", utxo.Value, sustainFee)
	}
	txid := reversedTxID(utxo.TxHash)
	if txid == nil {
		return "", UTXO{}, fmt.Errorf("invalid txid: %s", utxo.TxHash)
	}

	outValue := utxo.Value - uint64(sustainFee)

	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint32(1))
	buf.WriteByte(0x01)
	buf.Write(txid)
	binary.Write(&buf, binary.LittleEndian, uint32(utxo.TxPos))
	buf.WriteByte(0x01)
	buf.WriteByte(0x51)
	binary.Write(&buf, binary.LittleEndian, uint32(0xFFFFFFFF))

	buf.WriteByte(0x01) // 1 output
	binary.Write(&buf, binary.LittleEndian, uint64(outValue))
	writeVarInt(&buf, uint64(len(lockScript)))
	buf.Write(lockScript)

	binary.Write(&buf, binary.LittleEndian, uint32(0))

	return hex.EncodeToString(buf.Bytes()), UTXO{TxPos: 0, Value: outValue}, nil
}

// buildTerminalTx burns the UTXO into OP_FALSE OP_RETURN; full value becomes miner fee.
func buildTerminalTx(utxo UTXO) (string, error) {
	txid := reversedTxID(utxo.TxHash)
	if txid == nil {
		return "", fmt.Errorf("invalid txid: %s", utxo.TxHash)
	}

	opReturn := []byte{0x00, 0x6a} // OP_FALSE OP_RETURN

	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint32(1))
	buf.WriteByte(0x01)
	buf.Write(txid)
	binary.Write(&buf, binary.LittleEndian, uint32(utxo.TxPos))
	buf.WriteByte(0x01)
	buf.WriteByte(0x51)
	binary.Write(&buf, binary.LittleEndian, uint32(0xFFFFFFFF))

	buf.WriteByte(0x01) // 1 output
	binary.Write(&buf, binary.LittleEndian, uint64(0))
	writeVarInt(&buf, uint64(len(opReturn)))
	buf.Write(opReturn)

	binary.Write(&buf, binary.LittleEndian, uint32(0))

	return hex.EncodeToString(buf.Bytes()), nil
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
