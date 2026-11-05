package engine

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bsv-blockchain/tx-gen/internal/safe"
	"github.com/bsv-blockchain/tx-gen/internal/store"
	"github.com/bsv-blockchain/tx-gen/internal/txbuilder"
)

const (
	testTxID      = "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"
	otherTestTxID = "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"
)

var errBroadcast = errors.New("arcade unavailable")

// fakeBroadcaster records broadcasts and can fail the first failFirst of them.
type fakeBroadcaster struct {
	mu        sync.Mutex
	txids     []string
	failFirst int
	err       error
}

func (f *fakeBroadcaster) Broadcast(_ context.Context, txid string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txids = append(f.txids, txid)
	if len(f.txids) <= f.failFirst {
		return errBroadcast
	}
	return f.err
}

func (f *fakeBroadcaster) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.txids)
}

type panicBroadcaster struct{}

func (panicBroadcaster) Broadcast(context.Context, string, []byte) error {
	panic("test panic from broadcaster")
}

// fakeSource returns utxos once calls reaches readyAfter.
type fakeSource struct {
	utxos      []txbuilder.UTXO
	err        error
	readyAfter int32
	calls      atomic.Int32
}

func (f *fakeSource) FetchUTXOs(context.Context, string) ([]txbuilder.UTXO, error) {
	if f.calls.Add(1) <= f.readyAfter {
		return nil, f.err
	}
	return f.utxos, nil
}

