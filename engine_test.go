package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const otherTestTxID = "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"

type fakeBroadcaster struct {
	count atomic.Int64
	err   error
}

func (f *fakeBroadcaster) Broadcast(_ context.Context, _ []byte) error {
	f.count.Add(1)
	return f.err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type fakeKnownTxBroadcaster struct {
	count atomic.Int64
	txid  string
	kind  string
	err   error
}

func (f *fakeKnownTxBroadcaster) Broadcast(_ context.Context, _ []byte) error {
	f.count.Add(1)
	return f.err
}

func (f *fakeKnownTxBroadcaster) BroadcastKnownTx(_ context.Context, kind, txid string, _ []byte) error {
	f.count.Add(1)
	f.kind = kind
	f.txid = txid
	return f.err
}

type panicBroadcaster struct{}

func (p *panicBroadcaster) Broadcast(_ context.Context, _ []byte) error {
	panic("test panic from broadcaster")
}

type fakeUTXOSource struct {
	utxos []UTXO
	err   error
}

func (f fakeUTXOSource) FetchUTXOs(_ context.Context, _ string) ([]UTXO, error) {
	return f.utxos, f.err
}

func newTestEngine(broadcaster Broadcaster) *Engine {
	q := newQueue()
	e := &Engine{
		queue:       q,
		broadcaster: broadcaster,
		lockScript:  testLockScript(),
		resume:      newNotify(),
		numChains:   4,
		fanoutSize:  2,
	}
	e.state.Lifecycle = LifecycleStarting
	e.state.BootstrapStage = "none"
	e.state.ChainsTarget = 4
	return e
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close store: %v", err)
		}
	})
	return store
}

