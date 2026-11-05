package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bsv-blockchain/tx-gen/internal/backoff"
	"github.com/bsv-blockchain/tx-gen/internal/store"
	"github.com/bsv-blockchain/tx-gen/internal/txbuilder"
)

// l2Result is the outcome of fanning out one L1 parent.
type l2Result struct {
	outputs  []txbuilder.UTXO
	err      error
	accepted bool
}

// bootstrap brings the queue to one UTXO per chain, resuming from the
// persisted stage. It waits for the funding UTXO when there is nothing to
// start from.
func (e *Engine) bootstrap(ctx context.Context) error {
	queued := e.queue.len()
	e.logger.Info("engine starting", "utxos", queued)
	e.setState(func(s *State) {
		s.Lifecycle = LifecycleBootstrapping
		s.ChainsTarget = e.numChains
	})

	stage := e.Snapshot().BootstrapStage
	if stage == StageNone && queued == 0 {
		if err := e.waitForSeedUTXO(ctx); err != nil {
			return err
		}
		queued = e.queue.len()
	}

	// Fix the parameters now: POST /config may change them only while
	// unfunded, and every fanout below must agree on them.
	e.mu.Lock()
	e.bootstrapLocked = true
	cfg := e.bootstrapConfigLocked()
	e.mu.Unlock()
	l1Count := ceilDiv(cfg.NumChains, cfg.FanoutSize)

	switch {
	case stage == StageL2Done:
		e.logger.Info("resuming existing chains", "utxos", queued)
		return nil
	case stage == StageL1Done:
		e.logger.Info("bootstrap: L2 fanout from persisted stage", "from", queued, "to", cfg.NumChains)
		return e.bootstrapL2(ctx)
	case stage == StageL2Partial:
		return e.resumePartialL2(ctx, queued, cfg.NumChains, l1Count)
	case stage == StageUnknown:
		return fmt.Errorf("%w: bootstrap stage %q", ErrManualRecovery, stage)
	case queued == 1:
		e.logger.Info("bootstrap: L1 and L2 fanout", "from", 1, "l1", l1Count, "to", cfg.NumChains)
		return e.bootstrapFull(ctx)
	case queued == l1Count:
		e.logger.Info("bootstrap: L2 fanout only", "from", l1Count, "to", cfg.NumChains)
		return e.bootstrapL2(ctx)
	case queued > 0:
		e.logger.Info("resuming existing chains", "utxos", queued)
		e.setBootstrapStage(StageL2Done)
		return nil
	default:
		return ErrNoUTXOs
	}
}

// resumePartialL2 continues after an interrupted L2 fanout, trusting the
// persisted queue.
func (e *Engine) resumePartialL2(ctx context.Context, queued, numChains, l1Count int) error {
	switch {
	case queued >= numChains:
		e.logger.Info("resuming existing chains after partial L2", "utxos", queued)
		e.setBootstrapStage(StageL2Done)
		return nil
	case queued == l1Count:
		e.logger.Info("bootstrap: L2 fanout from partial stage", "from", queued, "to", numChains)
		return e.bootstrapL2(ctx)
	case queued > 0:
		e.logger.Warn("resuming with a partial L2 fanout", "utxos", queued, "target", numChains)
		e.setBootstrapStage(StageL2Done)
		return nil
	default:
		return fmt.Errorf("%w: stage %q with an empty queue; re-seed required", ErrManualRecovery, StageL2Partial)
	}
}

