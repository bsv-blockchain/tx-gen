package store

import (
	"bytes"
	"encoding/gob"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"

	"github.com/bsv-blockchain/tx-gen/internal/txbuilder"
)

const (
	txA = "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"
	txB = "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "nested", "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func TestOpenCreatesDirectoryAndFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "state.db")
	s, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(fileMode), info.Mode().Perm())
}

func TestOpenFailsWhenDirectoryCannotBeCreated(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	_, err := Open(filepath.Join(blocker, "state.db"))
	require.Error(t, err)
}

func TestUTXORoundTrip(t *testing.T) {
	s := openTestStore(t)
	a := txbuilder.UTXO{TxHash: txA, TxPos: 0, Value: 1_000, Height: 800_000}
	b := txbuilder.UTXO{TxHash: txB, TxPos: 3, Value: 2_000}
	require.NoError(t, s.SaveUTXO(a))
	require.NoError(t, s.SaveUTXO(b))
	require.NoError(t, s.SaveUTXO(a), "saving twice replaces")

	utxos, err := s.LoadAll()
	require.NoError(t, err)
	assert.ElementsMatch(t, []txbuilder.UTXO{a, b}, utxos)

	require.NoError(t, s.DeleteUTXO(txA, 0))
	require.NoError(t, s.DeleteUTXO(txA, 99), "deleting a missing UTXO is not an error")
	utxos, err = s.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, []txbuilder.UTXO{b}, utxos)
}

func TestMeta(t *testing.T) {
	s := openTestStore(t)
	value, err := s.GetMeta(MetaBootstrapStage)
	require.NoError(t, err)
	assert.Empty(t, value)

	require.NoError(t, s.SetMeta(MetaBootstrapStage, "l2_done"))
	value, err = s.GetMeta(MetaBootstrapStage)
	require.NoError(t, err)
	assert.Equal(t, "l2_done", value)
}

func TestPendingRoundTrip(t *testing.T) {
	s := openTestStore(t)
	require.ErrorIs(t, s.SavePending(PendingBroadcast{}), ErrPendingTxIDRequired)

	record := PendingBroadcast{
		TxID:    txB,
		EF:      []byte{1, 2, 3},
		Spent:   []txbuilder.UTXO{{TxHash: txA, Value: 1_000}},
		Created: []txbuilder.UTXO{{TxHash: txB, Value: 993}},
		Stage:   "l2_done",
	}
	require.NoError(t, s.SavePending(record))

	pending, err := s.LoadPending()
	require.NoError(t, err)
	require.Contains(t, pending, txB)
	got := pending[txB]
	assert.False(t, got.SavedAt.IsZero(), "SavedAt defaults to now")
	got.SavedAt = time.Time{}
	assert.Equal(t, record, got)

	require.NoError(t, s.ClearPending(txB))
	pending, err = s.LoadPending()
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestLoadPendingKeepsUndecodableRecords(t *testing.T) {
	s := openTestStore(t)
	require.NoError(t, s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketPending).Put([]byte(txA), []byte("raw-ef-bytes"))
	}))

	pending, err := s.LoadPending()
	require.NoError(t, err)
	require.Contains(t, pending, txA)
	assert.Equal(t, txA, pending[txA].TxID)
	assert.Equal(t, []byte("raw-ef-bytes"), pending[txA].EF)
	assert.Empty(t, pending[txA].Spent)
}

func TestCommitPending(t *testing.T) {
	s := openTestStore(t)
	oldTip := txbuilder.UTXO{TxHash: txA, TxPos: 0, Value: 1_000}
	newTip := txbuilder.UTXO{TxHash: txB, TxPos: 0, Value: 993}
	exhausted := txbuilder.UTXO{TxHash: txB, TxPos: 1, Value: 0}
	require.NoError(t, s.SaveUTXO(oldTip))
	require.NoError(t, s.SavePending(PendingBroadcast{TxID: txB, EF: []byte{1}}))

	require.NoError(t, s.CommitPending(txB, []txbuilder.UTXO{oldTip}, []txbuilder.UTXO{newTip, exhausted}))

	utxos, err := s.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, []txbuilder.UTXO{newTip}, utxos, "zero-value outputs are not stored")
	pending, err := s.LoadPending()
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestCommitPendingWithoutTxID(t *testing.T) {
	s := openTestStore(t)
	newTip := txbuilder.UTXO{TxHash: txB, Value: 5}
	require.NoError(t, s.CommitPending("", nil, []txbuilder.UTXO{newTip}))
	utxos, err := s.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, []txbuilder.UTXO{newTip}, utxos)
}

func TestLoadAllReportsCorruptRecords(t *testing.T) {
	s := openTestStore(t)
	require.NoError(t, s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketUTXOs).Put([]byte("bad:0"), []byte("not gob"))
	}))
	_, err := s.LoadAll()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad:0")
}

// legacyUTXO mirrors the UTXO type of the original single-package build.
// gob matches struct fields by name, not by type, so its records must
// decode into txbuilder.UTXO.
type legacyUTXO struct {
	TxHash string
	TxPos  uint32
	Value  uint64
	Height int64
}

func TestLegacyRecordsDecode(t *testing.T) {
	s := openTestStore(t)
	var buf bytes.Buffer
	require.NoError(t, gob.NewEncoder(&buf).Encode(legacyUTXO{TxHash: txA, TxPos: 2, Value: 700, Height: 9}))
	require.NoError(t, s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketUTXOs).Put(utxoKey(txA, 2), buf.Bytes())
	}))

	utxos, err := s.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, []txbuilder.UTXO{{TxHash: txA, TxPos: 2, Value: 700, Height: 9}}, utxos)
}

func TestUTXOKey(t *testing.T) {
	assert.Equal(t, []byte(txA+":0"), utxoKey(txA, 0))
	assert.Equal(t, []byte(txA+":4294967295"), utxoKey(txA, 4294967295))
}