func waitUntil(t *testing.T, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
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

// TestBootstrapFullNonSquare verifies arbitrary NUM_CHAINS values do not need FANOUT_SIZE^2.
func TestBootstrapFullNonSquare(t *testing.T) {
	fb := &fakeBroadcaster{}
	e := newTestEngine(fb)
	e.numChains = 5
	e.fanoutSize = 3
	e.queue.Push(UTXO{TxHash: testTxID, TxPos: 0, Value: 10_000})

	if err := e.bootstrapFull(context.Background()); err != nil {
		t.Fatalf("bootstrapFull: %v", err)
	}
	if got := fb.count.Load(); got != 3 {
		t.Errorf("broadcast count = %d, want 3 (1 L1 + 2 L2)", got)
	}
	if got := e.queue.Len(); got != 5 {
		t.Errorf("queue len = %d, want 5", got)
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

func TestBootstrapL2NonSquare(t *testing.T) {
	fb := &fakeBroadcaster{}
	e := newTestEngine(fb)
	e.numChains = 5
	e.fanoutSize = 3
	e.queue.Push(UTXO{TxHash: testTxID, TxPos: 0, Value: 5_000})
	e.queue.Push(UTXO{TxHash: testTxID, TxPos: 1, Value: 5_000})

	if err := e.bootstrapL2(context.Background()); err != nil {
		t.Fatalf("bootstrapL2: %v", err)
	}
	if got := fb.count.Load(); got != 2 {
		t.Errorf("broadcast count = %d, want 2", got)
	}
	if got := e.queue.Len(); got != 5 {
		t.Errorf("queue len = %d, want 5", got)
	}
}

func TestStartChainsKeepsActiveTipsDurable(t *testing.T) {
	store := newTestStore(t)
	q := newQueue()
	q.SetStore(store)
	active := UTXO{TxHash: testTxID, TxPos: 0, Value: 1_000}
	if err := q.PushPersisted(active); err != nil {
		t.Fatalf("PushPersisted: %v", err)
	}

	e := newTestEngine(&fakeBroadcaster{})
	e.queue = q
	e.store = store

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.startChains(ctx)
		close(done)
	}()
	waitUntil(t, time.Second, func() bool {
		return e.chainsActiveCnt.Load() == 1
	})

	utxos, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(utxos) != 1 || utxos[0].TxHash != active.TxHash || utxos[0].TxPos != active.TxPos {
		t.Fatalf("durable active tips = %+v, want only %+v", utxos, active)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("startChains did not stop")
	}
}

func TestPendingReplayAppliesDurableTransition(t *testing.T) {
	store := newTestStore(t)
	oldTip := UTXO{TxHash: testTxID, TxPos: 0, Value: 1_000}
	newTip := UTXO{TxHash: otherTestTxID, TxPos: 0, Value: 993}
	if err := store.SaveUTXO(oldTip); err != nil {
		t.Fatalf("SaveUTXO: %v", err)
	}
	if err := store.SavePendingRecord(PendingBroadcast{
		TxID:    otherTestTxID,
		EF:      []byte{1, 2, 3},
		Spent:   []UTXO{oldTip},
		Created: []UTXO{newTip},
		Stage:   "l2_done",
	}); err != nil {
		t.Fatalf("SavePendingRecord: %v", err)
	}

	q := newQueue()
	q.PushMemory(oldTip)
	e := newEngine(q, &arcadeClient{store: store}, testLockScript())

	utxos, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(utxos) != 1 || utxos[0].TxHash != newTip.TxHash || utxos[0].Value != newTip.Value {
		t.Fatalf("replayed UTXOs = %+v, want only %+v", utxos, newTip)
	}
	pending, err := store.LoadPending()
	if err != nil {
		t.Fatalf("LoadPending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after replay = %+v, want empty", pending)
	}
	if got := e.Snapshot().BootstrapStage; got != "l2_done" {
		t.Fatalf("bootstrap stage = %q, want l2_done", got)
	}
	if got := q.Len(); got != 1 {
		t.Fatalf("queue len = %d, want 1", got)
	}
	u, ok := q.PopMemory()
	if !ok || u.TxHash != newTip.TxHash {
		t.Fatalf("queue tip = %+v ok=%v, want %+v", u, ok, newTip)
	}
}

func TestNewEngineRestoresBootstrapStage(t *testing.T) {
	store := newTestStore(t)
	if err := store.SetMeta("bootstrap_stage", "l2_done"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if err := store.SaveUTXO(UTXO{TxHash: testTxID, TxPos: 0, Value: 1_000}); err != nil {
		t.Fatalf("SaveUTXO: %v", err)
	}

	e := newEngine(newQueue(), &arcadeClient{store: store}, testLockScript())
	if got := e.Snapshot().BootstrapStage; got != "l2_done" {
		t.Fatalf("bootstrap stage = %q, want l2_done", got)
	}
}

func TestEngineBroadcastPassesTxIDWhenSupported(t *testing.T) {
	fb := &fakeKnownTxBroadcaster{}
	e := newTestEngine(fb)

	if err := e.broadcastTx(context.Background(), testTxID, []byte{1, 2, 3}); err != nil {
		t.Fatalf("broadcastTx: %v", err)
	}
	if fb.txid != testTxID {
		t.Fatalf("txid = %q, want %q", fb.txid, testTxID)
	}
	if fb.kind != "tx" {
		t.Fatalf("kind = %q, want tx", fb.kind)
	}
}

func TestBootstrapL2DoesNotRequireWoCReconcile(t *testing.T) {
	e := newTestEngine(&fakeBroadcaster{})
	e.queue.Push(UTXO{TxHash: testTxID, TxPos: 0, Value: 5_000})
	e.queue.Push(UTXO{TxHash: testTxID, TxPos: 1, Value: 5_000})
	e.SetUTXOSource(fakeUTXOSource{utxos: []UTXO{
		{TxHash: otherTestTxID, TxPos: 0, Value: 1},
	}})

	if err := e.bootstrapL2(context.Background()); err != nil {
		t.Fatalf("bootstrapL2: %v", err)
	}
	if got := e.Snapshot().BootstrapStage; got != "l2_done" {
		t.Fatalf("bootstrap stage = %q, want l2_done", got)
	}
	if got := e.queue.Len(); got != e.numChains {
		t.Fatalf("queue len = %d, want %d", got, e.numChains)
	}
}

func TestArcadeTxStatusEndpoint(t *testing.T) {
	arcadeHTTP := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/tx/"+testTxID {
			t.Fatalf("request = %s %s, want GET /tx/%s", r.Method, r.URL.Path, testTxID)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"txid":"` + testTxID + `","txStatus":"SEEN_ON_NETWORK"}`)),
		}, nil
	})}

	e := newTestEngine(&fakeBroadcaster{})
	e.arcade = &arcadeClient{
		base:        "http://arcade.test",
		http:        arcadeHTTP,
		concurrency: make(chan struct{}, 1),
		retryMax:    1,
		logger:      slog.Default(),
	}
	s := newServerWithConfig(e, &Config{AdminToken: "secret"}, nil, slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/arcade/tx/"+testTxID, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if got := rr.Body.String(); got != `{"txid":"`+testTxID+`","txStatus":"SEEN_ON_NETWORK"}` {
		t.Fatalf("body = %s", got)
	}
}

func TestArcadeBroadcastSetsCallbackToken(t *testing.T) {
	arcadeHTTP := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got, want := r.Header.Get("X-CallbackToken"), "callback-secret"; got != want {
			t.Fatalf("X-CallbackToken = %q, want %q", got, want)
		}
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"status":"submitted"}`)),
		}, nil
	})}
	c := &arcadeClient{
		base:          "http://arcade.test",
		http:          arcadeHTTP,
		concurrency:   make(chan struct{}, 1),
		retryMax:      1,
		logger:        slog.Default(),
		callbackToken: "callback-secret",
	}

	if err := c.BroadcastKnownTx(context.Background(), "tx", testTxID, []byte{1, 2, 3}); err != nil {
		t.Fatalf("BroadcastKnownTx: %v", err)
	}
}

