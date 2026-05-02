package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

const feePerTx = 100 // satoshis; ~66-byte tx at 1 sat/byte with headroom

// buildLockScript returns raw bytes for "acade2 OP_DROP".
// Script: 03 AC AD E2 75
//   03     = push 3 bytes
//   AC AD E2 = data
//   75     = OP_DROP
func buildLockScript() []byte {
	return []byte{0x03, 0xAC, 0xAD, 0xE2, 0x75}
}

// scriptToHash returns SHA256(script) reversed (little-endian), as WhatOnChain expects.
func scriptToHash(script []byte) string {
	h := sha256.Sum256(script)
	for i, j := 0, len(h)-1; i < j; i, j = i+1, j-1 {
		h[i], h[j] = h[j], h[i]
	}
	return hex.EncodeToString(h[:])
}

// buildTx constructs a raw BSV transaction spending utxo with OP_TRUE, sending
// change back to lockScript. Returns hex-encoded raw tx and the new UTXO (TxHash unset).
func buildTx(utxo UTXO, lockScript []byte) (string, UTXO, error) {
	if utxo.Value <= feePerTx {
		return "", UTXO{}, fmt.Errorf("value %d <= fee %d", utxo.Value, feePerTx)
	}
	outValue := utxo.Value - uint64(feePerTx)

	txidBytes, err := hex.DecodeString(utxo.TxHash)
	if err != nil {
		return "", UTXO{}, fmt.Errorf("decode txid: %w", err)
	}
	// txid is stored little-endian in raw tx
	reversed := make([]byte, len(txidBytes))
	copy(reversed, txidBytes)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}

	var buf bytes.Buffer

	binary.Write(&buf, binary.LittleEndian, uint32(1)) // version
	buf.WriteByte(0x01)                                // input count
	buf.Write(reversed)                                // prev txid
	binary.Write(&buf, binary.LittleEndian, uint32(utxo.TxPos))
	buf.WriteByte(0x01) // unlocking script length
	buf.WriteByte(0x51) // OP_TRUE (OP_1)
	binary.Write(&buf, binary.LittleEndian, uint32(0xFFFFFFFF)) // sequence

	buf.WriteByte(0x01) // output count
	binary.Write(&buf, binary.LittleEndian, uint64(outValue))
	writeVarInt(&buf, uint64(len(lockScript)))
	buf.Write(lockScript)

	binary.Write(&buf, binary.LittleEndian, uint32(0)) // locktime

	newUTXO := UTXO{
		TxPos:  0,
		Value:  outValue,
		Height: 0, // unconfirmed until mined
	}
	return hex.EncodeToString(buf.Bytes()), newUTXO, nil
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
