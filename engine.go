package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
)

// Broadcaster sends an EF-encoded transaction to the network.
// Implemented by arcadeClient; injectable for tests.
type Broadcaster interface {
	Broadcast(ctx context.Context, ef []byte) error
}

// UTXOSource fetches unspent outputs for a script hash.
// Implemented by wocClient; injectable for tests.
type UTXOSource interface {
	FetchUTXOs(ctx context.Context, scriptHash string) ([]UTXO, error)
}

// Lifecycle represents the engine operating state.
type Lifecycle string

const (
	LifecycleStarting      Lifecycle = "starting"
	LifecycleBootstrapping Lifecycle = "bootstrapping"
	LifecyclePaused        Lifecycle = "paused"
	LifecycleRunning       Lifecycle = "running"
	LifecycleDegraded      Lifecycle = "degraded"
	LifecycleStopping      Lifecycle = "stopping"
	LifecycleStopped       Lifecycle = "stopped"
	LifecycleFailed        Lifecycle = "failed"
)

// ReorgRecord is the engine's internal view of a reorg event.
// Named "Record" to avoid conflict with sse.go's ReorgEvent wire type.
type ReorgRecord struct {
	Height         int       `json:"height"`
	Depth          int       `json:"depth"`
	Time           time.Time `json:"time"`
	ChainsImpacted int       `json:"chainsImpacted"`
	TipBefore      string    `json:"tipBefore"`
	TipAfter       string    `json:"tipAfter"`
}

// ErrorRecord captures the last significant engine error.
type ErrorRecord struct {
	Component string    `json:"component"`
	Msg       string    `json:"msg"`
	Time      time.Time `json:"time"`
}

// SSEState holds the last-known chain tip from SSE streams.
type SSEState struct {
	Connected bool      `json:"connected"`
	TipHeight int       `json:"tipHeight"`
	TipHash   string    `json:"tipHash"`
	TipTime   time.Time `json:"tipTime"`
}

// EngineState is a point-in-time snapshot of engine health.
// Field names match server.go's firstString/firstNumber key lookups.
type EngineState struct {
	Lifecycle                 Lifecycle    `json:"lifecycle"`
	RequestedTPS              int64        `json:"requestedTPS"`
	AcceptedTPS               float64      `json:"acceptedTPS"`
	ChainsActive              int          `json:"chainsActive"`
	ChainsTarget              int          `json:"chainsTarget"`
	ChainsExhausted           int          `json:"chainsExhausted"`
	BootstrapStage            string       `json:"bootstrapStage"`
	BootstrapFailures         int          `json:"bootstrapFailures"`
	LastBroadcastOk           time.Time    `json:"lastBroadcastOk"`
	ConsecutiveArcadeFailures int          `json:"consecutiveArcadeFailures"`
	LastWoCTime               time.Time    `json:"lastWoCTime"`
	LastWoCResult             string       `json:"lastWoCResult"`
	SSE                       SSEState     `json:"sse"`
	LastReorg                 *ReorgRecord `json:"lastReorg,omitempty"`
	LastError                 *ErrorRecord `json:"lastError,omitempty"`
}

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
	queue       *Queue
	arcade      *arcadeClient // retained for server.go arcadeReachable check
	broadcaster Broadcaster
	source      UTXOSource // optional; inject via SetUTXOSource for WoC reconciliation
	store       *Store
	lockScript  *script.Script

	// Overridable by tests (defaults set in newEngine).
	numChains  int
	fanoutSize int

	tps    atomic.Int64
	resume *notify

	mu              sync.RWMutex
	state           EngineState
	chainsActiveCnt atomic.Int64
	chainsExhausted atomic.Int64

	// TPS tracking: reset every second by trackTPS goroutine.
	acceptedCount atomic.Int64
	acceptedTPS   atomic.Value // stores float64
}

func newEngine(q *Queue, arcade *arcadeClient, lockScript *script.Script) *Engine {
	e := &Engine{
		queue:       q,
		arcade:      arcade,
		broadcaster: arcade,
		store:       arcade.store,
		lockScript:  lockScript,
		resume:      newNotify(),
		numChains:   10_000,
		fanoutSize:  100,
	}
	e.state.Lifecycle = LifecycleStarting
	e.state.BootstrapStage = "none"
	e.state.ChainsTarget = 10_000
	if arcade.store != nil {
		if err := q.Restore(arcade.store); err != nil {
			log.Printf("restore queue from store: %v", err)
		}
	}
	return e
}

// SetUTXOSource wires in a UTXOSource (WoC or mock) for bootstrap reconciliation.
func (e *Engine) SetUTXOSource(s UTXOSource) {
	e.mu.Lock()
	e.source = s
	e.mu.Unlock()
}

