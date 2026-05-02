package main

import (
	"context"
	"log"
	"sync/atomic"
	"time"
)

const maxConcurrent = 256

type Engine struct {
	queue      *Queue
	woc        *wocClient
	lockScript []byte
	tps        atomic.Int64
	tpsChange  chan struct{}
	sema       chan struct{}
}

func newEngine(q *Queue, woc *wocClient, lockScript []byte) *Engine {
	return &Engine{
		queue:      q,
		woc:        woc,
		lockScript: lockScript,
		tpsChange:  make(chan struct{}, 1),
		sema:       make(chan struct{}, maxConcurrent),
	}
}

func (e *Engine) SetTPS(tps int64) {
	e.tps.Store(tps)
	select {
	case e.tpsChange <- struct{}{}:
	default:
	}
}

func (e *Engine) TPS() int64 {
	return e.tps.Load()
}

func (e *Engine) run(ctx context.Context) {
	var ticker *time.Ticker
	var tickC <-chan time.Time

	reset := func() {
		if ticker != nil {
			ticker.Stop()
			ticker = nil
			tickC = nil
		}
		tps := e.tps.Load()
		if tps > 0 {
			ticker = time.NewTicker(time.Second / time.Duration(tps))
			tickC = ticker.C
		}
	}

	reset()

	for {
		select {
		case <-ctx.Done():
			if ticker != nil {
				ticker.Stop()
			}
			return
		case <-e.tpsChange:
			reset()
		case <-tickC:
			e.sendOne()
		}
	}
}

func (e *Engine) sendOne() {
	select {
	case e.sema <- struct{}{}:
	default:
		return // at concurrency cap
	}

	utxo, ok := e.queue.Pop()
	if !ok {
		<-e.sema
		log.Println("queue empty")
		return
	}

	go func(utxo UTXO) {
		defer func() { <-e.sema }()

		rawHex, newUTXO, err := buildTx(utxo, e.lockScript)
		if err != nil {
			log.Printf("skip %s:%d: %v", utxo.TxHash, utxo.TxPos, err)
			return
		}

		txid, err := e.woc.broadcast(rawHex)
		if err != nil {
			log.Printf("broadcast %s:%d: %v", utxo.TxHash, utxo.TxPos, err)
			e.queue.Push(utxo)
			return
		}

		newUTXO.TxHash = txid
		e.queue.Push(newUTXO)
		log.Printf("sent %s (queue: %d)", txid, e.queue.Len())
	}(utxo)
}