func testMode(t *testing.T) *txbuilder.Mode {
	t.Helper()
	mode, err := txbuilder.NewTaggedDropMode("inst01", 0)
	require.NoError(t, err)
	return mode
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

// newTestEngine returns a 4-chain, fanout-2 engine with optional overrides.
func newTestEngine(t *testing.T, broadcaster Broadcaster, configure ...func(*Options)) *Engine {
	t.Helper()
	opts := Options{
		Mode:             testMode(t),
		Broadcaster:      broadcaster,
		NumChains:        4,
		FanoutSize:       2,
		SeedPollInterval: 5 * time.Millisecond,
	}
	for _, fn := range configure {
		fn(&opts)
	}
	e, err := New(opts)
	require.NoError(t, err)
	e.jitter = func(time.Duration) time.Duration { return 0 }
	return e
}

func seed(value uint64) txbuilder.UTXO {
	return txbuilder.UTXO{TxHash: testTxID, TxPos: 0, Value: value}
}

func TestNewValidatesOptions(t *testing.T) {
	_, err := New(Options{Broadcaster: &fakeBroadcaster{}})
	require.ErrorIs(t, err, ErrModeRequired)

	_, err = New(Options{Mode: testMode(t)})
	require.ErrorIs(t, err, ErrBroadcasterRequired)
}

func TestNewAppliesDefaults(t *testing.T) {
	e, err := New(Options{Mode: testMode(t), Broadcaster: &fakeBroadcaster{}})
	require.NoError(t, err)
	cfg := e.BootstrapConfig()
	assert.Equal(t, DefaultNumChains, cfg.NumChains)
	assert.Equal(t, DefaultFanoutSize, cfg.FanoutSize)
	assert.Equal(t, txbuilder.DefaultSustainFee, cfg.SustainFee)
	assert.Equal(t, DefaultSeedPollInterval, e.seedInterval)

	snap := e.Snapshot()
	assert.Equal(t, LifecycleStarting, snap.Lifecycle)
	assert.Equal(t, StageNone, snap.BootstrapStage)
	assert.Equal(t, DefaultNumChains, snap.ChainsTarget)
}

// TestBootstrapFull verifies 1 UTXO -> L1 (1 broadcast) -> L2 (2 broadcasts) -> 4 chain UTXOs.
func TestBootstrapFull(t *testing.T) {
	fb := &fakeBroadcaster{}
	e := newTestEngine(t, fb, func(o *Options) { o.InitialUTXOs = []txbuilder.UTXO{seed(10_000)} })

	require.NoError(t, e.bootstrapFull(t.Context()))
	assert.Equal(t, 3, fb.count(), "1 L1 + 2 L2 broadcasts")
	assert.Equal(t, 4, e.queue.len())
	assert.Equal(t, StageL2Done, e.Snapshot().BootstrapStage)
}

// TestBootstrapFullNonSquare verifies NumChains need not be FanoutSize squared.
func TestBootstrapFullNonSquare(t *testing.T) {
	fb := &fakeBroadcaster{}
	e := newTestEngine(t, fb, func(o *Options) {
		o.NumChains, o.FanoutSize = 5, 3
		o.InitialUTXOs = []txbuilder.UTXO{seed(10_000)}
	})

	require.NoError(t, e.bootstrapFull(t.Context()))
	assert.Equal(t, 3, fb.count(), "1 L1 + 2 L2 broadcasts")
	assert.Equal(t, 5, e.queue.len())
}

func TestBootstrapFullPersistsEveryStep(t *testing.T) {
	s := newTestStore(t)
	e := newTestEngine(t, &fakeBroadcaster{}, func(o *Options) {
		o.Store = s
		o.InitialUTXOs = []txbuilder.UTXO{seed(10_000)}
	})

	require.NoError(t, e.bootstrapFull(t.Context()))
	utxos, err := s.LoadAll()
	require.NoError(t, err)
	assert.Len(t, utxos, 4, "only the chain UTXOs remain durable")
	pending, err := s.LoadPending()
	require.NoError(t, err)
	assert.Empty(t, pending)
	stage, err := s.GetMeta(store.MetaBootstrapStage)
	require.NoError(t, err)
	assert.Equal(t, string(StageL2Done), stage)
}

func TestBootstrapFullRequeuesSeedWhenBroadcastFails(t *testing.T) {
	fb := &fakeBroadcaster{err: errBroadcast}
	s := newTestStore(t)
	e := newTestEngine(t, fb, func(o *Options) {
		o.Store = s
		o.InitialUTXOs = []txbuilder.UTXO{seed(10_000)}
	})

	err := e.bootstrapFull(t.Context())
	require.ErrorIs(t, err, errBroadcast)
	assert.Equal(t, bootstrapAttempts, fb.count())
	assert.Equal(t, 1, e.queue.len(), "the seed goes back on the queue")
	pending, loadErr := s.LoadPending()
	require.NoError(t, loadErr)
	assert.Empty(t, pending, "no pending record survives a failed broadcast")
}

func TestBootstrapFullRetriesTransientFailures(t *testing.T) {
	fb := &fakeBroadcaster{failFirst: 1}
	e := newTestEngine(t, fb, func(o *Options) { o.InitialUTXOs = []txbuilder.UTXO{seed(10_000)} })

	require.NoError(t, e.bootstrapFull(t.Context()))
	assert.Equal(t, 4, fb.count(), "1 failed + 1 accepted L1, then 2 L2")
	assert.Equal(t, 4, e.queue.len())
}

func TestBootstrapFullRejectsUnfundableSeed(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{}, func(o *Options) { o.InitialUTXOs = []txbuilder.UTXO{seed(5)} })
	err := e.bootstrapFull(t.Context())
	require.ErrorIs(t, err, txbuilder.ErrInsufficientValue)
	assert.Equal(t, 1, e.queue.len())
}

func TestBootstrapFullEmptyQueue(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{})
	require.ErrorIs(t, e.bootstrapFull(t.Context()), ErrNoUTXOs)
}

// TestBootstrapL2Resume verifies 2 L1 UTXOs -> 2 broadcasts -> 4 chain UTXOs.
func TestBootstrapL2Resume(t *testing.T) {
	fb := &fakeBroadcaster{}
	e := newTestEngine(t, fb, func(o *Options) {
		o.InitialUTXOs = []txbuilder.UTXO{
			{TxHash: testTxID, TxPos: 0, Value: 5_000},
			{TxHash: testTxID, TxPos: 1, Value: 5_000},
		}
	})

	require.NoError(t, e.bootstrapL2(t.Context()))
	assert.Equal(t, 2, fb.count())
	assert.Equal(t, 4, e.queue.len())
	assert.Equal(t, StageL2Done, e.Snapshot().BootstrapStage)
}

