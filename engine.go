package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

const (
	numChains  = 10_000
	fanoutSize = 100
)

// notify is a broadcast channel: Broadcast() wakes all current C() waiters.
type notify struct {
	mu sync.Mutex
	ch chan struct{}
}

func newNotify() *notify { return &notify{ch: make(chan struct{})} }

func (n *notify) C() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *notify) Broadcast() {
	n.mu.Lock()
	old := n.ch
	n.ch = make(chan struct{})
	n.mu.Unlock()
	close(old)
}

type Engine struct {
	queue      *Queue
	woc        *wocClient
	lockScript []byte
	tps        atomic.Int64
	resume     *notify // broadcast when TPS goes from 0 → >0
}

func newEngine(q *Queue, woc *wocClient, lockScript []byte) *Engine {
	return &Engine{
		queue:      q,
		woc:        woc,
		lockScript: lockScript,
		resume:     newNotify(),
	}
}

func (e *Engine) SetTPS(tps int64) {
	e.tps.Store(tps)
	if tps > 0 {
		e.resume.Broadcast()
	}
}

func (e *Engine) TPS() int64 { return e.tps.Load() }

// run is the main engine goroutine. It bootstraps if needed then launches chains.
func (e *Engine) run(ctx context.Context) {
	qlen := e.queue.Len()
	log.Printf("startup: %d UTXOs", qlen)

	var err error
	switch {
	case qlen == 1:
		log.Println("bootstrap: L1 + L2 fanout (1 → 100 → 10,000)")
		err = e.bootstrapFull(ctx)
	case qlen == fanoutSize:
		log.Println("bootstrap: L2 fanout only (100 → 10,000)")
		err = e.bootstrapL2(ctx)
	default:
		log.Printf("resuming with %d existing UTXOs", qlen)
	}

	if err != nil {
		log.Printf("bootstrap error: %v", err)
		return
	}

	e.startChains(ctx)
}

func (e *Engine) bootstrapFull(ctx context.Context) error {
	utxo, ok := e.queue.Pop()
	if !ok {
		return fmt.Errorf("queue empty")
	}

	rawHex, l1Outputs, err := buildFanoutTx(utxo, fanoutSize, e.lockScript)
	if err != nil {
		e.queue.Push(utxo)
		return fmt.Errorf("build L1: %w", err)
	}
	txid, err := e.woc.broadcast(rawHex)
	if err != nil {
		e.queue.Push(utxo)
		return fmt.Errorf("broadcast L1: %w", err)
	}
	for i := range l1Outputs {
		l1Outputs[i].TxHash = txid
	}
	log.Printf("L1 tx: %s (100 outputs @ %d sats each)", txid, l1Outputs[0].Value)

	for _, u := range l1Outputs {
		e.queue.Push(u)
	}
	return e.bootstrapL2(ctx)
}

func (e *Engine) bootstrapL2(ctx context.Context) error {
	var parents []UTXO
	for {
		u, ok := e.queue.Pop()
		if !ok {
			break
		}
		parents = append(parents, u)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, p := range parents {
		wg.Add(1)
		go func(parent UTXO) {
			defer wg.Done()
			rawHex, outputs, err := buildFanoutTx(parent, fanoutSize, e.lockScript)
			if err != nil {
				log.Printf("L2 build %s: %v", parent.TxHash, err)
				return
			}
			txid, err := e.woc.broadcast(rawHex)
			if err != nil {
				log.Printf("L2 broadcast %s: %v", parent.TxHash, err)
				return
			}
			for i := range outputs {
				outputs[i].TxHash = txid
			}
			mu.Lock()
			for _, out := range outputs {
				e.queue.Push(out)
			}
			mu.Unlock()
		}(p)
	}

	wg.Wait()
	log.Printf("bootstrap complete: %d UTXOs ready", e.queue.Len())
	return nil
}

func (e *Engine) startChains(ctx context.Context) {
	count := 0
	for {
		utxo, ok := e.queue.Pop()
		if !ok {
			break
		}
		go e.runChain(ctx, utxo)
		count++
	}
	log.Printf("started %d chains", count)
	<-ctx.Done()
}

// waitActive blocks until TPS > 0 or ctx is cancelled.
func (e *Engine) waitActive(ctx context.Context) (int64, bool) {
	for {
		if tps := e.tps.Load(); tps > 0 {
			return tps, true
		}
		select {
		case <-ctx.Done():
			return 0, false
		case <-e.resume.C():
		}
	}
}

// chainInterval returns how long each chain should wait between transactions
// so that the aggregate rate across numChains chains equals tps.
func chainInterval(tps int64) time.Duration {
	return time.Duration(float64(numChains) / float64(tps) * float64(time.Second))
}

func (e *Engine) runChain(ctx context.Context, utxo UTXO) {
	// Spread initial fire across one interval to avoid thundering herd.
	tps, ok := e.waitActive(ctx)
	if !ok {
		return
	}
	jitter := time.Duration(rand.Int63n(int64(chainInterval(tps))))
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	for {
		tps, ok := e.waitActive(ctx)
		if !ok {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(chainInterval(tps)):
		}

		rawHex, newUTXO, err := buildSustainTx(utxo, e.lockScript)
		if err != nil {
			log.Printf("sustain build %s:%d: %v", utxo.TxHash, utxo.TxPos, err)
			continue
		}
		txid, err := e.woc.broadcast(rawHex)
		if err != nil {
			log.Printf("sustain broadcast %s:%d: %v — retry next tick", utxo.TxHash, utxo.TxPos, err)
			continue
		}
		newUTXO.TxHash = txid
		if newUTXO.Value == 0 {
			log.Printf("chain done → %s (0-sat output, chain length %d)", txid, utxo.Value/sustainFee)
			return
		}
		utxo = newUTXO
	}
}
