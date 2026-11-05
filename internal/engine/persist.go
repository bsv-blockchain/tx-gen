package engine

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/bsv-blockchain/tx-gen/internal/store"
	"github.com/bsv-blockchain/tx-gen/internal/txbuilder"
)

// restore loads the queue and bootstrap stage at startup.
//
// The queue is seeded from initial (and saved to the store) when it is not
// empty, and restored from the store otherwise. Pending broadcasts left by a
// crash are then replayed, and the persisted bootstrap stage is resumed.
func (e *Engine) restore(initial []txbuilder.UTXO) error {
	if e.store == nil {
		for _, u := range initial {
			e.queue.push(u)
		}
		return nil
	}

	if len(initial) > 0 {
		for _, u := range initial {
			e.queue.push(u)
		}
		if err := e.queue.persistAll(); err != nil {
			return fmt.Errorf("persist initial UTXOs: %w", err)
		}
	} else if err := e.queue.restore(); err != nil {
		return fmt.Errorf("restore UTXOs: %w", err)
	}

	changed, err := e.replayPending()
	if err != nil {
		return fmt.Errorf("replay pending broadcasts: %w", err)
	}
	if changed {
		if err = e.queue.restore(); err != nil {
			return fmt.Errorf("restore UTXOs after replay: %w", err)
		}
	}

	stage, err := e.store.GetMeta(store.MetaBootstrapStage)
	if err != nil {
		return fmt.Errorf("restore bootstrap stage: %w", err)
	}
	if stage != "" {
		e.setState(func(s *State) { s.BootstrapStage = Stage(stage) })
		e.metrics.SetBootstrapStage(stage)
	}
	return nil
}

// replayPending applies pending broadcasts left by a crash, trusting that
// Arcade accepted each one (it was written before the broadcast was sent).
//
// Records are applied oldest first. A record without its state transition
// cannot be replayed safely: the stage becomes StageUnknown, which keeps the
// engine from generating until the state is reconciled. It reports whether
// any UTXO changed.
func (e *Engine) replayPending() (bool, error) {
	pending, err := e.store.LoadPending()
	if err != nil {
		return false, err
	}
	records := make([]store.PendingBroadcast, 0, len(pending))
	for _, p := range pending {
		records = append(records, p)
	}
	slices.SortFunc(records, func(a, b store.PendingBroadcast) int {
		return cmp.Or(a.SavedAt.Compare(b.SavedAt), cmp.Compare(a.TxID, b.TxID))
	})

	var changed bool
	var unknown int
	for _, p := range records {
		if len(p.Spent) == 0 && len(p.Created) == 0 {
			unknown++
			continue
		}
		if err = e.store.CommitPending(p.TxID, p.Spent, p.Created); err != nil {
			return changed, fmt.Errorf("commit pending %s: %w", p.TxID, err)
		}
		changed = true
		e.logger.Info("replayed pending broadcast", "txid", p.TxID, "stage", p.Stage)
		if p.Stage != "" {
			e.setBootstrapStage(Stage(p.Stage))
		}
	}

	if unknown > 0 {
		msg := fmt.Sprintf("%d pending broadcasts require WoC reconciliation", unknown)
		e.logger.Warn("pending broadcasts cannot be replayed", "count", unknown)
		e.setBootstrapStage(StageUnknown)
		e.setState(func(s *State) {
			s.LastError = &ErrorRecord{Component: "pending_replay", Msg: msg, Time: time.Now()}
		})
	}
	return changed, nil
}

// setBootstrapStage records stage in memory, the store, and metrics.
func (e *Engine) setBootstrapStage(stage Stage) {
	e.setState(func(s *State) { s.BootstrapStage = stage })
	e.metrics.SetBootstrapStage(string(stage))
	if e.store == nil {
		return
	}
	if err := e.store.SetMeta(store.MetaBootstrapStage, string(stage)); err != nil {
		e.logger.Error("persist bootstrap stage", "stage", stage, "error", err)
	}
}

// savePending writes p before its broadcast.
func (e *Engine) savePending(p store.PendingBroadcast) error {
	if e.store == nil {
		return nil
	}
	return e.store.SavePending(p)
}

// clearPending removes the pending record of a broadcast that failed.
func (e *Engine) clearPending(txid string) {
	if e.store == nil {
		return
	}
	if err := e.store.ClearPending(txid); err != nil {
		e.logger.Error("clear pending broadcast", "txid", txid, "error", err)
	}
}