func TestBootstrapL2NonSquare(t *testing.T) {
	fb := &fakeBroadcaster{}
	e := newTestEngine(t, fb, func(o *Options) {
		o.NumChains, o.FanoutSize = 5, 3
		o.InitialUTXOs = []txbuilder.UTXO{
			{TxHash: testTxID, TxPos: 0, Value: 5_000},
			{TxHash: testTxID, TxPos: 1, Value: 5_000},
		}
	})

	require.NoError(t, e.bootstrapL2(t.Context()))
	assert.Equal(t, 2, fb.count())
	assert.Equal(t, 5, e.queue.len())
}

func TestBootstrapL2FailureLeavesPartialStage(t *testing.T) {
	fb := &fakeBroadcaster{err: errBroadcast}
	e := newTestEngine(t, fb, func(o *Options) {
		o.InitialUTXOs = []txbuilder.UTXO{
			{TxHash: testTxID, TxPos: 0, Value: 5_000},
			{TxHash: testTxID, TxPos: 1, Value: 5_000},
		}
	})

	err := e.bootstrapL2(t.Context())
	require.ErrorIs(t, err, ErrBootstrapIncomplete)
	snap := e.Snapshot()
	assert.Equal(t, StageL2Partial, snap.BootstrapStage)
	assert.Equal(t, LifecycleFailed, snap.Lifecycle)
	assert.Equal(t, 2, snap.BootstrapFailures)
	require.NotNil(t, snap.LastError)
	assert.Equal(t, "bootstrap_l2", snap.LastError.Component)
	assert.Equal(t, 2, e.queue.len(), "failed parents go back on the queue")
}

func TestBootstrapL2RecoversPanickingParent(t *testing.T) {
	var panics atomic.Int32
	e := newTestEngine(t, panicBroadcaster{}, func(o *Options) {
		o.Runner = safe.Runner{OnPanic: func(string) { panics.Add(1) }}
		o.InitialUTXOs = []txbuilder.UTXO{
			{TxHash: testTxID, TxPos: 0, Value: 5_000},
			{TxHash: testTxID, TxPos: 1, Value: 5_000},
		}
	})

	require.ErrorIs(t, e.bootstrapL2(t.Context()), ErrBootstrapIncomplete)
	assert.Equal(t, int32(2), panics.Load())
}

func TestBootstrapL2RejectsTooManyParents(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{}, func(o *Options) {
		o.NumChains = 1
		o.InitialUTXOs = []txbuilder.UTXO{
			{TxHash: testTxID, TxPos: 0, Value: 5_000},
			{TxHash: testTxID, TxPos: 1, Value: 5_000},
		}
	})
	require.ErrorIs(t, e.bootstrapL2(t.Context()), ErrManualRecovery)
	assert.Equal(t, 2, e.queue.len())
}

func TestBootstrapL2EmptyQueue(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{})
	require.ErrorIs(t, e.bootstrapL2(t.Context()), ErrNoUTXOs)
}

func TestBootstrapL2DoesNotRequireWoCReconcile(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{}, func(o *Options) {
		o.Source = &fakeSource{utxos: []txbuilder.UTXO{{TxHash: otherTestTxID, Value: 1}}}
		o.InitialUTXOs = []txbuilder.UTXO{
			{TxHash: testTxID, TxPos: 0, Value: 5_000},
			{TxHash: testTxID, TxPos: 1, Value: 5_000},
		}
	})

	require.NoError(t, e.bootstrapL2(t.Context()))
	assert.Equal(t, StageL2Done, e.Snapshot().BootstrapStage)
	assert.Equal(t, 4, e.queue.len())
}