// SetTPS sets the target TPS and immediately wakes all sleeping chains.
func (e *Engine) SetTPS(tps int64) {
	e.tps.Store(tps)
	e.mu.Lock()
	e.state.RequestedTPS = tps
	if tps > 0 {
		if e.state.Lifecycle == LifecyclePaused {
			e.state.Lifecycle = LifecycleRunning
		}
	} else {
		if e.state.Lifecycle == LifecycleRunning || e.state.Lifecycle == LifecycleDegraded {
			e.state.Lifecycle = LifecyclePaused
		}
	}
	e.mu.Unlock()
	// Always wake — chains re-read tps immediately regardless of direction.
	e.resume.Broadcast()
}

func (e *Engine) TPS() int64 { return e.tps.Load() }

// setState applies fn to EngineState under the write lock.
func (e *Engine) setState(fn func(*EngineState)) {
	e.mu.Lock()
	fn(&e.state)
	e.mu.Unlock()
}

// Snapshot returns a copy of the current EngineState.
// Called via reflection by server.go (must have 0 args, 1 return value).
func (e *Engine) Snapshot() EngineState {
	e.mu.RLock()
	s := e.state
	e.mu.RUnlock()
	s.ChainsActive = int(e.chainsActiveCnt.Load())
	s.ChainsExhausted = int(e.chainsExhausted.Load())
	s.RequestedTPS = e.tps.Load()
	if v := e.acceptedTPS.Load(); v != nil {
		s.AcceptedTPS = v.(float64)
	}
	return s
}

// RecordReorg is called by sse.go via reflection on reorg events.
// Signature must match: callEngineMethod(engine, "RecordReorg", height, depth, tipBefore, tipAfter)
func (e *Engine) RecordReorg(height, depth int, tipBefore, tipAfter string) {
	r := &ReorgRecord{
		Height:         height,
		Depth:          depth,
		Time:           time.Now(),
		ChainsImpacted: int(e.chainsActiveCnt.Load()),
		TipBefore:      tipBefore,
		TipAfter:       tipAfter,
	}
	e.setState(func(s *EngineState) { s.LastReorg = r })
	IncReorg(depth)
}

// RecordTip is called by sse.go via reflection on tip events.
func (e *Engine) RecordTip(height int, hash string, t time.Time) {
	e.setState(func(s *EngineState) {
		s.SSE.Connected = true
		s.SSE.TipHeight = height
		s.SSE.TipHash = hash
		s.SSE.TipTime = t
	})
}

// UpdateTip is an alias for RecordTip; sse.go calls both names.
func (e *Engine) UpdateTip(height int, hash string, t time.Time) {
	e.RecordTip(height, hash, t)
}

func (e *Engine) recordBroadcastOk() {
	e.setState(func(s *EngineState) {
		s.LastBroadcastOk = time.Now()
		s.ConsecutiveArcadeFailures = 0
		if s.Lifecycle == LifecycleDegraded {
			s.Lifecycle = LifecycleRunning
		}
	})
	e.acceptedCount.Add(1)
}

func (e *Engine) recordBroadcastFail(component, msg string) {
	e.setState(func(s *EngineState) {
		s.ConsecutiveArcadeFailures++
		s.LastError = &ErrorRecord{Component: component, Msg: msg, Time: time.Now()}
		if s.ConsecutiveArcadeFailures >= 5 {
			s.Lifecycle = LifecycleDegraded
		}
	})
}

// run is the main engine entry point called from main.go via runSafe.
func (e *Engine) run(ctx context.Context) {
	var infraWG sync.WaitGroup
	SafeGo(&infraWG, "tps_tracker", func() { e.trackTPS(ctx) })

	qlen := e.queue.Len()
	log.Printf("startup: %d UTXOs", qlen)

	e.setState(func(s *EngineState) {
		s.Lifecycle = LifecycleBootstrapping
		s.ChainsTarget = e.numChains
	})

	var bootstrapErr error
	switch {
	case qlen == 1:
		log.Printf("bootstrap: L1 + L2 fanout (1 → %d → %d)", e.fanoutSize, e.numChains)
		bootstrapErr = e.bootstrapFull(ctx)
	case qlen == e.fanoutSize:
		log.Printf("bootstrap: L2 fanout only (%d → %d)", e.fanoutSize, e.numChains)
		bootstrapErr = e.bootstrapL2(ctx)
	default:
		log.Printf("resuming with %d existing UTXOs", qlen)
	}

	if bootstrapErr != nil {
		log.Printf("bootstrap error: %v", bootstrapErr)
		e.setState(func(s *EngineState) {
			s.Lifecycle = LifecycleFailed
			s.LastError = &ErrorRecord{Component: "bootstrap", Msg: bootstrapErr.Error(), Time: time.Now()}
		})
		infraWG.Wait()
		return
	}

	e.startChains(ctx)
	infraWG.Wait()
}

