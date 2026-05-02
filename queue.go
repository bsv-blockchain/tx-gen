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
	mu sync.Mutex
	h  utxoHeap
}

func newQueue() *Queue {
	q := &Queue{}
	heap.Init(&q.h)
	return q
}

func (q *Queue) Push(u UTXO) {
	q.mu.Lock()
	heap.Push(&q.h, u)
	q.mu.Unlock()
}

func (q *Queue) Pop() (UTXO, bool) {
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