func TestBootstrapDispatch(t *testing.T) {
	l1 := []txbuilder.UTXO{
		{TxHash: testTxID, TxPos: 0, Value: 5_000},
		{TxHash: testTxID, TxPos: 1, Value: 5_000},
	}
	three := append(append([]txbuilder.UTXO(nil), l1...), txbuilder.UTXO{TxHash: testTxID, TxPos: 2, Value: 5_000})
	four := append(append([]txbuilder.UTXO(nil), three...), txbuilder.UTXO{TxHash: testTxID, TxPos: 3, Value: 5_000})

	tests := []struct {
		name           string
		stage          Stage
		initial        []txbuilder.UTXO
		wantErr        error
		wantBroadcasts int
		wantQueue      int
		wantStage      Stage
	}{
		{name: "fresh seed runs full bootstrap", stage: StageNone, initial: []txbuilder.UTXO{seed(10_000)}, wantBroadcasts: 3, wantQueue: 4, wantStage: StageL2Done},
		{name: "L1 outputs run L2 only", stage: StageNone, initial: l1, wantBroadcasts: 2, wantQueue: 4, wantStage: StageL2Done},
		{name: "other counts resume as chains", stage: StageNone, initial: three, wantQueue: 3, wantStage: StageL2Done},
		{name: "l2_done resumes", stage: StageL2Done, initial: three, wantQueue: 3, wantStage: StageL2Done},
		{name: "l1_done runs L2", stage: StageL1Done, initial: l1, wantBroadcasts: 2, wantQueue: 4, wantStage: StageL2Done},
		{name: "l2_partial with L1 outputs reruns L2", stage: StageL2Partial, initial: l1, wantBroadcasts: 2, wantQueue: 4, wantStage: StageL2Done},
		{name: "l2_partial with all chains resumes", stage: StageL2Partial, initial: four, wantQueue: 4, wantStage: StageL2Done},
		{name: "l2_partial with some chains resumes", stage: StageL2Partial, initial: three, wantQueue: 3, wantStage: StageL2Done},
		{name: "l2_partial with nothing needs recovery", stage: StageL2Partial, wantErr: ErrManualRecovery, wantStage: StageL2Partial},
		{name: "unknown needs recovery", stage: StageUnknown, initial: three, wantErr: ErrManualRecovery, wantQueue: 3, wantStage: StageUnknown},
		{name: "nothing to start from", stage: StageNone, wantErr: ErrNoUTXOs, wantStage: StageNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakeBroadcaster{}
			s := newTestStore(t)
			require.NoError(t, s.SetMeta(store.MetaBootstrapStage, string(tc.stage)))
			e := newTestEngine(t, fb, func(o *Options) {
				o.Store = s
				o.InitialUTXOs = tc.initial
			})

			err := e.bootstrap(t.Context())
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantBroadcasts, fb.count(), "broadcasts")
			assert.Equal(t, tc.wantQueue, e.queue.len(), "queue length")
			assert.Equal(t, tc.wantStage, e.Snapshot().BootstrapStage, "stage")
		})
	}
}

func TestRunWaitsForFundingThenRunsChains(t *testing.T) {
	fb := &fakeBroadcaster{}
	source := &fakeSource{utxos: []txbuilder.UTXO{seed(1_000_000)}, readyAfter: 2, err: errBroadcast}
	s := newTestStore(t)
	e := newTestEngine(t, fb, func(o *Options) {
		o.Source = source
		o.Store = s
	})
	e.SetTPS(1_000)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	require.Eventually(t, func() bool { return e.Snapshot().ChainsActive == 4 }, 5*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool { return fb.count() > 3 }, 5*time.Second, 5*time.Millisecond, "chains broadcast after bootstrap")
	snap := e.Snapshot()
	assert.Equal(t, LifecycleRunning, snap.Lifecycle)
	assert.Equal(t, StageL2Done, snap.BootstrapStage)
	assert.Equal(t, "1 utxos", snap.LastWoCResult)
	assert.GreaterOrEqual(t, source.calls.Load(), int32(3))

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	assert.Equal(t, LifecycleStopped, e.Snapshot().Lifecycle)
}

func TestRunReportsBootstrapFailure(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{})
	err := e.Run(t.Context())
	require.ErrorIs(t, err, ErrNoUTXOs)
	snap := e.Snapshot()
	assert.Equal(t, LifecycleFailed, snap.Lifecycle)
	require.NotNil(t, snap.LastError)
	assert.Equal(t, "bootstrap", snap.LastError.Component)
}

func TestRunStopsWhileWaitingForFunding(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{}, func(o *Options) { o.Source = &fakeSource{readyAfter: 1 << 30} })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunChainsKeepsActiveTipsDurable(t *testing.T) {
	s := newTestStore(t)
	active := seed(1_000)
	e := newTestEngine(t, &fakeBroadcaster{}, func(o *Options) {
		o.Store = s
		o.InitialUTXOs = []txbuilder.UTXO{active}
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		e.runChains(ctx)
		close(done)
	}()
	require.Eventually(t, func() bool { return e.chainsActive.Load() == 1 }, time.Second, 5*time.Millisecond)

	utxos, err := s.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, []txbuilder.UTXO{active}, utxos, "a running chain's tip stays durable")
	assert.Equal(t, LifecyclePaused, e.Snapshot().Lifecycle)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runChains did not stop")
	}
}

