package main

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"go.etcd.io/bbolt"
	"os"
	"path/filepath"
)

var (
	bucketUTXOs   = []byte("utxos")
	bucketMeta    = []byte("meta")
	bucketPending = []byte("pending")
)

type Store struct {
	db *bbolt.DB
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
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) SaveUTXO(utxo UTXO) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketUTXOs)
		key := []byte(utxoKey(utxo.TxHash, utxo.TxPos))
		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(utxo); err != nil {
			return err
		}
		return b.Put(key, buf.Bytes())
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
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketPending)
		return b.Put([]byte(txid), ef)
	})
}

func (s *Store) ClearPending(txid string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketPending)
		return b.Delete([]byte(txid))
	})
}

func (s *Store) Close() error {
	return s.db.Close()
}

func utxoKey(txid string, vout uint32) string {
	return fmt.Sprintf("%s:%d", txid, vout)
}
