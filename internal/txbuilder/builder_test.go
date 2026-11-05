package txbuilder

import (
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	sdktx "github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// testTxID is a valid 64-character hex txid.
	testTxID = "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"

	// testP2PKHWIF is a throwaway testnet key used only by these tests.
	testP2PKHWIF = "cNGwGSc7KRrTmdLUZ54fiSXWbhLNDc2Eg5zNucgQxyQCzuQ5YRDq"

	// testInstanceID is exactly six bytes, so the tag needs no padding.
	testInstanceID = "inst01"
)

func taggedDropMode(t testing.TB) *Mode {
	t.Helper()
	mode, err := NewTaggedDropMode(testInstanceID, 0)
	require.NoError(t, err)
	return mode
}

func p2pkhMode(t testing.TB) *Mode {
	t.Helper()
	mode, err := NewP2PKHMode(testP2PKHWIF, 0)
	require.NoError(t, err)
	return mode
}

func TestBuildFanout(t *testing.T) {
	mode := taggedDropMode(t)
	tests := []struct {
		name    string
		value   uint64
		n       int
		wantErr error
	}{
		{name: "normal 4 outputs", value: 10_000, n: 4},
		{name: "minimal 2 outputs", value: 100, n: 2},
		{name: "cannot pay fee", value: 5, n: 2, wantErr: ErrInsufficientValue},
		{name: "zero per output", value: 9, n: 2, wantErr: ErrInsufficientValue},
		{name: "zero outputs", value: 10_000, n: 0, wantErr: ErrInvalidFanoutCount},
		{name: "negative outputs", value: 10_000, n: -1, wantErr: ErrInvalidFanoutCount},
		{name: "too many outputs", value: 10_000, n: int(MaxTxBytes) + 1, wantErr: ErrTxTooLarge},
		{name: "too large serialized", value: 10_000, n: int(MaxTxBytes / 10), wantErr: ErrTxTooLarge},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			utxo := UTXO{TxHash: testTxID, TxPos: 0, Value: tc.value}
			tx, outputs, err := BuildFanout(utxo, tc.n, mode)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Len(t, tx.TxID, 64)
			assert.NotEmpty(t, tx.EF)
			require.Len(t, outputs, tc.n)

			var total uint64
			for i, out := range outputs {
				assert.Equal(t, tx.TxID, out.TxHash, "output %d txid", i)
				assert.EqualValues(t, i, out.TxPos, "output %d vout", i)
				assert.Equal(t, outputs[0].Value, out.Value, "outputs must be equal")
				total += out.Value
			}
			assert.Less(t, total, tc.value, "no fee deducted")
		})
	}
}

func TestBuildFanoutFeeAndValueSplit(t *testing.T) {
	mode := taggedDropMode(t)
	// version and lock time (8) + input count (1) + one OP_TRUE input (32 + 4 + 1 + 1 + 4 = 42)
	// + output count (1) + 3 outputs of 8 + 1 + 8 = 17 bytes: 103 bytes -> ceil(103 * 100 / 1000) = 11 sats.
	const value = 1_000
	tx, outputs, err := BuildFanout(UTXO{TxHash: testTxID, Value: value}, 3, mode)
	require.NoError(t, err)
	require.Len(t, outputs, 3)
	assert.Equal(t, uint64((value-11)/3), outputs[0].Value)

	parsed, err := sdktx.NewTransactionFromBytes(tx.EF)
	require.NoError(t, err)
	assert.Equal(t, tx.TxID, parsed.TxID().String())
	assert.Len(t, parsed.Outputs, 3)
	assert.Len(t, parsed.Bytes(), 103)
}

func TestBuildFanoutInvalidTxID(t *testing.T) {
	_, _, err := BuildFanout(UTXO{TxHash: "not-a-txid", Value: 10_000}, 2, taggedDropMode(t))
	require.ErrorIs(t, err, ErrInvalidTxID)
}

func TestBuildFanoutNilMode(t *testing.T) {
	_, _, err := BuildFanout(UTXO{TxHash: testTxID, Value: 10_000}, 2, nil)
	require.ErrorIs(t, err, ErrNilMode)
}