func TestPendingReplayAppliesDurableTransition(t *testing.T) {
	s := newTestStore(t)
	oldTip := seed(1_000)
	newTip := txbuilder.UTXO{TxHash: otherTestTxID, TxPos: 0, Value: 993}
	require.NoError(t, s.SaveUTXO(oldTip))
	require.NoError(t, s.SavePending(store.PendingBroadcast{
		TxID:    otherTestTxID,
		EF:      []byte{1, 2, 3},
		Spent:   []txbuilder.UTXO{oldTip},
		Created: []txbuilder.UTXO{newTip},
		Stage:   string(StageL2Done),
	}))

	e := newTestEngine(t, &fakeBroadcaster{}, func(o *Options) {
		o.Store = s
		o.InitialUTXOs = []txbuilder.UTXO{oldTip}
	})

	utxos, err := s.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, []txbuilder.UTXO{newTip}, utxos)
	pending, err := s.LoadPending()
	require.NoError(t, err)
	assert.Empty(t, pending)
	assert.Equal(t, StageL2Done, e.Snapshot().BootstrapStage)
	require.Equal(t, 1, e.queue.len())
	u, ok := e.queue.pop()
	require.True(t, ok)
	assert.Equal(t, newTip, u)
}

func TestPendingReplayMarksUnreplayableRecordsUnknown(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.SavePending(store.PendingBroadcast{TxID: otherTestTxID, EF: []byte{1}}))

	e := newTestEngine(t, &fakeBroadcaster{}, func(o *Options) { o.Store = s })
	snap := e.Snapshot()
	assert.Equal(t, StageUnknown, snap.BootstrapStage)
	require.NotNil(t, snap.LastError)
	assert.Equal(t, "pending_replay", snap.LastError.Component)

	stage, err := s.GetMeta(store.MetaBootstrapStage)
	require.NoError(t, err)
	assert.Equal(t, string(StageUnknown), stage)
	require.ErrorIs(t, e.bootstrap(t.Context()), ErrManualRecovery)
}

func TestNewRestoresBootstrapStageAndQueue(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.SetMeta(store.MetaBootstrapStage, string(StageL2Done)))
	require.NoError(t, s.SaveUTXO(seed(1_000)))

	e := newTestEngine(t, &fakeBroadcaster{}, func(o *Options) { o.Store = s })
	assert.Equal(t, StageL2Done, e.Snapshot().BootstrapStage)
	assert.Equal(t, 1, e.queue.len())
}

func TestNewPersistsInitialUTXOs(t *testing.T) {
	s := newTestStore(t)
	newTestEngine(t, &fakeBroadcaster{}, func(o *Options) {
		o.Store = s
		o.InitialUTXOs = []txbuilder.UTXO{seed(1_000)}
	})
	utxos, err := s.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, []txbuilder.UTXO{seed(1_000)}, utxos)
}

func TestConfigureBootstrap(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{})
	numChains, fanoutSize, fee := 1_200, 100, uint64(9)

	cfg, err := e.ConfigureBootstrap(BootstrapUpdate{NumChains: &numChains, FanoutSize: &fanoutSize, SustainFee: &fee})
	require.NoError(t, err)
	assert.Equal(t, BootstrapConfig{NumChains: 1_200, FanoutSize: 100, SustainFee: 9}, cfg)
	assert.Equal(t, cfg, e.BootstrapConfig())
	assert.Equal(t, 1_200, e.Snapshot().ChainsTarget)
	assert.Equal(t, uint64(9), e.currentMode().SustainFee)
}

