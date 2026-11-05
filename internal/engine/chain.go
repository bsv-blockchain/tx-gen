package engine

import (
	"context"
	"sync"
	"time"

	"github.com/bsv-blockchain/tx-gen/internal/store"
	"github.com/bsv-blockchain/tx-gen/internal/txbuilder"
)

// runChains starts one chain per queued UTXO and blocks until ctx is
// canceled and every chain has returned.
func (e *Engine) runChains(ctx context.Context) {
	e.setState(func(s *State) {
		if e.tps.Load() > 0 {
			s.Lifecycle = LifecycleRunning
		} else {
			s.Lifecycle = LifecyclePaused
		}
		s.ChainsTarget = e.numChains
	})

	var chains sync.WaitGroup
	started := 0
	for {
		utxo, ok := e.queue.pop()
		if !ok {
			break
		}
		e.metrics.SetChainsActive(int(e.chainsActive.Add(1)))
		e.runner.Go(&chains, "chain", func() {
			defer func() { e.metrics.SetChainsActive(int(e.chainsActive.Add(-1))) }()
			e.runChain(ctx, utxo)
		})
		started++
	}
	e.logger.Info("chains started", "count", started)
	e.metrics.SetQueueDepth(e.queue.len())

	<-ctx.Done()
	e.setState(func(s *State) { s.Lifecycle = LifecycleStopped })
	chains.Wait()
}

// runChain broadcasts one sustain transaction per interval, each spending the
// previous one's output, until the output is exhausted or ctx is canceled.
//
// A failed broadcast is retried on the next tick with the same UTXO; a store
// failure stops the chain, because its durable tip can no longer be trusted.
func (e *Engine) runChain(ctx context.Context, utxo txbuilder.UTXO) {
	if !e.waitInitialJitter(ctx) {
		return
	}

	hops := 0
	for {
		tx, next, err := txbuilder.BuildSustain(utxo, e.currentMode())
		if err != nil {
			e.logger.Error("build sustain transaction", "txid", utxo.TxHash, "vout", utxo.TxPos, "error", err)
			if !e.waitNextInterval(ctx) {
				return
			}
			continue
		}

		pending := store.PendingBroadcast{
			TxID:    tx.TxID,
			EF:      tx.EF,
			Spent:   []txbuilder.UTXO{utxo},
			Created: []txbuilder.UTXO{next},
			Stage:   string(StageL2Done),
		}
		if err = e.savePending(pending); err != nil {
			e.recordBroadcastFail("store", err)
			e.logger.Error("save pending sustain; stopping chain", "txid", utxo.TxHash, "vout", utxo.TxPos, "error", err)
			e.metrics.IncChainTerminated("store_error")
			return
		}
		if err = e.broadcaster.Broadcast(ctx, tx.TxID, tx.EF); err != nil {
			e.clearPending(tx.TxID)
			e.recordBroadcastFail("arcade", err)
			e.logger.Warn("sustain broadcast failed; retrying next tick", "txid", utxo.TxHash, "vout", utxo.TxPos, "error", err)
			if !e.waitNextInterval(ctx) {
				return
			}
			continue
		}
		if err = e.queue.commit(tx.TxID, pending.Spent, pending.Created); err != nil {
			e.recordBroadcastFail("store", err)
			e.logger.Error("commit sustain; stopping chain", "txid", tx.TxID, "error", err)
			e.metrics.IncChainTerminated("store_error")
			return
		}
		e.recordBroadcastOk()
		hops++

		if next.Value == 0 {
			e.chainsExhausted.Add(1)
			e.metrics.IncChainTerminated("exhausted")
			e.logger.Info("chain exhausted", "final_txid", tx.TxID, "hops", hops)
			return
		}
		utxo = next
		if !e.waitNextInterval(ctx) {
			return
		}
	}
}

// waitActive blocks while the target TPS is 0 and returns the TPS once it is
// positive, together with the wake channel that was current when the TPS was
// read. It returns false when ctx is canceled.
//
// The channel is taken before the TPS is read, so a SetTPS that lands in
// between closes the channel the caller waits on next and is never missed.
func (e *Engine) waitActive(ctx context.Context) (int64, <-chan struct{}, bool) {
	for {
		wake := e.resume.C()
		if tps := e.tps.Load(); tps > 0 {
			return tps, wake, true
		}
		select {
		case <-ctx.Done():
			return 0, nil, false
		case <-wake:
		}
	}
}

// waitDuration waits for d. It reports elapsed=false when wake closed first
// (SetTPS was called), and ok=false when ctx was canceled.
func (e *Engine) waitDuration(ctx context.Context, wake <-chan struct{}, d time.Duration) (elapsed, ok bool) {
	if d <= 0 {
		return true, true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, false
	case <-timer.C:
		return true, true
	case <-wake:
		return false, true
	}
}

// waitInitialJitter waits a random part of one interval, so that chains
// started together do not broadcast in lockstep.
func (e *Engine) waitInitialJitter(ctx context.Context) bool {
	return e.waitInterval(ctx, e.jitter)
}

// waitNextInterval waits one full interval at the current TPS.
func (e *Engine) waitNextInterval(ctx context.Context) bool {
	return e.waitInterval(ctx, func(d time.Duration) time.Duration { return d })
}

// waitInterval waits for wait(interval) at the current TPS and chain count.
//
// A TPS change restarts the wait at the new interval, so rate changes take
// effect immediately even for chains in a long sleep. It returns false when
// ctx is canceled.
func (e *Engine) waitInterval(ctx context.Context, wait func(time.Duration) time.Duration) bool {
	for {
		tps, wake, ok := e.waitActive(ctx)
		if !ok {
			return false
		}
		elapsed, ok := e.waitDuration(ctx, wake, wait(chainInterval(tps, e.chainsActive.Load())))
		if !ok {
			return false
		}
		if elapsed {
			return true
		}
	}
}

// chainInterval returns how often each chain must broadcast so that chains
// chains together reach tps transactions per second.
func chainInterval(tps, chains int64) time.Duration {
	if chains <= 0 || tps <= 0 {
		return time.Second
	}
	return time.Duration(float64(chains) / float64(tps) * float64(time.Second))
}
