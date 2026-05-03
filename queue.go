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

// Restore loads all UTXOs from the store into the heap and enables write-through.
// Call at startup before enqueueing new items; a non-empty result skips WoC fetch.
func (q *Queue) Restore(store *Store) error {
	utxos, err := store.LoadAll()
	if err != nil {
		return err
	}
	q.mu.Lock()
	q.store = store
	for _, u := range utxos {
		heap.Push(&q.h, u)
	}
	q.mu.Unlock()
	return nil
}

func (q *Queue) Push(u UTXO) {
	q.mu.Lock()
	heap.Push(&q.h, u)
	if q.store != nil {
		_ = q.store.SaveUTXO(u)
	}
	q.mu.Unlock()
}

func (q *Queue) Pop() (UTXO, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.h) == 0 {
		return UTXO{}, false
	}
	u := heap.Pop(&q.h).(UTXO)
	if q.store != nil {
		_ = q.store.DeleteUTXO(u.TxHash, u.TxPos)
	}
	return u, true
}

func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.h)
}
