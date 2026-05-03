package main

import (
	"container/heap"
	"sync"
)

type UTXO struct {
	TxHash string
	TxPos  uint32
	Value  uint64
	Height int64
}

type utxoHeap []UTXO

func (h utxoHeap) Len() int { return len(h) }
func (h utxoHeap) Less(i, j int) bool {
	// oldest confirmed first; unconfirmed (height==0) go last
	hi, hj := h[i].Height, h[j].Height
	if hi == 0 {
		hi = 1 << 62
	}
	if hj == 0 {
		hj = 1 << 62
	}
	return hi < hj
}
func (h utxoHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *utxoHeap) Push(x interface{}) { *h = append(*h, x.(UTXO)) }
func (h *utxoHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

type Queue struct {
	mu    sync.Mutex
	h     utxoHeap
	store *Store
}

func newQueue() *Queue {
	q := &Queue{}
	heap.Init(&q.h)
	return q
}

// SetStore wires a store for write-through persistence without loading existing data.
func (q *Queue) SetStore(s *Store) {
	q.mu.Lock()
	q.store = s
	q.mu.Unlock()
}

func (q *Queue) PersistAll() error {
	q.mu.Lock()
	store := q.store
	utxos := append([]UTXO(nil), q.h...)
	q.mu.Unlock()
	if store == nil {
		return nil
	}
	for _, u := range utxos {
		if err := store.SaveUTXO(u); err != nil {
			return err
		}
	}
	return nil
}

// Restore loads all UTXOs from the store into the heap and enables write-through.
// Call at startup before enqueueing new items; a non-empty result skips WoC fetch.
func (q *Queue) Restore(store *Store) error {
	utxos, err := store.LoadAll()
	if err != nil {
		return err
	}
	q.mu.Lock()
	q.h = q.h[:0]
	q.store = store
	for _, u := range utxos {
		heap.Push(&q.h, u)
	}
	q.mu.Unlock()
	return nil
}

func (q *Queue) Push(u UTXO) {
	_ = q.PushPersisted(u)
}

func (q *Queue) PushMemory(u UTXO) {
	q.mu.Lock()
	heap.Push(&q.h, u)
	q.mu.Unlock()
}

func (q *Queue) Pop() (UTXO, bool) {
	u, ok, _ := q.PopPersisted()
	return u, ok
}

func (q *Queue) PopMemory() (UTXO, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.h) == 0 {
		return UTXO{}, false
	}
	return heap.Pop(&q.h).(UTXO), true
}

func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.h)
}

func (q *Queue) PushPersisted(u UTXO) error {
	q.mu.Lock()
	heap.Push(&q.h, u)
	var err error
	if q.store != nil {
		err = q.store.SaveUTXO(u)
	}
	q.mu.Unlock()
	return err
}

func (q *Queue) PopPersisted() (UTXO, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.h) == 0 {
		return UTXO{}, false, nil
	}
	u := heap.Pop(&q.h).(UTXO)
	var err error
	if q.store != nil {
		err = q.store.DeleteUTXO(u.TxHash, u.TxPos)
	}
	return u, true, err
}

func (q *Queue) UpdateActiveTip(old, new UTXO) error {
	q.mu.Lock()
	store := q.store
	q.mu.Unlock()
	if store == nil {
		return nil
	}
	return store.CommitPending("", []UTXO{old}, []UTXO{new})
}

func (q *Queue) CommitPending(txid string, spent, created []UTXO) error {
	q.mu.Lock()
	store := q.store
	q.mu.Unlock()
	if store == nil {
		return nil
	}
	return store.CommitPending(txid, spent, created)
}