// trackTPS samples accepted broadcast count each second for AcceptedTPS reporting.
func (e *Engine) trackTPS(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n := e.acceptedCount.Swap(0)
			e.acceptedTPS.Store(float64(n))
		}
	}
}

// bootstrapFull: L0 UTXO → L1 fanout → L2 fanout.
func (e *Engine) bootstrapFull(ctx context.Context) error {
	e.setState(func(s *EngineState) { s.BootstrapStage = "none" })
	e.setMeta("bootstrap_stage", "none")
	SetBootstrapStage("none")

	utxo, ok := e.queue.Pop()
	if !ok {
		return fmt.Errorf("queue empty")
	}

	txid, efBytes, l1Outs, buildErr := buildFanoutTx(utxo, e.fanoutSize, e.lockScript)
	if buildErr != nil {
		e.queue.Push(utxo)
		return fmt.Errorf("build L1: %w", buildErr)
	}

	var broadcastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		e.savePending(txid, efBytes)
		if broadcastErr = e.broadcaster.Broadcast(ctx, efBytes); broadcastErr == nil {
			break
		}
		e.clearPending(txid)
		log.Printf("L1 broadcast attempt %d: %v", attempt, broadcastErr)
		if attempt < 3 {
			if err := sleepContext(ctx, retryBackoff(attempt)); err != nil {
				e.queue.Push(utxo)
				return err
			}
		}
	}
	if broadcastErr != nil {
		e.clearPending(txid)
		e.queue.Push(utxo)
		return fmt.Errorf("broadcast L1: %w", broadcastErr)
	}

	e.clearPending(txid)
	e.recordBroadcastOk()
	for i := range l1Outs {
		l1Outs[i].TxHash = txid
	}
	log.Printf("L1 tx: %s (%d outputs @ %d sats each)", txid, e.fanoutSize, l1Outs[0].Value)

	e.setState(func(s *EngineState) { s.BootstrapStage = "l1_done" })
	e.setMeta("bootstrap_stage", "l1_done")
	SetBootstrapStage("l1_done")

	for _, u := range l1Outs {
		e.queue.Push(u)
	}
	return e.bootstrapL2(ctx)
}

// parentStatus tracks per-parent L2 bootstrap progress.
type parentStatus int32

const (
	psPending      parentStatus = iota
	psBuilding                  // nolint: unused
	psBroadcasting              // nolint: unused
	psAccepted
	psFailed // nolint: unused
)

// bootstrapL2 fans out all queued L1 outputs to create numChains leaf UTXOs.
func (e *Engine) bootstrapL2(ctx context.Context) error {
	var parents []UTXO
	for {
		u, ok := e.queue.Pop()
		if !ok {
			break
		}
		parents = append(parents, u)
	}
	if len(parents) == 0 {
		return fmt.Errorf("no L1 outputs to fan out")
	}

	type l2result struct {
		outputs []UTXO
		err     error
	}
	results := make([]l2result, len(parents))
	statuses := make([]atomic.Int32, len(parents))

	var wg sync.WaitGroup
	for idx, p := range parents {
		i, parent := idx, p
		statuses[i].Store(int32(psPending))
		SafeGo(&wg, fmt.Sprintf("l2_%d", i), func() {
			statuses[i].Store(int32(psBuilding))
			txid, ef, outs, err := buildFanoutTx(parent, e.fanoutSize, e.lockScript)
			if err != nil {
				statuses[i].Store(int32(psFailed))
				results[i] = l2result{err: fmt.Errorf("build %s: %w", parent.TxHash, err)}
				e.setState(func(s *EngineState) { s.BootstrapFailures++ })
				return
			}

			statuses[i].Store(int32(psBroadcasting))
			e.savePending(txid, ef)

			var bcastErr error
			for attempt := 1; attempt <= 3; attempt++ {
				if bcastErr = e.broadcaster.Broadcast(ctx, ef); bcastErr == nil {
					break
				}
				log.Printf("L2 broadcast attempt %d for %s: %v", attempt, parent.TxHash, bcastErr)
				if attempt < 3 {
					if err := sleepContext(ctx, retryBackoff(attempt)); err != nil {
						bcastErr = err
						break
					}
				}
			}

			e.clearPending(txid)
			if bcastErr != nil {
				statuses[i].Store(int32(psFailed))
				results[i] = l2result{err: fmt.Errorf("broadcast %s: %w", parent.TxHash, bcastErr)}
				e.setState(func(s *EngineState) { s.BootstrapFailures++ })
				return
			}

			e.recordBroadcastOk()
			for j := range outs {
				outs[j].TxHash = txid
			}
			statuses[i].Store(int32(psAccepted))
			results[i] = l2result{outputs: outs}
		})
	}
	wg.Wait()

	var failed int
	for i, r := range results {
		if statuses[i].Load() != int32(psAccepted) {
			failed++
			log.Printf("L2 parent %d failed: %v", i, r.err)
		} else {
			for _, out := range r.outputs {
				e.queue.Push(out)
			}
		}
	}

	log.Printf("bootstrap L2 complete: %d UTXOs ready (%d parents failed)", e.queue.Len(), failed)

	if failed > 0 {
		msg := fmt.Sprintf("%d/%d L2 fanouts failed", failed, len(parents))
		e.setState(func(s *EngineState) {
			s.Lifecycle = LifecycleFailed
			s.LastError = &ErrorRecord{Component: "bootstrap_l2", Msg: msg, Time: time.Now()}
		})
		e.setMeta("bootstrap_stage", "l2_partial")
		return fmt.Errorf("%s", msg)
	}

	e.setState(func(s *EngineState) { s.BootstrapStage = "l2_done" })
	e.setMeta("bootstrap_stage", "l2_done")
	SetBootstrapStage("l2_done")
	return nil
}