func TestBuildSustain(t *testing.T) {
	mode := taggedDropMode(t)
	tests := []struct {
		name      string
		value     uint64
		wantErr   error
		wantValue uint64
	}{
		{name: "normal", value: 100, wantValue: 100 - DefaultSustainFee},
		{name: "chain end at fee", value: DefaultSustainFee, wantValue: 0},
		{name: "chain end below fee", value: DefaultSustainFee - 1, wantValue: 0},
		{name: "zero value", value: 0, wantErr: ErrZeroValueUTXO},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			utxo := UTXO{TxHash: testTxID, TxPos: 0, Value: tc.value}
			tx, next, err := BuildSustain(utxo, mode)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Len(t, tx.TxID, 64)
			assert.NotEmpty(t, tx.EF)
			assert.Equal(t, tc.wantValue, next.Value)
			assert.Equal(t, tx.TxID, next.TxHash)
			assert.Equal(t, uint32(0), next.TxPos)
		})
	}
}

func TestBuildSustainInvalidTxID(t *testing.T) {
	_, _, err := BuildSustain(UTXO{TxHash: "not-a-txid", Value: 100}, taggedDropMode(t))
	require.ErrorIs(t, err, ErrInvalidTxID)
}

func TestBuildSustainNilMode(t *testing.T) {
	_, _, err := BuildSustain(UTXO{TxHash: testTxID, Value: 100}, nil)
	require.ErrorIs(t, err, ErrNilMode)
}

func TestBuildSustainP2PKHSignsInput(t *testing.T) {
	mode := p2pkhMode(t)
	tx, next, err := BuildSustain(UTXO{TxHash: testTxID, TxPos: 0, Value: 100}, mode)
	require.NoError(t, err)
	assert.Equal(t, 100-DefaultP2PKHSustainFee, next.Value)

	parsed, err := sdktx.NewTransactionFromBytes(tx.EF)
	require.NoError(t, err)
	assert.Equal(t, tx.TxID, parsed.TxID().String())
	require.Len(t, parsed.Inputs, 1)
	unlock := parsed.Inputs[0].UnlockingScript
	require.NotNil(t, unlock, "P2PKH input was not signed")
	assert.NotEmpty(t, *unlock, "P2PKH input was not signed")
	require.Len(t, parsed.Outputs, 1)
	assert.True(t, parsed.Outputs[0].LockingScript.IsP2PKH(), "output locking script is not P2PKH")
}

func TestBuildFanoutP2PKHSignsInput(t *testing.T) {
	tx, outputs, err := BuildFanout(UTXO{TxHash: testTxID, Value: 10_000}, 4, p2pkhMode(t))
	require.NoError(t, err)
	require.Len(t, outputs, 4)

	parsed, err := sdktx.NewTransactionFromBytes(tx.EF)
	require.NoError(t, err)
	require.Len(t, parsed.Inputs, 1)
	require.NotNil(t, parsed.Inputs[0].UnlockingScript)
	assert.NotEmpty(t, *parsed.Inputs[0].UnlockingScript)
}

func TestNewModeDefaultsToTaggedDrop(t *testing.T) {
	mode, err := NewMode("", testInstanceID, 0)
	require.NoError(t, err)
	assert.Equal(t, ModeTaggedDrop, mode.Name)
	assert.Equal(t, DefaultSustainFee, mode.SustainFee)
	assert.Nil(t, mode.Unlocker, "tagged-drop mode must not have an unlocking template")
	assert.Empty(t, mode.Address)
	assert.Equal(t, taggedDropMode(t).ScriptHash(), mode.ScriptHash())
}

func TestNewModeUsesP2PKHWhenPrivateKeyIsSet(t *testing.T) {
	mode, err := NewMode(testP2PKHWIF, testInstanceID, 0)
	require.NoError(t, err)
	assert.Equal(t, ModeP2PKH, mode.Name)
	assert.Equal(t, DefaultP2PKHSustainFee, mode.SustainFee)
	assert.NotNil(t, mode.Unlocker)
	assert.NotEmpty(t, mode.Address)
	assert.True(t, mode.LockScript.IsP2PKH())
	assert.NotEqual(t, taggedDropMode(t).ScriptHash(), mode.ScriptHash())
}

