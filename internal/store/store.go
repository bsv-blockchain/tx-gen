// Package store persists tx-gen state in a BoltDB file, so that a restart
// resumes every chain from its last accepted tip instead of bootstrapping
// again.
//
// The file has three buckets:
//
//   - utxos: the spendable chain tips, keyed by "txid:vout".
//   - meta: small string settings such as the bootstrap stage and the
//     lock-script hash that the state belongs to.
//   - pending: broadcasts written before they are sent, so that a crash
//     mid-broadcast can be replayed on startup.
//
// Values are gob encoded. The field names of txbuilder.UTXO and
// PendingBroadcast are therefore part of the on-disk format.
package store

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"go.etcd.io/bbolt"

	"github.com/bsv-blockchain/tx-gen/internal/txbuilder"
)

// Meta keys written by tx-gen.
const (
	// MetaBootstrapStage records how far bootstrap has progressed.
	MetaBootstrapStage = "bootstrap_stage"

	// MetaLockScriptHash records the script hash the state belongs to.
	MetaLockScriptHash = "lock_script_hash"

	// MetaTxMode records the transaction mode name the state was used with.
	MetaTxMode = "tx_mode"
)

const (
	// dirMode is the permission of a state directory that Open creates.
	dirMode = 0o750

	// fileMode is the permission of the state file.
	fileMode = 0o600

	// openTimeout bounds the wait for another process's file lock.
	openTimeout = 5 * time.Second
)

var (
	bucketUTXOs   = []byte("utxos")
	bucketMeta    = []byte("meta")
	bucketPending = []byte("pending")
)

// ErrPendingTxIDRequired is returned when a pending broadcast has no txid.
var ErrPendingTxIDRequired = errors.New("pending broadcast txid is required")

// Store is a BoltDB-backed state store. It is safe for concurrent use.
type Store struct {
	db *bbolt.DB
}

// PendingBroadcast is a transaction written to the store before it is
// broadcast, together with the state transition that applies once Arcade
// accepts it.
type PendingBroadcast struct {
	// TxID is the transaction id; it is also the record key.
	TxID string

	// EF is the EF-encoded transaction.
	EF []byte

	// Spent lists the UTXOs the transaction consumes.
	Spent []txbuilder.UTXO

	// Created lists the UTXOs the transaction creates.
	Created []txbuilder.UTXO

	// Stage is the bootstrap stage reached once the transaction is accepted.
	Stage string

	// SavedAt is when the record was written.
	SavedAt time.Time
}

// Open opens (creating if needed) the state file at path, along with its
// parent directory.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	db, err := bbolt.Open(path, fileMode, &bbolt.Options{Timeout: openTimeout})
	if err != nil {
		return nil, fmt.Errorf("open state file %s: %w", path, err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketUTXOs, bucketMeta, bucketPending} {
			if _, bucketErr := tx.CreateBucketIfNotExists(name); bucketErr != nil {
				return fmt.Errorf("create bucket %s: %w", name, bucketErr)
			}
		}
		return nil
	})
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &Store{db: db}, nil
}

// Close closes the state file.
func (s *Store) Close() error {
	return s.db.Close()
}

// SaveUTXO stores u, replacing any UTXO with the same outpoint.
func (s *Store) SaveUTXO(u txbuilder.UTXO) error {
	encoded, err := encodeGob(u)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketUTXOs).Put(utxoKey(u.TxHash, u.TxPos), encoded)
	})
}

// DeleteUTXO removes the UTXO at txid:vout, if present.
func (s *Store) DeleteUTXO(txid string, vout uint32) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketUTXOs).Delete(utxoKey(txid, vout))
	})
}

// LoadAll returns every stored UTXO.
func (s *Store) LoadAll() ([]txbuilder.UTXO, error) {
	var utxos []txbuilder.UTXO
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketUTXOs).ForEach(func(key, value []byte) error {
			var u txbuilder.UTXO
			if err := gob.NewDecoder(bytes.NewReader(value)).Decode(&u); err != nil {
				return fmt.Errorf("decode UTXO %s: %w", key, err)
			}
			utxos = append(utxos, u)
			return nil
		})
	})
	return utxos, err
}

// GetMeta returns the meta value for key, or "" when it is not set.
func (s *Store) GetMeta(key string) (string, error) {
	var value string
	err := s.db.View(func(tx *bbolt.Tx) error {
		if v := tx.Bucket(bucketMeta).Get([]byte(key)); v != nil {
			value = string(v)
		}
		return nil
	})
	return value, err
}

// SetMeta stores value under key.
func (s *Store) SetMeta(key, value string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Put([]byte(key), []byte(value))
	})
}

// SavePending stores p before it is broadcast. SavedAt defaults to now.
func (s *Store) SavePending(p PendingBroadcast) error {
	if p.TxID == "" {
		return ErrPendingTxIDRequired
	}
	if p.SavedAt.IsZero() {
		p.SavedAt = time.Now().UTC()
	}
	encoded, err := encodeGob(p)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketPending).Put([]byte(p.TxID), encoded)
	})
}

// ClearPending removes the pending record for txid, if present.
func (s *Store) ClearPending(txid string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketPending).Delete([]byte(txid))
	})
}

// LoadPending returns every pending broadcast, keyed by txid.
//
// A record that does not decode is returned with only TxID and EF set (the
// raw bytes), so that it is surfaced for reconciliation rather than lost.
func (s *Store) LoadPending() (map[string]PendingBroadcast, error) {
	pending := make(map[string]PendingBroadcast)
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketPending).ForEach(func(key, value []byte) error {
			var p PendingBroadcast
			if err := gob.NewDecoder(bytes.NewReader(value)).Decode(&p); err != nil {
				p = PendingBroadcast{TxID: string(key), EF: bytes.Clone(value)}
			}
			if p.TxID == "" {
				p.TxID = string(key)
			}
			pending[string(key)] = p
			return nil
		})
	})
	return pending, err
}

// CommitPending atomically applies an accepted broadcast: it removes the
// pending record for txid (when txid is not empty), deletes the spent UTXOs,
// and stores the created ones. Zero-value outputs end their chain and are not
// stored.
func (s *Store) CommitPending(txid string, spent, created []txbuilder.UTXO) error {
	encoded := make([][]byte, len(created))
	for i, u := range created {
		if u.Value == 0 {
			continue
		}
		var err error
		if encoded[i], err = encodeGob(u); err != nil {
			return err
		}
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		utxos := tx.Bucket(bucketUTXOs)
		if txid != "" {
			if err := tx.Bucket(bucketPending).Delete([]byte(txid)); err != nil {
				return err
			}
		}
		for _, u := range spent {
			if err := utxos.Delete(utxoKey(u.TxHash, u.TxPos)); err != nil {
				return err
			}
		}
		for i, u := range created {
			if encoded[i] == nil {
				continue
			}
			if err := utxos.Put(utxoKey(u.TxHash, u.TxPos), encoded[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// utxoKey returns the bucket key "txid:vout".
func utxoKey(txid string, vout uint32) []byte {
	key := make([]byte, 0, len(txid)+11)
	key = append(key, txid...)
	key = append(key, ':')
	return strconv.AppendUint(key, uint64(vout), 10)
}

// encodeGob gob-encodes v.
func encodeGob(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, fmt.Errorf("encode %T: %w", v, err)
	}
	return buf.Bytes(), nil
}
