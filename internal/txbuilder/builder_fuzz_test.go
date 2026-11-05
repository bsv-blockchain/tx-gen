package txbuilder

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FuzzBuildSustain checks that any successfully built sustain hop has a
// non-empty EF encoding and never creates value.
func FuzzBuildSustain(f *testing.F) {
	mode := taggedDropMode(f)
	f.Add(testTxID, uint32(0), uint64(1000))
	f.Add(testTxID, uint32(0), DefaultSustainFee)
	f.Add(testTxID, uint32(7), uint64(1))
	f.Add("not-a-txid", uint32(0), uint64(1000))

	f.Fuzz(func(t *testing.T, txid string, pos uint32, value uint64) {
		tx, next, err := BuildSustain(UTXO{TxHash: txid, TxPos: pos, Value: value}, mode)
		if err != nil {
			return
		}
		assert.NotEmpty(t, tx.EF, "EF bytes empty on success")
		assert.LessOrEqual(t, next.Value, value, "sustain hop created value")
		assert.Equal(t, tx.TxID, next.TxHash)
	})
}

// FuzzBuildFanout checks that any successful fanout splits value without
// creating any, into exactly n outputs.
func FuzzBuildFanout(f *testing.F) {
	mode := taggedDropMode(f)
	f.Add(uint64(10_000), 4)
	f.Add(uint64(100), 2)
	f.Add(uint64(5), 2)
	f.Add(uint64(1<<40), 300)

	f.Fuzz(func(t *testing.T, value uint64, n int) {
		if n > 5_000 { // keep each iteration fast
			return
		}
		_, outputs, err := BuildFanout(UTXO{TxHash: testTxID, Value: value}, n, mode)
		if err != nil {
			return
		}
		require.Len(t, outputs, n)
		var total uint64
		for _, out := range outputs {
			total += out.Value
		}
		assert.Less(t, total, value, "fanout must pay a fee")
	})
}

// FuzzTaggedDropLockScript checks that every non-blank instance id yields a
// well-formed eight-byte push/drop script.
func FuzzTaggedDropLockScript(f *testing.F) {
	f.Add("inst01")
	f.Add("éééé")
	f.Add("a")
	f.Add("  padded  ")

	f.Fuzz(func(t *testing.T, instanceID string) {
		s, err := TaggedDropLockScript(instanceID)
		if err != nil {
			require.ErrorIs(t, err, ErrInstanceIDRequired)
			return
		}
		require.Len(t, *s, lockScriptTagBytes+2)
		assert.Equal(t, byte(lockScriptTagBytes), (*s)[0])
	})
}
