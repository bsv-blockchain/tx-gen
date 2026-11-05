package engine

import (
	"container/heap"
	"sync"

	"github.com/bsv-blockchain/tx-gen/internal/store"
	"github.com/bsv-blockchain/tx-gen/internal/txbuilder"
)

// unconfirmedHeight sorts unconfirmed UTXOs (height 0) after every
// confirmed one.
const unconfirmedHeight = 1 << 62

// queue is a min-heap of UTXOs, oldest confirmation first, with optional
// write-through to the store. It is safe for concurrent use.
//
// push and pop change only memory: chain tips stay in the store while their
// chain runs, and leave it only when CommitPending records the spend.
type queue struct {
	mu    sync.Mutex
	heap  utxoHeap
	store *store.Store
}

// newQueue returns an empty queue that persists to s, or only to memory
// when s is nil.
func newQueue(s *store.Store) *queue {
	return &queue{store: s}
}

// push adds u in memory only.
func (q *queue) push(u txbuilder.UTXO) {
	q.mu.Lock()
	heap.Push(&q.heap, u)
	q.mu.Unlock()
}

// pushPersisted adds u in memory and saves it to the store.
func (q *queue) pushPersisted(u txbuilder.UTXO) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	heap.Push(&q.heap, u)
	if q.store == nil {
		return nil
	}
	return q.store.SaveUTXO(u)
}

// pop removes and returns the oldest UTXO from memory.
func (q *queue) pop() (txbuilder.UTXO, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.heap) == 0 {
		return txbuilder.UTXO{}, false
	}
	return heap.Pop(&q.heap).(txbuilder.UTXO), true
}

// drain removes and returns every UTXO, oldest first.
func (q *queue) drain() []txbuilder.UTXO {
	q.mu.Lock()
	defer q.mu.Unlock()
	utxos := make([]txbuilder.UTXO, 0, len(q.heap))
	for len(q.heap) > 0 {
		utxos = append(utxos, heap.Pop(&q.heap).(txbuilder.UTXO))
	}
	return utxos
}

// len returns the number of UTXOs in memory.
func (q *queue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.heap)
}

// persistAll saves every UTXO in memory to the store.
func (q *queue) persistAll() error {
	q.mu.Lock()
	utxos := append([]txbuilder.UTXO(nil), q.heap...)
	q.mu.Unlock()
	if q.store == nil {
		return nil
	}
	for _, u := range utxos {
		if err := q.store.SaveUTXO(u); err != nil {
			return err
		}
	}
	return nil
}

// restore replaces the memory contents with the UTXOs in the store.
func (q *queue) restore() error {
	if q.store == nil {
		return nil
	}
	utxos, err := q.store.LoadAll()
	if err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.heap = q.heap[:0]
	for _, u := range utxos {
		heap.Push(&q.heap, u)
	}
	return nil
}

// commit records an accepted broadcast in the store: the pending record is
// cleared, spent UTXOs are removed, and created ones are saved.
func (q *queue) commit(txid string, spent, created []txbuilder.UTXO) error {
	if q.store == nil {
		return nil
	}
	return q.store.CommitPending(txid, spent, created)
}

// utxoHeap implements heap.Interface, ordering UTXOs by confirmation height.
type utxoHeap []txbuilder.UTXO

// Len implements sort.Interface.
func (h *utxoHeap) Len() int { return len(*h) }

// Less orders confirmed UTXOs by height, with unconfirmed ones last.
func (h *utxoHeap) Less(i, j int) bool {
	return sortHeight((*h)[i].Height) < sortHeight((*h)[j].Height)
}

// Swap implements sort.Interface.
func (h *utxoHeap) Swap(i, j int) { (*h)[i], (*h)[j] = (*h)[j], (*h)[i] }

// Push implements heap.Interface.
func (h *utxoHeap) Push(x any) { *h = append(*h, x.(txbuilder.UTXO)) }

// Pop implements heap.Interface.
func (h *utxoHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

// sortHeight maps an unconfirmed height (0) past every confirmed height.
func sortHeight(height int64) int64 {
	if height == 0 {
		return unconfirmedHeight
	}
	return height
}