// waitForSeedUTXO polls the UTXO source until the funding UTXO appears.
func (e *Engine) waitForSeedUTXO(ctx context.Context) error {
	if e.source == nil {
		return ErrNoUTXOs
	}
	scriptHash := e.currentMode().ScriptHash()
	e.logger.Info("waiting for funding UTXO", "script_hash", scriptHash, "poll_interval", e.seedInterval)

	ticker := time.NewTicker(e.seedInterval)
	defer ticker.Stop()
	for {
		utxos, err := e.source.FetchUTXOs(ctx, scriptHash)
		switch {
		case err == nil:
			e.setState(func(s *State) {
				s.LastWoCTime = time.Now()
				s.LastWoCResult = fmt.Sprintf("%d utxos", len(utxos))
			})
			if len(utxos) > 0 {
				for _, u := range utxos {
					if pushErr := e.queue.pushPersisted(u); pushErr != nil {
						return fmt.Errorf("persist funding UTXO: %w", pushErr)
					}
				}
				e.metrics.SetQueueDepth(e.queue.len())
				e.logger.Info("funding UTXO detected", "utxos", len(utxos))
				return nil
			}
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			e.setState(func(s *State) {
				s.LastWoCTime = time.Now()
				s.LastWoCResult = "error"
				s.LastError = &ErrorRecord{Component: "woc", Msg: err.Error(), Time: time.Now()}
			})
			e.logger.Warn("funding UTXO lookup failed", "error", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// bootstrapFull fans the single funding UTXO out to the L1 outputs, then runs
// the L2 fanout.
func (e *Engine) bootstrapFull(ctx context.Context) error {
	e.setBootstrapStage(StageNone)

	seed, ok := e.queue.pop()
	if !ok {
		return ErrNoUTXOs
	}
	cfg := e.BootstrapConfig()
	tx, l1Outputs, err := txbuilder.BuildFanout(seed, ceilDiv(cfg.NumChains, cfg.FanoutSize), e.currentMode())
	if err != nil {
		e.queue.push(seed)
		return fmt.Errorf("build L1 fanout: %w", err)
	}

	pending := store.PendingBroadcast{
		TxID:    tx.TxID,
		EF:      tx.EF,
		Spent:   []txbuilder.UTXO{seed},
		Created: l1Outputs,
		Stage:   string(StageL1Done),
	}
	if err = e.broadcastWithRetry(ctx, pending, "L1"); err != nil {
		e.queue.push(seed)
		return fmt.Errorf("broadcast L1 fanout: %w", err)
	}

	if err = e.queue.commit(tx.TxID, pending.Spent, l1Outputs); err != nil {
		return fmt.Errorf("commit L1 fanout: %w", err)
	}
	e.recordBroadcastOk()
	e.setBootstrapStage(StageL1Done)

	for _, u := range l1Outputs {
		e.queue.push(u)
	}
	return e.bootstrapL2(ctx)
}

// bootstrapL2 fans every queued L1 output out in parallel, spreading
// NumChains chain UTXOs across them. Any failure leaves the stage at
// StageL2Partial and the engine failed: chains start only after a complete
// L2 fanout.
func (e *Engine) bootstrapL2(ctx context.Context) error {
	e.setBootstrapStage(StageL2Partial)

	parents := e.queue.drain()
	if len(parents) == 0 {
		return fmt.Errorf("%w: no L1 outputs to fan out", ErrNoUTXOs)
	}
	numChains := e.BootstrapConfig().NumChains
	if len(parents) > numChains {
		for _, parent := range parents {
			e.queue.push(parent)
		}
		return fmt.Errorf("%w: got %d L1 outputs, want at most %d", ErrManualRecovery, len(parents), numChains)
	}

	results := make([]l2Result, len(parents))
	var wg sync.WaitGroup
	for i, parent := range parents {
		e.runner.Go(&wg, "bootstrap_l2", func() {
			results[i] = e.fanoutParent(ctx, parent, l2FanoutSizeForParent(numChains, len(parents), i))
		})
	}
	wg.Wait()

	failed, produced := 0, 0
	for i, result := range results {
		if !result.accepted {
			failed++
			e.logger.Warn("L2 fanout failed", "parent", i, "txid", parents[i].TxHash, "error", result.err)
			continue
		}
		for _, out := range result.outputs {
			e.queue.push(out)
			produced++
		}
	}
	e.metrics.SetQueueDepth(e.queue.len())
	e.logger.Info("bootstrap L2 complete", "utxos", e.queue.len(), "failed_parents", failed)

	var msg string
	switch {
	case failed > 0:
		msg = fmt.Sprintf("%d/%d L2 fanouts failed", failed, len(parents))
	case produced != numChains:
		msg = fmt.Sprintf("bootstrap produced %d L2 UTXOs, expected %d", produced, numChains)
	default:
		e.setBootstrapStage(StageL2Done)
		return nil
	}
	e.setState(func(s *State) {
		s.Lifecycle = LifecycleFailed
		s.LastError = &ErrorRecord{Component: "bootstrap_l2", Msg: msg, Time: time.Now()}
	})
	e.setBootstrapStage(StageL2Partial)
	return fmt.Errorf("%w: %s", ErrBootstrapIncomplete, msg)
}

// fanoutParent fans one L1 output out to n chain UTXOs. A parent whose fanout
// is not broadcast goes back on the queue for a later retry.
func (e *Engine) fanoutParent(ctx context.Context, parent txbuilder.UTXO, n int) l2Result {
	fail := func(err error, requeue bool) l2Result {
		e.setState(func(s *State) { s.BootstrapFailures++ })
		if requeue {
			e.queue.push(parent)
		}
		return l2Result{err: err}
	}

	tx, outputs, err := txbuilder.BuildFanout(parent, n, e.currentMode())
	if err != nil {
		return fail(fmt.Errorf("build fanout of %s: %w", parent.TxHash, err), true)
	}
	pending := store.PendingBroadcast{
		TxID:    tx.TxID,
		EF:      tx.EF,
		Spent:   []txbuilder.UTXO{parent},
		Created: outputs,
		Stage:   string(StageL2Partial),
	}
	if err = e.broadcastWithRetry(ctx, pending, "L2"); err != nil {
		return fail(fmt.Errorf("broadcast fanout of %s: %w", parent.TxHash, err), true)
	}
	if err = e.queue.commit(tx.TxID, pending.Spent, outputs); err != nil {
		return fail(fmt.Errorf("commit fanout of %s: %w", parent.TxHash, err), false)
	}
	e.recordBroadcastOk()
	return l2Result{outputs: outputs, accepted: true}
}

// broadcastWithRetry broadcasts p, retrying with backoff.
//
// The pending record exists only while an attempt is in flight: it is written
// before each attempt and removed after each failure. A crash during the
// backoff therefore never replays a rejected transaction as accepted, and
// rebuilding the fanout after a restart yields the same transaction.
func (e *Engine) broadcastWithRetry(ctx context.Context, p store.PendingBroadcast, layer string) error {
	var err error
	for attempt := 1; attempt <= bootstrapAttempts; attempt++ {
		if err = e.savePending(p); err != nil {
			return fmt.Errorf("save pending %s: %w", p.TxID, err)
		}
		if err = e.broadcaster.Broadcast(ctx, p.TxID, p.EF); err == nil {
			return nil
		}
		e.clearPending(p.TxID)
		e.logger.Warn("fanout broadcast failed", "layer", layer, "txid", p.TxID, "attempt", attempt, "error", err)
		if attempt == bootstrapAttempts {
			break
		}
		if sleepErr := backoff.Sleep(ctx, backoff.Retry(attempt)); sleepErr != nil {
			return sleepErr
		}
	}
	return err
}

// ceilDiv returns n/d rounded up, or 0 when d is not positive.
func ceilDiv(n, d int) int {
	if d <= 0 {
		return 0
	}
	return (n + d - 1) / d
}

// l2FanoutSizeForParent spreads numChains outputs as evenly as possible over
// parentCount parents: the first numChains%parentCount parents get one extra.
func l2FanoutSizeForParent(numChains, parentCount, parentIndex int) int {
	if numChains <= 0 || parentCount <= 0 || parentIndex < 0 || parentIndex >= parentCount {
		return 0
	}
	size := numChains / parentCount
	if parentIndex < numChains%parentCount {
		size++
	}
	return size
}
