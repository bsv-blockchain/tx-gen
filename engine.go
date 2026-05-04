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

type knownTxBroadcaster interface {
	BroadcastKnownTx(ctx context.Context, kind, txid string, ef []byte) error
}

// UTXOSource fetches unspent outputs for a script hash.
// Implemented by wocClient; injectable for tests.
type UTXOSource interface {
	FetchUTXOs(ctx context.Context, scriptHash string) ([]UTXO, error)
}

var randInt63n = rand.Int63n

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
	txMode      *TxMode

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
	return newEngineWithMode(q, arcade, newTaggedDropTxMode(lockScript, sustainFee))
}

func newEngineWithMode(q *Queue, arcade *arcadeClient, mode *TxMode) *Engine {
	mode = normalizedTxMode(mode)
	var store *Store
	if arcade != nil {
		store = arcade.store
	}
	e := &Engine{
		queue:       q,
		arcade:      arcade,
		broadcaster: arcade,
		store:       store,
		lockScript:  mode.LockScript,
		txMode:      mode,
		resume:      newNotify(),
		numChains:   10_000,
		fanoutSize:  100,
	}
	e.state.Lifecycle = LifecycleStarting
	e.state.BootstrapStage = "none"
	e.state.ChainsTarget = 10_000
	if store != nil {
		if q.Len() == 0 {
			if err := q.Restore(store); err != nil {
				log.Printf("restore queue from store: %v", err)
			}
		} else {
			q.SetStore(store)
			if err := q.PersistAll(); err != nil {
				log.Printf("persist initial queue: %v", err)
			}
		}
		if changed, err := e.replayPending(); err != nil {
			log.Printf("replay pending broadcasts: %v", err)
		} else if changed {
			if err := q.Restore(store); err != nil {
				log.Printf("restore queue after pending replay: %v", err)
			}
		}
		if stage, err := store.GetMeta("bootstrap_stage"); err != nil {
			log.Printf("restore bootstrap stage: %v", err)
		} else if stage != "" {
			e.state.BootstrapStage = stage
			SetBootstrapStage(stage)
		}
	}
	return e
}

func (e *Engine) mode() *TxMode {
	if e != nil && e.txMode != nil {
		return normalizedTxMode(e.txMode)
	}
	if e != nil {
		return newTaggedDropTxMode(e.lockScript, sustainFee)
	}
	return newTaggedDropTxMode(mustBuildLockScript("tx-gen"), sustainFee)
}

func (e *Engine) configure(numChains, fanoutSize int) {
	if numChains <= 0 {
		numChains = 10_000
	}
	if fanoutSize <= 0 {
		fanoutSize = 100
	}
	e.mu.Lock()
	e.numChains = numChains
	e.fanoutSize = fanoutSize
	e.state.ChainsTarget = numChains
	e.mu.Unlock()
}

type BootstrapConfig struct {
	NumChains  int    `json:"numChains"`
	FanoutSize int    `json:"fanoutSize"`
	SustainFee uint64 `json:"sustainFee"`
}

func (e *Engine) bootstrapConfig() BootstrapConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()
	numChains := e.numChains
	if numChains <= 0 {
		numChains = 10_000
	}
	fanoutSize := e.fanoutSize
	if fanoutSize <= 0 {
		fanoutSize = 100
	}
	sustainFee := uint64(sustainFee)
	if e.txMode != nil && e.txMode.SustainFee > 0 {
		sustainFee = e.txMode.SustainFee
	}
	return BootstrapConfig{
		NumChains:  numChains,
		FanoutSize: fanoutSize,
		SustainFee: sustainFee,
	}
}

func (e *Engine) ConfigureBootstrap(numChains, fanoutSize *int, sustainFeeValue *uint64) (BootstrapConfig, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	queueLen := 0
	if e.queue != nil {
		queueLen = e.queue.Len()
	}
	lifecycle := e.state.Lifecycle
	if e.state.BootstrapStage != "none" || (lifecycle != LifecycleStarting && lifecycle != LifecycleBootstrapping) || queueLen != 0 || e.chainsActiveCnt.Load() != 0 {
		return BootstrapConfig{}, fmt.Errorf("bootstrap parameters can only be changed before initial funding is detected")
	}

	if numChains != nil {
		if *numChains <= 0 {
			return BootstrapConfig{}, fmt.Errorf("numChains must be positive")
		}
		e.numChains = *numChains
	}
	if fanoutSize != nil {
		if *fanoutSize <= 0 {
			return BootstrapConfig{}, fmt.Errorf("fanoutSize must be positive")
		}
		e.fanoutSize = *fanoutSize
	}
	if sustainFeeValue != nil {
		if *sustainFeeValue == 0 {
			return BootstrapConfig{}, fmt.Errorf("sustainFee must be positive")
		}
		mode := e.txMode
		if mode == nil {
			mode = newTaggedDropTxMode(e.lockScript, *sustainFeeValue)
			e.txMode = mode
		}
		mode.SustainFee = *sustainFeeValue
	}

	cfg := e.bootstrapConfigLocked()
	e.state.ChainsTarget = cfg.NumChains
	return cfg, nil
}