func TestNewModeHonorsSustainFee(t *testing.T) {
	tagged, err := NewMode("", testInstanceID, 11)
	require.NoError(t, err)
	assert.Equal(t, uint64(11), tagged.SustainFee)

	signed, err := NewMode(testP2PKHWIF, "", 33)
	require.NoError(t, err)
	assert.Equal(t, uint64(33), signed.SustainFee)
}

func TestNewModeRejectsMissingInstanceID(t *testing.T) {
	_, err := NewMode("", "  ", 0)
	require.ErrorIs(t, err, ErrInstanceIDRequired)
	assert.Contains(t, err.Error(), "INSTANCE_ID")
}

func TestNewP2PKHModeRejectsBadKeys(t *testing.T) {
	_, err := NewP2PKHMode("not-a-private-key", 0)
	require.ErrorIs(t, err, ErrInvalidPrivateKey)
	assert.Contains(t, err.Error(), "PRIVATE_KEY")

	_, err = NewP2PKHMode("   ", 0)
	require.ErrorIs(t, err, ErrPrivateKeyRequired)
}

// TestNewP2PKHModeAcceptsHexKey verifies the 32-byte hex form of a key locks
// to the same address as its WIF. The hex is derived at runtime, so no raw
// key material is committed.
func TestNewP2PKHModeAcceptsHexKey(t *testing.T) {
	wifMode := p2pkhMode(t)
	priv, err := ec.PrivateKeyFromWif(testP2PKHWIF)
	require.NoError(t, err)

	hexMode, err := NewP2PKHMode(priv.Hex(), 0)
	require.NoError(t, err)
	assert.True(t, hexMode.LockScript.IsP2PKH())
	assert.Equal(t, wifMode.Address, hexMode.Address)
	assert.Equal(t, wifMode.ScriptHash(), hexMode.ScriptHash())
}

func TestWithSustainFeeCopiesMode(t *testing.T) {
	mode := taggedDropMode(t)
	changed := mode.WithSustainFee(42)
	assert.Equal(t, uint64(42), changed.SustainFee)
	assert.Equal(t, DefaultSustainFee, mode.SustainFee, "original must not change")
	assert.Equal(t, mode.LockScript, changed.LockScript)
}

func TestTaggedDropLockScriptNormalizesInstanceID(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		want       []byte
	}{
		{name: "short is padded", instanceID: "abc", want: []byte{0x06, 'a', 'b', 'c', ' ', ' ', ' ', script.OpDROP}},
		{name: "long is truncated", instanceID: "abcdefghi", want: []byte{0x06, 'a', 'b', 'c', 'd', 'e', 'f', script.OpDROP}},
		{name: "whitespace is trimmed", instanceID: "  ab  ", want: []byte{0x06, 'a', 'b', ' ', ' ', ' ', ' ', script.OpDROP}},
		{
			name:       "multi-byte runes are never split",
			instanceID: "éééé",
			want:       []byte{0x06, 0xc3, 0xa9, 0xc3, 0xa9, 0xc3, 0xa9, script.OpDROP},
		},
		{
			name:       "a rune that does not fit leaves padding",
			instanceID: "abcdeé",
			want:       []byte{0x06, 'a', 'b', 'c', 'd', 'e', ' ', script.OpDROP},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TaggedDropLockScript(tc.instanceID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, []byte(*got))
		})
	}
}

func TestScriptHashIsReversedSHA256(t *testing.T) {
	s := script.Script{script.OpTRUE}
	// sha256(0x51) = 4ae81572f06e1b88fd5ced7a1a000945432e83e1551e6f721ee9c00b8cc33260
	assert.Equal(t, "6032c38c0bc0e91e726f1e55e1832e434509001a7aed5cfd881b6ef07215e84a", ScriptHash(&s))
}

func TestVarIntSize(t *testing.T) {
	tests := []struct {
		n    uint64
		want uint64
	}{
		{0, 1}, {0xfc, 1}, {0xfd, 3}, {0xffff, 3}, {0x10000, 5}, {0xffffffff, 5}, {0x100000000, 9},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, varIntSize(tc.n), "varIntSize(%d)", tc.n)
	}
}