func TestConfigureBootstrapValidatesBeforeApplying(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{})
	good, bad, zero := 50, 0, uint64(0)

	_, err := e.ConfigureBootstrap(BootstrapUpdate{NumChains: &good, FanoutSize: &bad})
	require.ErrorIs(t, err, ErrInvalidBootstrap)
	assert.Equal(t, 4, e.BootstrapConfig().NumChains, "nothing is applied when any field is invalid")

	_, err = e.ConfigureBootstrap(BootstrapUpdate{NumChains: &bad})
	require.ErrorIs(t, err, ErrInvalidBootstrap)
	_, err = e.ConfigureBootstrap(BootstrapUpdate{SustainFee: &zero})
	require.ErrorIs(t, err, ErrInvalidBootstrap)
}

func TestConfigureBootstrapLockedAfterFunding(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{}, func(o *Options) { o.InitialUTXOs = []txbuilder.UTXO{seed(10_000)} })
	numChains := 1_200
	_, err := e.ConfigureBootstrap(BootstrapUpdate{NumChains: &numChains})
	require.ErrorIs(t, err, ErrBootstrapLocked)
	assert.Equal(t, 4, e.BootstrapConfig().NumChains)
}

// TestChainTermination verifies a 21-sat chain broadcasts 3 times and exits (21 -> 14 -> 7 -> 0).
func TestChainTermination(t *testing.T) {
	fb := &fakeBroadcaster{}
	e := newTestEngine(t, fb)
	e.SetTPS(10_000)
	e.chainsActive.Add(1)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	e.runChain(ctx, seed(21))

	assert.Equal(t, 3, fb.count())
	assert.Equal(t, 1, e.Snapshot().ChainsExhausted)
}

func TestRunChainPersistsEachHop(t *testing.T) {
	s := newTestStore(t)
	fb := &fakeBroadcaster{}
	e := newTestEngine(t, fb, func(o *Options) {
		o.Store = s
		o.InitialUTXOs = []txbuilder.UTXO{seed(14)}
	})
	e.SetTPS(10_000)
	e.chainsActive.Add(1)
	e.runChain(t.Context(), seed(14))

	assert.Equal(t, 2, fb.count())
	utxos, err := s.LoadAll()
	require.NoError(t, err)
	assert.Empty(t, utxos, "an exhausted chain leaves nothing to spend")
}

func TestRunChainRetriesFailedBroadcast(t *testing.T) {
	fb := &fakeBroadcaster{failFirst: 2}
	e := newTestEngine(t, fb)
	e.SetTPS(10_000)
	e.chainsActive.Add(1)
	e.runChain(t.Context(), seed(7))

	assert.Equal(t, 3, fb.count(), "two failures, then the final hop")
	assert.Equal(t, 1, e.Snapshot().ChainsExhausted)
}

func TestRunChainBroadcastsAfterInitialJitter(t *testing.T) {
	fb := &fakeBroadcaster{}
	e := newTestEngine(t, fb)
	e.chainsActive.Add(1)
	e.SetTPS(1)

	done := make(chan struct{})
	go func() {
		e.runChain(t.Context(), seed(txbuilder.DefaultSustainFee))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("runChain did not broadcast after the initial jitter")
	}
	assert.Equal(t, 1, fb.count())
}

func TestRunChainStopsOnCancel(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	e.runChain(ctx, seed(1_000)) // TPS is 0: returns once ctx is done
}

func TestDegradedAfterConsecutiveFailures(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{})
	e.setState(func(s *State) { s.Lifecycle = LifecycleRunning })

	for range degradedAfterFailures - 1 {
		e.recordBroadcastFail("arcade", errBroadcast)
	}
	assert.Equal(t, LifecycleRunning, e.Snapshot().Lifecycle)
	e.recordBroadcastFail("arcade", errBroadcast)
	snap := e.Snapshot()
	assert.Equal(t, LifecycleDegraded, snap.Lifecycle)
	assert.Equal(t, degradedAfterFailures, snap.ConsecutiveArcadeFailures)

	e.recordBroadcastOk()
	snap = e.Snapshot()
	assert.Equal(t, LifecycleRunning, snap.Lifecycle)
	assert.Zero(t, snap.ConsecutiveArcadeFailures)
	assert.False(t, snap.LastBroadcastOk.IsZero())
}