func TestConfigUsesConfiguredMaxTPS(t *testing.T) {
	e := newTestEngine(&fakeBroadcaster{})
	s := newServerWithConfig(e, &Config{AdminToken: "secret", MaxTPS: 5}, nil, slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/config", bytes.NewBufferString(`{"tps":6}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if got := e.TPS(); got != 0 {
		t.Fatalf("TPS after rejected request = %d, want 0", got)
	}

	req = httptest.NewRequest(http.MethodPost, "/config", bytes.NewBufferString(`{"tps":5}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if got := e.TPS(); got != 5 {
		t.Fatalf("TPS after accepted request = %d, want 5", got)
	}
}

func TestConfigUpdatesBootstrapParameters(t *testing.T) {
	e := newTestEngine(&fakeBroadcaster{})
	s := newServerWithConfig(e, &Config{AdminToken: "secret", NumChains: 4, FanoutSize: 2, SustainFee: sustainFee}, nil, slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/config", bytes.NewBufferString(`{"tps":2,"numChains":1200,"fanoutSize":100,"sustainFee":7}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if got := e.TPS(); got != 2 {
		t.Fatalf("TPS = %d, want 2", got)
	}
	cfg := e.bootstrapConfig()
	if cfg.NumChains != 1200 || cfg.FanoutSize != 100 || cfg.SustainFee != 7 {
		t.Fatalf("bootstrap config = %+v, want numChains=1200 fanoutSize=100 sustainFee=7", cfg)
	}
	if s.cfg.NumChains != 1200 || s.cfg.FanoutSize != 100 || s.cfg.SustainFee != 7 {
		t.Fatalf("server cfg = %+v, want updated bootstrap values", s.cfg)
	}
}

func TestConfigRejectsBootstrapParametersAfterFundingDetected(t *testing.T) {
	e := newTestEngine(&fakeBroadcaster{})
	e.queue.PushMemory(UTXO{TxHash: testTxID, TxPos: 0, Value: 10_000})
	s := newServerWithConfig(e, &Config{AdminToken: "secret"}, nil, slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/config", bytes.NewBufferString(`{"numChains":1200}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusConflict, rr.Body.String())
	}
	if got := e.bootstrapConfig().NumChains; got != 4 {
		t.Fatalf("numChains = %d, want unchanged 4", got)
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

func TestRunChainBroadcastsAfterInitialJitter(t *testing.T) {
	oldRandInt63n := randInt63n
	randInt63n = func(int64) int64 { return 0 }
	t.Cleanup(func() { randInt63n = oldRandInt63n })

	fb := &fakeBroadcaster{}
	e := newTestEngine(fb)
	e.chainsActiveCnt.Add(1)
	e.SetTPS(1)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		e.runChain(ctx, UTXO{TxHash: testTxID, TxPos: 0, Value: sustainFee})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("runChain did not broadcast after initial jitter")
	}
	if got := fb.count.Load(); got != 1 {
		t.Fatalf("broadcast count = %d, want 1", got)
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