func (e *Engine) startChains(ctx context.Context) {
	e.setState(func(s *EngineState) {
		s.Lifecycle = LifecycleRunning
		s.ChainsTarget = e.numChains
	})

	var chainWG sync.WaitGroup
	count := 0
	for {
		utxo, ok := e.queue.Pop()
		if !ok {
			break
		}
		u := utxo
		n := e.chainsActiveCnt.Add(1)
		SetChainsActive(int(n))
		SafeGo(&chainWG, fmt.Sprintf("chain_%d", count), func() {
			defer func() {
				remaining := e.chainsActiveCnt.Add(-1)
				e.chainsExhausted.Add(1)
				SetChainsActive(int(remaining))
			}()
			e.runChain(ctx, u)
		})
		count++
	}
	log.Printf("started %d chains", count)
	<-ctx.Done()
	e.setState(func(s *EngineState) { s.Lifecycle = LifecycleStopped })
	chainWG.Wait()
}

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

// chainInterval computes per-chain tick duration from actual active chain count.
func chainInterval(tps, chains int64) time.Duration {
	if chains <= 0 || tps <= 0 {
		return time.Second
	}
	return time.Duration(float64(chains) / float64(tps) * float64(time.Second))
}

func (e *Engine) runChain(ctx context.Context, utxo UTXO) {
	tps, ok := e.waitActive(ctx)
	if !ok {
		return
	}
	chains := e.chainsActiveCnt.Load()
	jitter := time.Duration(rand.Int63n(int64(chainInterval(tps, chains)) + 1))
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
		chains := e.chainsActiveCnt.Load()

		select {
		case <-ctx.Done():
			return
		case <-time.After(chainInterval(tps, chains)):
		case <-e.resume.C():
			// TPS changed — skip this tick, re-read the new rate.
			continue
		}

		txid, efBytes, newUTXO, err := buildSustainTx(utxo, e.lockScript)
		if err != nil {
			log.Printf("sustain build %s:%d: %v", utxo.TxHash, utxo.TxPos, err)
			continue
		}

		e.savePending(txid, efBytes)
		if err := e.broadcaster.Broadcast(ctx, efBytes); err != nil {
			e.clearPending(txid)
			e.recordBroadcastFail("arcade", err.Error())
			log.Printf("sustain broadcast %s:%d: %v — retry next tick", utxo.TxHash, utxo.TxPos, err)
			continue
		}
		e.clearPending(txid)
		e.recordBroadcastOk()

		newUTXO.TxHash = txid
		if newUTXO.Value == 0 {
			log.Printf("chain done → %s (chain length %d)", txid, utxo.Value/sustainFee)
			return
		}
		utxo = newUTXO
	}
}

// nil-safe store helpers.

func (e *Engine) setMeta(key, val string) {
	if e.store != nil {
		_ = e.store.SetMeta(key, val)
	}
}

func (e *Engine) savePending(txid string, ef []byte) {
	if e.store != nil {
		_ = e.store.SavePending(txid, ef)
	}
}

func (e *Engine) clearPending(txid string) {
	if e.store != nil {
		_ = e.store.ClearPending(txid)
	}
}