func TestSetTPSLifecycle(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{})
	e.setState(func(s *State) { s.Lifecycle = LifecyclePaused })

	e.SetTPS(16)
	assert.Equal(t, LifecycleRunning, e.Snapshot().Lifecycle)
	assert.Equal(t, int64(16), e.TPS())
	assert.Equal(t, int64(16), e.Snapshot().RequestedTPS)

	e.SetTPS(0)
	assert.Equal(t, LifecyclePaused, e.Snapshot().Lifecycle)

	e.setState(func(s *State) { s.Lifecycle = LifecycleBootstrapping })
	e.SetTPS(5)
	assert.Equal(t, LifecycleBootstrapping, e.Snapshot().Lifecycle, "SetTPS does not end bootstrap")
}

// TestPauseResume verifies that waitActive blocks at TPS 0 and wakes immediately on SetTPS.
func TestPauseResume(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{})
	received := make(chan int64, 1)
	go func() {
		tps, _, _ := e.waitActive(t.Context())
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
		assert.Equal(t, int64(42), tps)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("waitActive did not wake after SetTPS(42)")
	}
}

// TestWaitDurationHonorsEarlierWake verifies a SetTPS that lands between
// reading the TPS and starting the wait is not lost.
func TestWaitDurationHonorsEarlierWake(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{})
	e.SetTPS(1)
	_, wake, ok := e.waitActive(t.Context())
	require.True(t, ok)

	e.SetTPS(10_000) // lands before the wait starts

	done := make(chan bool, 1)
	go func() {
		elapsed, _ := e.waitDuration(t.Context(), wake, time.Hour)
		done <- elapsed
	}()
	select {
	case elapsed := <-done:
		assert.False(t, elapsed, "the wait was interrupted, not elapsed")
	case <-time.After(time.Second):
		t.Fatal("a SetTPS before the wait started was missed")
	}
}

// blockingBroadcaster blocks its first broadcast until release is closed.
type blockingBroadcaster struct {
	fakeBroadcaster

	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingBroadcaster) Broadcast(ctx context.Context, txid string, ef []byte) error {
	b.once.Do(func() {
		close(b.started)
		<-b.release
	})
	return b.fakeBroadcaster.Broadcast(ctx, txid, ef)
}

// TestConfigureBootstrapLockedDuringL1 verifies bootstrap parameters cannot
// change once the funding UTXO is being spent, even though the queue is
// momentarily empty while the L1 fanout is in flight.
func TestConfigureBootstrapLockedDuringL1(t *testing.T) {
	b := &blockingBroadcaster{started: make(chan struct{}), release: make(chan struct{})}
	e := newTestEngine(t, b, func(o *Options) { o.InitialUTXOs = []txbuilder.UTXO{seed(10_000)} })

	done := make(chan error, 1)
	go func() { done <- e.bootstrap(t.Context()) }()
	<-b.started
	require.Zero(t, e.queue.len(), "the seed is popped while the L1 broadcast is in flight")

	numChains := 2
	_, err := e.ConfigureBootstrap(BootstrapUpdate{NumChains: &numChains})
	require.ErrorIs(t, err, ErrBootstrapLocked)

	close(b.release)
	require.NoError(t, <-done)
	assert.Equal(t, 4, e.queue.len(), "bootstrap used the parameters it started with")
}

func TestRunResetsAcceptedTPSWhenItEnds(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{})
	e.acceptedTPS.Store(7)
	require.ErrorIs(t, e.Run(t.Context()), ErrNoUTXOs)
	assert.Zero(t, e.Snapshot().AcceptedTPS)
}

// TestSetTPSInterruptsLongWait verifies a TPS increase cuts a long interval short.
func TestSetTPSInterruptsLongWait(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{})
	e.chainsActive.Store(10_000)
	e.SetTPS(1) // 10,000 chains at 1 TPS: one broadcast per chain every 2h46m

	done := make(chan bool, 1)
	go func() { done <- e.waitNextInterval(t.Context()) }()
	time.Sleep(20 * time.Millisecond)
	e.chainsActive.Store(1)
	e.SetTPS(10_000) // now 100µs

	select {
	case ok := <-done:
		assert.True(t, ok)
	case <-time.After(time.Second):
		t.Fatal("SetTPS did not interrupt the long wait")
	}
}

