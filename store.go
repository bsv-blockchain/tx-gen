package main

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"go.etcd.io/bbolt"
	"os"
	"path/filepath"
	"time"
)

var (
	bucketUTXOs   = []byte("utxos")
	bucketMeta    = []byte("meta")
	bucketPending = []byte("pending")
)

type Store struct {
	db *bbolt.DB
}

type PendingBroadcast struct {
	TxID    string
	EF      []byte
	Spent   []UTXO
	Created []UTXO
	Stage   string
	SavedAt time.Time
}

func NewStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(bucketUTXOs); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketMeta); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketPending); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if closeErr := db.Close(); closeErr != nil {
			return nil, fmt.Errorf("%w; close db: %v", err, closeErr)
		}
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) SaveUTXO(utxo UTXO) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketUTXOs)
		key := []byte(utxoKey(utxo.TxHash, utxo.TxPos))
		encoded, err := encodeGob(utxo)
		if err != nil {
			return err
		}
		return b.Put(key, encoded)
	})
}

func (s *Store) DeleteUTXO(txid string, vout uint32) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketUTXOs)
		key := []byte(utxoKey(txid, vout))
		return b.Delete(key)
	})
}

func (s *Store) LoadAll() ([]UTXO, error) {
	var utxos []UTXO
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketUTXOs)
		return b.ForEach(func(k, v []byte) error {
			var u UTXO
			if err := gob.NewDecoder(bytes.NewReader(v)).Decode(&u); err != nil {
				return err
			}
			utxos = append(utxos, u)
			return nil
		})
	})
	return utxos, err
}

func (s *Store) GetMeta(key string) (string, error) {
	var val string
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		v := b.Get([]byte(key))
		if v != nil {
			val = string(v)
		}
		return nil
	})
	return val, err
}

func (s *Store) SetMeta(key, val string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		return b.Put([]byte(key), []byte(val))
	})
}

func (s *Store) SavePending(txid string, ef []byte) error {
	return s.SavePendingRecord(PendingBroadcast{TxID: txid, EF: ef})
}

func (s *Store) SavePendingRecord(p PendingBroadcast) error {
	if p.TxID == "" {
		return fmt.Errorf("pending txid is required")
	}
	if p.SavedAt.IsZero() {
		p.SavedAt = time.Now().UTC()
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketPending)
		encoded, err := encodeGob(p)
		if err != nil {
			return err
		}
		return b.Put([]byte(p.TxID), encoded)
	})
}

func (s *Store) ClearPending(txid string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketPending)
		return b.Delete([]byte(txid))
	})
}

func (s *Store) LoadPending() (map[string]PendingBroadcast, error) {
	pend := make(map[string]PendingBroadcast)
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketPending)
		return b.ForEach(func(k, v []byte) error {
			var p PendingBroadcast
			if err := gob.NewDecoder(bytes.NewReader(v)).Decode(&p); err != nil {
				p = PendingBroadcast{
					TxID: string(k),
					EF:   append([]byte(nil), v...),
				}
			}
			if p.TxID == "" {
				p.TxID = string(k)
			}
			pend[string(k)] = p
			return nil
		})
	})
	return pend, err
}

func (s *Store) CommitPending(txid string, spent, created []UTXO) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		utxos := tx.Bucket(bucketUTXOs)
		pending := tx.Bucket(bucketPending)
		if txid != "" {
			if err := pending.Delete([]byte(txid)); err != nil {
				return err
			}
		}
		for _, u := range spent {
			if err := utxos.Delete([]byte(utxoKey(u.TxHash, u.TxPos))); err != nil {
				return err
			}
		}
		for _, u := range created {
			if u.Value == 0 {
				continue
			}
			encoded, err := encodeGob(u)
			if err != nil {
				return err
			}
			if err := utxos.Put([]byte(utxoKey(u.TxHash, u.TxPos)), encoded); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) Close() error {
	return s.db.Close()
}

func utxoKey(txid string, vout uint32) string {
	return fmt.Sprintf("%s:%d", txid, vout)
}

func encodeGob(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
