package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBroadcaster struct {
	count atomic.Int64
	err   error
}

func (f *fakeBroadcaster) Broadcast(_ context.Context, _ []byte) error {
	f.count.Add(1)
	return f.err
}

type panicBroadcaster struct{}

func (p *panicBroadcaster) Broadcast(_ context.Context, _ []byte) error {
	panic("test panic from broadcaster")
}

func newTestEngine(broadcaster Broadcaster) *Engine {
	q := newQueue()
	e := &Engine{
		queue:       q,
		broadcaster: broadcaster,
		lockScript:  buildLockScript(),
		resume:      newNotify(),
		numChains:   4,
		fanoutSize:  2,
	}
	e.state.Lifecycle = LifecycleStarting
	e.state.BootstrapStage = "none"
	e.state.ChainsTarget = 4
	return e
}

// TestBootstrapFull verifies: 1 UTXO → L1(1 broadcast) → L2(2 broadcasts) → 4 leaf UTXOs.
func TestBootstrapFull(t *testing.T) {
	fb := &fakeBroadcaster{}
	e := newTestEngine(fb)
	e.queue.Push(UTXO{TxHash: testTxID, TxPos: 0, Value: 10_000})

	if err := e.bootstrapFull(context.Background()); err != nil {
		t.Fatalf("bootstrapFull: %v", err)
	}
	if got := fb.count.Load(); got != 3 {
		t.Errorf("broadcast count = %d, want 3 (1 L1 + 2 L2)", got)
	}
	if got := e.queue.Len(); got != 4 {
		t.Errorf("queue len = %d, want 4", got)
	}
}

// TestBootstrapL2Resume verifies: 2 existing L1 UTXOs → 2 broadcasts → 4 leaf UTXOs.
func TestBootstrapL2Resume(t *testing.T) {
	fb := &fakeBroadcaster{}
	e := newTestEngine(fb)
	e.queue.Push(UTXO{TxHash: testTxID, TxPos: 0, Value: 5_000})
	e.queue.Push(UTXO{TxHash: testTxID, TxPos: 1, Value: 5_000})

	if err := e.bootstrapL2(context.Background()); err != nil {
		t.Fatalf("bootstrapL2: %v", err)
	}
	if got := fb.count.Load(); got != 2 {
		t.Errorf("broadcast count = %d, want 2", got)
	}
	if got := e.queue.Len(); got != 4 {
		t.Errorf("queue len = %d, want 4", got)
	}
}

// TestChainTermination verifies a chain with value=21 broadcasts 3 times then exits (21→14→7→0).
func TestChainTermination(t *testing.T) {
	fb := &fakeBroadcaster{}
	e := newTestEngine(fb)
	e.SetTPS(10_000)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	e.chainsActiveCnt.Add(1)
	SafeGo(&wg, "chain_term_test", func() {
		defer e.chainsActiveCnt.Add(-1)
		e.runChain(ctx, UTXO{TxHash: testTxID, TxPos: 0, Value: 21})
	})
	wg.Wait()

	if got := fb.count.Load(); got != 3 {
		t.Errorf("broadcast count = %d, want 3 (21→14→7→0)", got)
	}
}

// TestPauseResume verifies waitActive blocks on TPS=0 and unblocks immediately on SetTPS.
func TestPauseResume(t *testing.T) {
	fb := &fakeBroadcaster{}
	e := newTestEngine(fb)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	received := make(chan int64, 1)
	go func() {
		tps, _ := e.waitActive(ctx)
		received <- tps
	}()

	select {
	case <-received:
		t.Fatal("waitActive returned before SetTPS")
	case <-time.After(20 * time.Millisecond):
	}

	e.SetTPS(42)

	select {
	case tps := <-received:
		if tps != 42 {
			t.Errorf("tps = %d, want 42", tps)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("waitActive did not unblock after SetTPS(42)")
	}
}

// TestPanicRecovery verifies SafeGo recovers a panic in a chain goroutine and the engine survives.
func TestPanicRecovery(t *testing.T) {
	pb := &panicBroadcaster{}
	e := newTestEngine(pb)
	e.SetTPS(10_000)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	e.chainsActiveCnt.Add(1)
	SafeGo(&wg, "chain_panic_test", func() {
		defer e.chainsActiveCnt.Add(-1)
		e.runChain(ctx, UTXO{TxHash: testTxID, TxPos: 0, Value: 1000})
	})
	wg.Wait()

	snap := e.Snapshot()
	if snap.Lifecycle == "" {
		t.Error("engine state lost after panic recovery")
	}
}