// TestPanicRecovery verifies a panic in a chain is recovered and counted, and the engine survives.
func TestPanicRecovery(t *testing.T) {
	var panics atomic.Int32
	e := newTestEngine(t, panicBroadcaster{}, func(o *Options) {
		o.Runner = safe.Runner{OnPanic: func(string) { panics.Add(1) }}
	})
	e.SetTPS(10_000)
	e.chainsActive.Add(1)

	var wg sync.WaitGroup
	e.runner.Go(&wg, "chain", func() { e.runChain(t.Context(), seed(1_000)) })
	wg.Wait()

	assert.Equal(t, int32(1), panics.Load())
	assert.Equal(t, LifecycleStarting, e.Snapshot().Lifecycle, "engine state survives the panic")
}

func TestRecordTipAndReorg(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{})
	at := time.Unix(1_700_000_000, 0)
	e.RecordTip(900_000, "tip-hash", at)
	e.chainsActive.Store(7)

	impacted := e.RecordReorg(899_999, 2, "old", "new")
	assert.Equal(t, 7, impacted)

	snap := e.Snapshot()
	assert.Equal(t, SSEState{Connected: true, TipHeight: 900_000, TipHash: "tip-hash", TipTime: at}, snap.SSE)
	require.NotNil(t, snap.LastReorg)
	assert.Equal(t, 899_999, snap.LastReorg.Height)
	assert.Equal(t, 2, snap.LastReorg.Depth)
	assert.Equal(t, 7, snap.LastReorg.ChainsImpacted)
	assert.Equal(t, "old", snap.LastReorg.TipBefore)
	assert.Equal(t, "new", snap.LastReorg.TipAfter)
}

func TestTrackTPS(t *testing.T) {
	e := newTestEngine(t, &fakeBroadcaster{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go e.trackTPS(ctx)

	for range 3 {
		e.recordBroadcastOk()
	}
	require.Eventually(t, func() bool { return e.Snapshot().AcceptedTPS == 3 }, 3*time.Second, 10*time.Millisecond)
}

func TestChainInterval(t *testing.T) {
	assert.Equal(t, 600*time.Second, chainInterval(16, 9_600))
	assert.Equal(t, time.Second, chainInterval(0, 9_600))
	assert.Equal(t, time.Second, chainInterval(16, 0))
	assert.Equal(t, 100*time.Microsecond, chainInterval(10_000, 1))
}

func TestL2FanoutSizeForParent(t *testing.T) {
	sizes := make([]int, 3)
	total := 0
	for i := range sizes {
		sizes[i] = l2FanoutSizeForParent(10, 3, i)
		total += sizes[i]
	}
	assert.Equal(t, []int{4, 3, 3}, sizes)
	assert.Equal(t, 10, total)

	assert.Zero(t, l2FanoutSizeForParent(0, 3, 0))
	assert.Zero(t, l2FanoutSizeForParent(10, 0, 0))
	assert.Zero(t, l2FanoutSizeForParent(10, 3, 3))
	assert.Zero(t, l2FanoutSizeForParent(10, 3, -1))
}

func TestCeilDiv(t *testing.T) {
	assert.Equal(t, 100, ceilDiv(10_000, 100))
	assert.Equal(t, 2, ceilDiv(5, 3))
	assert.Equal(t, 1, ceilDiv(1, 100))
	assert.Zero(t, ceilDiv(5, 0))
}

func TestQueueOrdersOldestConfirmationFirst(t *testing.T) {
	q := newQueue(nil)
	q.push(txbuilder.UTXO{TxHash: "unconfirmed", Height: 0})
	q.push(txbuilder.UTXO{TxHash: "newer", Height: 900})
	q.push(txbuilder.UTXO{TxHash: "older", Height: 100})

	order := make([]string, 0, 3)
	for _, u := range q.drain() {
		order = append(order, u.TxHash)
	}
	assert.Equal(t, []string{"older", "newer", "unconfirmed"}, order)
	_, ok := q.pop()
	assert.False(t, ok)
}

func TestQueueWithoutStore(t *testing.T) {
	q := newQueue(nil)
	require.NoError(t, q.pushPersisted(seed(1)))
	require.NoError(t, q.persistAll())
	require.NoError(t, q.restore())
	require.NoError(t, q.commit("x", nil, nil))
	assert.Equal(t, 1, q.len())
}