func (e *Engine) bootstrapConfigLocked() BootstrapConfig {
	numChains := e.numChains
	if numChains <= 0 {
		numChains = 10_000
	}
	fanoutSize := e.fanoutSize
	if fanoutSize <= 0 {
		fanoutSize = 100
	}
	fee := uint64(sustainFee)
	if e.txMode != nil && e.txMode.SustainFee > 0 {
		fee = e.txMode.SustainFee
	}
	return BootstrapConfig{NumChains: numChains, FanoutSize: fanoutSize, SustainFee: fee}
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

func (e *Engine) broadcastTx(ctx context.Context, txid string, efBytes []byte) error {
	if b, ok := e.broadcaster.(knownTxBroadcaster); ok {
		return b.BroadcastKnownTx(ctx, "tx", txid, efBytes)
	}
	return e.broadcaster.Broadcast(ctx, efBytes)
}

// run is the main engine entry point called from main.go via runSafe.
func (e *Engine) run(ctx context.Context) {
	var infraWG sync.WaitGroup
	SafeGo(&infraWG, "tps_tracker", func() { e.trackTPS(ctx) })

	qlen := e.queue.Len()
	log.Printf("startup: %d UTXOs", qlen)

	bootstrapCfg := e.bootstrapConfig()
	e.setState(func(s *EngineState) {
		s.Lifecycle = LifecycleBootstrapping
		s.ChainsTarget = bootstrapCfg.NumChains
	})

	var bootstrapErr error
	stage := e.Snapshot().BootstrapStage
	if stage == "none" && qlen == 0 {
		bootstrapErr = e.waitForSeedUTXO(ctx)
		qlen = e.queue.Len()
	}
	switch {
	case bootstrapErr != nil:
	case stage == "l2_done":
		log.Printf("resuming with %d existing UTXOs", qlen)
	case stage == "l1_done":
		log.Printf("bootstrap: L2 fanout from persisted stage (%d → %d)", qlen, e.bootstrapConfig().NumChains)
		bootstrapErr = e.bootstrapL2(ctx)
	case stage == "l2_partial":
		// WoC reconciliation removed; trust queue state from store.
		if qlen >= e.bootstrapConfig().NumChains {
			log.Printf("resuming with %d existing UTXOs (l2_partial → l2_done)", qlen)
			e.setBootstrapStage("l2_done")
		} else if qlen == e.l1FanoutSize() {
			log.Printf("bootstrap: L2 fanout from l2_partial (%d → %d)", qlen, e.bootstrapConfig().NumChains)
			bootstrapErr = e.bootstrapL2(ctx)
		} else if qlen > 0 {
			log.Printf("resuming with %d existing UTXOs (partial L2)", qlen)
			e.setBootstrapStage("l2_done")
		} else {
			bootstrapErr = fmt.Errorf("l2_partial with empty queue: re-seed required")
		}
	case stage == "unknown":
		bootstrapErr = fmt.Errorf("bootstrap stage %q requires manual recovery", stage)
	case qlen == 1:
		log.Printf("bootstrap: L1 + L2 fanout (1 → %d → %d)", e.l1FanoutSize(), e.bootstrapConfig().NumChains)
		bootstrapErr = e.bootstrapFull(ctx)
	case qlen == e.l1FanoutSize():
		log.Printf("bootstrap: L2 fanout only (%d → %d)", e.l1FanoutSize(), e.bootstrapConfig().NumChains)
		bootstrapErr = e.bootstrapL2(ctx)
	case qlen > 0:
		log.Printf("resuming with %d existing UTXOs", qlen)
		e.setBootstrapStage("l2_done")
	default:
		bootstrapErr = fmt.Errorf("no UTXOs available")
	}

	if bootstrapErr != nil {
		log.Printf("bootstrap error: %v", bootstrapErr)
		e.setState(func(s *EngineState) {
			s.Lifecycle = LifecycleFailed
			s.LastError = &ErrorRecord{Component: "bootstrap", Msg: bootstrapErr.Error(), Time: time.Now()}
		})
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

func (e *Engine) waitForSeedUTXO(ctx context.Context) error {
	if e.source == nil {
		return fmt.Errorf("no UTXOs available")
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		utxos, err := e.source.FetchUTXOs(ctx, scriptHash(e.lockScript))
		if err == nil {
			e.setState(func(s *EngineState) {
				s.LastWoCTime = time.Now()
				s.LastWoCResult = fmt.Sprintf("%d utxos", len(utxos))
			})
			if len(utxos) > 0 {
				for _, u := range utxos {
					e.queue.Push(u)
				}
				SetQueueDepth(e.queue.Len())
				log.Printf("seed UTXO detected: %d UTXOs", len(utxos))
				return nil
			}
		} else if ctx.Err() != nil {
			return ctx.Err()
		} else {
			e.setState(func(s *EngineState) {
				s.LastWoCTime = time.Now()
				s.LastWoCResult = "error"
				s.LastError = &ErrorRecord{Component: "woc", Msg: err.Error(), Time: time.Now()}
			})
			log.Printf("seed UTXO lookup: %v", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func ceilDivInt(n, d int) int {
	if d <= 0 {
		return 0
	}
	return (n + d - 1) / d
}

func (e *Engine) l1FanoutSize() int {
	cfg := e.bootstrapConfig()
	return ceilDivInt(cfg.NumChains, cfg.FanoutSize)
}

func l2FanoutSizeForParent(numChains, parentCount, parentIndex int) int {
	if numChains <= 0 || parentCount <= 0 || parentIndex < 0 || parentIndex >= parentCount {
		return 0
	}
	base := numChains / parentCount
	remainder := numChains % parentCount
	if parentIndex < remainder {
		return base + 1
	}
	return base
}

// bootstrapFull: L0 UTXO → L1 fanout → L2 fanout.
func (e *Engine) bootstrapFull(ctx context.Context) error {
	e.setBootstrapStage("none")

	utxo, ok := e.queue.PopMemory()
	if !ok {
		return fmt.Errorf("queue empty")
	}

	txid, efBytes, l1Outs, buildErr := buildFanoutTxWithMode(utxo, e.l1FanoutSize(), e.mode())
	if buildErr != nil {
		e.queue.PushMemory(utxo)
		return fmt.Errorf("build L1: %w", buildErr)
	}
	for i := range l1Outs {
		l1Outs[i].TxHash = txid
	}

	var broadcastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := e.savePendingRecord(PendingBroadcast{
			TxID:    txid,
			EF:      efBytes,
			Spent:   []UTXO{utxo},
			Created: l1Outs,
			Stage:   "l1_done",
		}); err != nil {
			e.queue.PushMemory(utxo)
			return fmt.Errorf("save L1 pending: %w", err)
		}
		if broadcastErr = e.broadcastTx(ctx, txid, efBytes); broadcastErr == nil {
			break
		}
		e.clearPending(txid)
		log.Printf("L1 broadcast attempt %d: %v", attempt, broadcastErr)
		if attempt < 3 {
			if err := sleepContext(ctx, retryBackoff(attempt)); err != nil {
				e.queue.PushMemory(utxo)
				return err
			}
		}
	}
	if broadcastErr != nil {
		e.clearPending(txid)
		e.queue.PushMemory(utxo)
		return fmt.Errorf("broadcast L1: %w", broadcastErr)
	}

	if err := e.queue.CommitPending(txid, []UTXO{utxo}, l1Outs); err != nil {
		return fmt.Errorf("commit L1 broadcast: %w", err)
	}
	e.recordBroadcastOk()
	e.setBootstrapStage("l1_done")

	for _, u := range l1Outs {
		e.queue.PushMemory(u)
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
	e.setBootstrapStage("l2_partial")

	var parents []UTXO
	for {
		u, ok := e.queue.PopMemory()
		if !ok {
			break
		}
		parents = append(parents, u)
	}
	if len(parents) == 0 {
		return fmt.Errorf("no L1 outputs to fan out")
	}
	targetChains := e.bootstrapConfig().NumChains
	if len(parents) > targetChains {
		for _, parent := range parents {
			e.queue.PushMemory(parent)
		}
		return fmt.Errorf("too many L1 outputs: got %d, want at most %d", len(parents), targetChains)
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
			fanoutSize := l2FanoutSizeForParent(targetChains, len(parents), i)
			txid, ef, outs, err := buildFanoutTxWithMode(parent, fanoutSize, e.mode())
			if err != nil {
				statuses[i].Store(int32(psFailed))
				results[i] = l2result{err: fmt.Errorf("build %s: %w", parent.TxHash, err)}
				e.setState(func(s *EngineState) { s.BootstrapFailures++ })
				e.queue.PushMemory(parent)
				return
			}
			for j := range outs {
				outs[j].TxHash = txid
			}

			statuses[i].Store(int32(psBroadcasting))
			if err := e.savePendingRecord(PendingBroadcast{
				TxID:    txid,
				EF:      ef,
				Spent:   []UTXO{parent},
				Created: outs,
				Stage:   "l2_partial",
			}); err != nil {
				statuses[i].Store(int32(psFailed))
				results[i] = l2result{err: fmt.Errorf("save pending %s: %w", parent.TxHash, err)}
				e.setState(func(s *EngineState) { s.BootstrapFailures++ })
				e.queue.PushMemory(parent)
				return
			}

			var bcastErr error
			for attempt := 1; attempt <= 3; attempt++ {
				if bcastErr = e.broadcastTx(ctx, txid, ef); bcastErr == nil {
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

			if bcastErr != nil {
				e.clearPending(txid)
				statuses[i].Store(int32(psFailed))
				results[i] = l2result{err: fmt.Errorf("broadcast %s: %w", parent.TxHash, bcastErr)}
				e.setState(func(s *EngineState) { s.BootstrapFailures++ })
				e.queue.PushMemory(parent)
				return
			}

			if err := e.queue.CommitPending(txid, []UTXO{parent}, outs); err != nil {
				statuses[i].Store(int32(psFailed))
				results[i] = l2result{err: fmt.Errorf("commit %s: %w", parent.TxHash, err)}
				e.setState(func(s *EngineState) { s.BootstrapFailures++ })
				return
			}
			e.recordBroadcastOk()
			statuses[i].Store(int32(psAccepted))
			results[i] = l2result{outputs: outs}
		})
	}
	wg.Wait()

	var failed int
	var expected []UTXO
	for i, r := range results {
		if statuses[i].Load() != int32(psAccepted) {
			failed++
			log.Printf("L2 parent %d failed: %v", i, r.err)
		} else {
			for _, out := range r.outputs {
				e.queue.PushMemory(out)
				expected = append(expected, out)
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
		e.setBootstrapStage("l2_partial")
		return fmt.Errorf("%s", msg)
	}
	if len(expected) != targetChains {
		msg := fmt.Sprintf("bootstrap produced %d L2 UTXOs, expected %d", len(expected), targetChains)
		e.setState(func(s *EngineState) {
			s.Lifecycle = LifecycleFailed
			s.LastError = &ErrorRecord{Component: "bootstrap_l2", Msg: msg, Time: time.Now()}
		})
		e.setBootstrapStage("l2_partial")
		return fmt.Errorf("%s", msg)
	}

	e.setBootstrapStage("l2_done")
	return nil
}

func (e *Engine) startChains(ctx context.Context) {
	bootstrapCfg := e.bootstrapConfig()
	e.setState(func(s *EngineState) {
		if e.tps.Load() > 0 {
			s.Lifecycle = LifecycleRunning
		} else {
			s.Lifecycle = LifecyclePaused
		}
		s.ChainsTarget = bootstrapCfg.NumChains
	})

	var chainWG sync.WaitGroup
	count := 0
	for {
		utxo, ok := e.queue.PopMemory()
		if !ok {
			break
		}
		u := utxo
		n := e.chainsActiveCnt.Add(1)
		SetChainsActive(int(n))
		SafeGo(&chainWG, fmt.Sprintf("chain_%d", count), func() {
			defer func() {
				remaining := e.chainsActiveCnt.Add(-1)
				SetChainsActive(int(remaining))
			}()
			e.runChain(ctx, u)
		})
		count++
	}
	log.Printf("started %d chains", count)
	SetQueueDepth(e.queue.Len())
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

func jitterDuration(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(randInt63n(int64(max) + 1))
}

func (e *Engine) waitDuration(ctx context.Context, d time.Duration) (elapsed bool, ok bool) {
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
	case <-e.resume.C():
		return false, true
	}
}

func (e *Engine) waitInitialJitter(ctx context.Context) bool {
	for {
		tps, ok := e.waitActive(ctx)
		if !ok {
			return false
		}
		elapsed, ok := e.waitDuration(ctx, jitterDuration(chainInterval(tps, e.chainsActiveCnt.Load())))
		if !ok {
			return false
		}
		if elapsed {
			return true
		}
	}
}

func (e *Engine) waitNextInterval(ctx context.Context) bool {
	for {
		tps, ok := e.waitActive(ctx)
		if !ok {
			return false
		}
		elapsed, ok := e.waitDuration(ctx, chainInterval(tps, e.chainsActiveCnt.Load()))
		if !ok {
			return false
		}
		if elapsed {
			return true
		}
	}
}

func (e *Engine) runChain(ctx context.Context, utxo UTXO) {
	if !e.waitInitialJitter(ctx) {
		return
	}

	for {
		mode := e.mode()
		txid, efBytes, newUTXO, err := buildSustainTxWithMode(utxo, mode)
		if err != nil {
			log.Printf("sustain build %s:%d: %v", utxo.TxHash, utxo.TxPos, err)
			if !e.waitNextInterval(ctx) {
				return
			}
			continue
		}
		newUTXO.TxHash = txid

		if err := e.savePendingRecord(PendingBroadcast{
			TxID:    txid,
			EF:      efBytes,
			Spent:   []UTXO{utxo},
			Created: []UTXO{newUTXO},
			Stage:   "l2_done",
		}); err != nil {
			e.recordBroadcastFail("store", err.Error())
			log.Printf("sustain pending %s:%d: %v", utxo.TxHash, utxo.TxPos, err)
			return
		}
		if err := e.broadcastTx(ctx, txid, efBytes); err != nil {
			e.clearPending(txid)
			e.recordBroadcastFail("arcade", err.Error())
			log.Printf("sustain broadcast %s:%d: %v — retry next tick", utxo.TxHash, utxo.TxPos, err)
			if !e.waitNextInterval(ctx) {
				return
			}
			continue
		}

		if e.queue != nil {
			if err := e.queue.CommitPending(txid, []UTXO{utxo}, []UTXO{newUTXO}); err != nil {
				e.recordBroadcastFail("store", err.Error())
				log.Printf("sustain commit %s:%d: %v", utxo.TxHash, utxo.TxPos, err)
				return
			}
		}
		e.recordBroadcastOk()
		if newUTXO.Value == 0 {
			e.chainsExhausted.Add(1)
			IncChainTerminated("exhausted")
			log.Printf("chain done → %s (chain length %d)", txid, utxo.Value/mode.SustainFee)
			return
		}
		utxo = newUTXO
		if !e.waitNextInterval(ctx) {
			return
		}
	}
}

// nil-safe store helpers.

func (e *Engine) setMeta(key, val string) {
	if e.store != nil {
		_ = e.store.SetMeta(key, val)
	}
}

func (e *Engine) setBootstrapStage(stage string) {
	e.setState(func(s *EngineState) { s.BootstrapStage = stage })
	e.setMeta("bootstrap_stage", stage)
	SetBootstrapStage(stage)
}

func (e *Engine) savePendingRecord(p PendingBroadcast) error {
	if e.store != nil {
		return e.store.SavePendingRecord(p)
	}
	return nil
}

func (e *Engine) clearPending(txid string) {
	if e.store != nil {
		_ = e.store.ClearPending(txid)
	}
}

func (e *Engine) replayPending() (bool, error) {
	if e.store == nil {
		return false, nil
	}
	pend, err := e.store.LoadPending()
	if err != nil {
		return false, err
	}
	var changed bool
	var unknown int
	for txid, p := range pend {
		if len(p.Spent) == 0 && len(p.Created) == 0 {
			unknown++
			continue
		}
		if err := e.store.CommitPending(txid, p.Spent, p.Created); err != nil {
			return changed, err
		}
		changed = true
		if p.Stage != "" {
			e.setBootstrapStage(p.Stage)
		}
	}
	if unknown > 0 {
		msg := fmt.Sprintf("%d pending broadcasts require WoC reconciliation", unknown)
		e.setState(func(s *EngineState) {
			s.BootstrapStage = "unknown"
			s.LastError = &ErrorRecord{Component: "pending_replay", Msg: msg, Time: time.Now()}
		})
		e.setMeta("bootstrap_stage", "unknown")
		SetBootstrapStage("unknown")
	}
	return changed, nil
}
