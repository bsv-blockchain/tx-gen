// Package engine drives tx-gen's transaction chains.
//
// On startup the engine bootstraps from a single funded UTXO: an L1 fanout
// splits it into ceil(NumChains/FanoutSize) outputs, and parallel L2 fanouts
// split those into NumChains chain UTXOs. Each chain then broadcasts one
// sustain transaction per NumChains/TPS seconds, spending its previous output,
// until the output can no longer pay the fee.
//
// Every broadcast is first written to the store as a pending record and
// committed once Arcade accepts it, so a crash at any point resumes from the
// last accepted chain tips without double-spending.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/tx-gen/internal/backoff"
	"github.com/bsv-blockchain/tx-gen/internal/metrics"
	"github.com/bsv-blockchain/tx-gen/internal/safe"
	"github.com/bsv-blockchain/tx-gen/internal/store"
	"github.com/bsv-blockchain/tx-gen/internal/txbuilder"
)

const (
	// DefaultNumChains is the number of chains when Options.NumChains is 0.
	DefaultNumChains = 10_000

	// DefaultFanoutSize is the fanout width when Options.FanoutSize is 0.
	DefaultFanoutSize = 100

	// DefaultSeedPollInterval is how often WhatsOnChain is polled for the
	// funding UTXO when Options.SeedPollInterval is 0.
	DefaultSeedPollInterval = 10 * time.Second

	// degradedAfterFailures is the number of consecutive broadcast failures
	// that moves the engine to LifecycleDegraded.
	degradedAfterFailures = 5

	// bootstrapAttempts is the number of broadcast attempts per fanout.
	bootstrapAttempts = 3
)

// Broadcaster sends an EF-encoded transaction to the network.
type Broadcaster interface {
	// Broadcast submits ef, whose transaction id is txid. It returns nil
	// once the network has accepted the transaction.
	Broadcast(ctx context.Context, txid string, ef []byte) error
}

// UTXOSource looks up unspent outputs by script hash.
type UTXOSource interface {
	// FetchUTXOs returns the unspent outputs locked by the script whose
	// WhatsOnChain script hash is scriptHash.
	FetchUTXOs(ctx context.Context, scriptHash string) ([]txbuilder.UTXO, error)
}

// Options configures an Engine. Mode and Broadcaster are required.
type Options struct {
	// Mode locks and unlocks every generated output.
	Mode *txbuilder.Mode

	// Broadcaster submits transactions.
	Broadcaster Broadcaster

	// Source, when set, is polled for the funding UTXO while the queue is
	// empty and bootstrap has not started.
	Source UTXOSource

	// Store persists chain tips, pending broadcasts, and the bootstrap stage.
	// A nil Store keeps everything in memory.
	Store *store.Store

	// InitialUTXOs seeds the queue (and is saved to Store). When empty, the
	// queue is restored from Store instead.
	InitialUTXOs []txbuilder.UTXO

	// NumChains is the number of chains bootstrap creates.
	NumChains int

	// FanoutSize is the maximum outputs per bootstrap fanout transaction.
	FanoutSize int

	// SeedPollInterval is how often Source is polled for the funding UTXO.
	SeedPollInterval time.Duration

	// Metrics receives engine metrics. It may be nil.
	Metrics *metrics.Metrics

	// Logger receives engine logs. A nil Logger uses slog.Default.
	Logger *slog.Logger

	// Runner starts the engine's goroutines. Its zero value logs panics only.
	Runner safe.Runner
}

// BootstrapConfig is the effective bootstrap configuration.
type BootstrapConfig struct {
	// NumChains is the number of chains bootstrap creates.
	NumChains int `json:"numChains"`

	// FanoutSize is the maximum outputs per bootstrap fanout transaction.
	FanoutSize int `json:"fanoutSize"`

	// SustainFee is the fee per sustain hop in satoshis.
	SustainFee uint64 `json:"sustainFee"`
}

// BootstrapUpdate changes bootstrap parameters before funding is detected.
// A nil field is left unchanged.
type BootstrapUpdate struct {
	// NumChains, if set, must be positive.
	NumChains *int

	// FanoutSize, if set, must be positive.
	FanoutSize *int

	// SustainFee, if set, must be positive.
	SustainFee *uint64
}

// Engine runs the bootstrap and the chains. Create one with New.
type Engine struct {
	queue        *queue
	broadcaster  Broadcaster
	source       UTXOSource
	store        *store.Store
	metrics      *metrics.Metrics
	logger       *slog.Logger
	runner       safe.Runner
	seedInterval time.Duration

	// jitter returns the random delay before a chain's first broadcast;
	// tests replace it to make timing deterministic.
	jitter func(maxJitter time.Duration) time.Duration

	tps    atomic.Int64
	resume *notify

	// mu guards state and the bootstrap parameters below it.
	mu         sync.RWMutex
	state      State
	mode       *txbuilder.Mode
	numChains  int
	fanoutSize int

	// bootstrapLocked is set once bootstrap starts spending the funding
	// UTXO; from then on the bootstrap parameters are fixed.
	bootstrapLocked bool

	chainsActive    atomic.Int64
	chainsExhausted atomic.Int64

	// acceptedCount counts accepted broadcasts in the current second;
	// acceptedTPS holds the count of the previous second.
	acceptedCount atomic.Int64
	acceptedTPS   atomic.Int64
}

// New creates an engine and restores its persisted state: it replays any
// pending broadcasts left by a crash, and resumes the saved bootstrap stage.
func New(opts Options) (*Engine, error) {
	if opts.Mode == nil {
		return nil, ErrModeRequired
	}
	if opts.Broadcaster == nil {
		return nil, ErrBroadcasterRequired
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	runner := opts.Runner
	if runner.Logger == nil {
		runner.Logger = logger
	}

	e := &Engine{
		queue:        newQueue(opts.Store),
		broadcaster:  opts.Broadcaster,
		source:       opts.Source,
		store:        opts.Store,
		metrics:      opts.Metrics,
		logger:       logger,
		runner:       runner,
		seedInterval: positiveOr(opts.SeedPollInterval, DefaultSeedPollInterval),
		jitter:       backoff.Jitter,
		resume:       newNotify(),
		mode:         opts.Mode,
		numChains:    positiveOr(opts.NumChains, DefaultNumChains),
		fanoutSize:   positiveOr(opts.FanoutSize, DefaultFanoutSize),
	}
	e.state.Lifecycle = LifecycleStarting
	e.state.BootstrapStage = StageNone
	e.state.ChainsTarget = e.numChains

	if err := e.restore(opts.InitialUTXOs); err != nil {
		return nil, err
	}
	e.metrics.SetQueueDepth(e.queue.len())
	return e, nil
}

// Run bootstraps the chains if needed and then runs them until ctx is
// canceled. It returns the bootstrap error, if any, leaving the engine in
// LifecycleFailed; it returns nil after a clean shutdown.
func (e *Engine) Run(ctx context.Context) error {
	trackCtx, stopTracking := context.WithCancel(ctx)
	var infra sync.WaitGroup
	e.runner.Go(&infra, "tps_tracker", func() { e.trackTPS(trackCtx) })
	defer func() {
		stopTracking()
		infra.Wait()
		e.acceptedTPS.Store(0)
	}()

	if err := e.bootstrap(ctx); err != nil {
		if ctx.Err() != nil {
			e.logger.Info("bootstrap interrupted by shutdown")
			e.setState(func(s *State) { s.Lifecycle = LifecycleStopped })
			return err
		}
		e.logger.Error("bootstrap failed", "error", err)
		e.setState(func(s *State) {
			s.Lifecycle = LifecycleFailed
			s.LastError = &ErrorRecord{Component: "bootstrap", Msg: err.Error(), Time: time.Now()}
		})
		return err
	}
	e.runChains(ctx)
	return nil
}

// SetTPS sets the target transactions per second and immediately wakes every
// waiting chain, so the new rate applies without waiting out old intervals.
// A TPS of 0 pauses all chains without losing state.
func (e *Engine) SetTPS(tps int64) {
	e.tps.Store(tps)
	e.metrics.SetTPSTarget(tps)
	e.setState(func(s *State) {
		s.RequestedTPS = tps
		switch {
		case tps > 0 && s.Lifecycle == LifecyclePaused:
			s.Lifecycle = LifecycleRunning
		case tps <= 0 && (s.Lifecycle == LifecycleRunning || s.Lifecycle == LifecycleDegraded):
			s.Lifecycle = LifecyclePaused
		}
	})
	e.resume.Broadcast()
}

// TPS returns the target transactions per second.
func (e *Engine) TPS() int64 {
	return e.tps.Load()
}

// Snapshot returns a copy of the current engine state.
func (e *Engine) Snapshot() State {
	e.mu.RLock()
	s := e.state
	e.mu.RUnlock()
	s.ChainsActive = int(e.chainsActive.Load())
	s.ChainsExhausted = int(e.chainsExhausted.Load())
	s.RequestedTPS = e.tps.Load()
	s.AcceptedTPS = float64(e.acceptedTPS.Load())
	return s
}

// BootstrapConfig returns the effective bootstrap configuration.
func (e *Engine) BootstrapConfig() BootstrapConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.bootstrapConfigLocked()
}

// ConfigureBootstrap applies update and returns the resulting configuration.
//
// Bootstrap parameters can change only while the engine is still waiting for
// its funding UTXO; once bootstrap starts it returns ErrBootstrapLocked. Every
// field is validated before any is applied.
func (e *Engine) ConfigureBootstrap(update BootstrapUpdate) (BootstrapConfig, error) {
	switch {
	case update.NumChains != nil && *update.NumChains <= 0:
		return BootstrapConfig{}, fmt.Errorf("%w: numChains must be positive", ErrInvalidBootstrap)
	case update.FanoutSize != nil && *update.FanoutSize <= 0:
		return BootstrapConfig{}, fmt.Errorf("%w: fanoutSize must be positive", ErrInvalidBootstrap)
	case update.SustainFee != nil && *update.SustainFee == 0:
		return BootstrapConfig{}, fmt.Errorf("%w: sustainFee must be positive", ErrInvalidBootstrap)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	lifecycle := e.state.Lifecycle
	funded := e.bootstrapLocked ||
		e.state.BootstrapStage != StageNone ||
		(lifecycle != LifecycleStarting && lifecycle != LifecycleBootstrapping) ||
		e.queue.len() != 0 ||
		e.chainsActive.Load() != 0
	if funded {
		return BootstrapConfig{}, ErrBootstrapLocked
	}

	if update.NumChains != nil {
		e.numChains = *update.NumChains
	}
	if update.FanoutSize != nil {
		e.fanoutSize = *update.FanoutSize
	}
	if update.SustainFee != nil {
		e.mode = e.mode.WithSustainFee(*update.SustainFee)
	}
	e.state.ChainsTarget = e.numChains
	return e.bootstrapConfigLocked(), nil
}

// RecordTip records the latest chain tip from the Arcade tip stream.
func (e *Engine) RecordTip(height int, hash string, at time.Time) {
	e.setState(func(s *State) {
		s.SSE.Connected = true
		s.SSE.TipHeight = height
		s.SSE.TipHash = hash
		s.SSE.TipTime = at
	})
}

// RecordReorg records a chain reorganization and returns the number of
// chains active at the time.
func (e *Engine) RecordReorg(height, depth int, tipBefore, tipAfter string) int {
	record := &ReorgRecord{
		Height:         height,
		Depth:          depth,
		Time:           time.Now(),
		ChainsImpacted: int(e.chainsActive.Load()),
		TipBefore:      tipBefore,
		TipAfter:       tipAfter,
	}
	e.setState(func(s *State) { s.LastReorg = record })
	return record.ChainsImpacted
}

// bootstrapConfigLocked returns the bootstrap configuration; e.mu must be held.
func (e *Engine) bootstrapConfigLocked() BootstrapConfig {
	return BootstrapConfig{
		NumChains:  e.numChains,
		FanoutSize: e.fanoutSize,
		SustainFee: e.mode.SustainFee,
	}
}

// currentMode returns the transaction mode in effect.
func (e *Engine) currentMode() *txbuilder.Mode {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.mode
}

// setState applies fn to the state under the write lock.
func (e *Engine) setState(fn func(*State)) {
	e.mu.Lock()
	fn(&e.state)
	e.mu.Unlock()
}

// recordBroadcastOk records an accepted broadcast and clears a degraded
// state.
func (e *Engine) recordBroadcastOk() {
	e.setState(func(s *State) {
		s.LastBroadcastOk = time.Now()
		s.ConsecutiveArcadeFailures = 0
		if s.Lifecycle == LifecycleDegraded {
			s.Lifecycle = LifecycleRunning
		}
	})
	e.acceptedCount.Add(1)
}

// recordBroadcastFail records a failed broadcast. Enough consecutive
// failures move the engine to LifecycleDegraded.
func (e *Engine) recordBroadcastFail(component string, err error) {
	e.setState(func(s *State) {
		s.ConsecutiveArcadeFailures++
		s.LastError = &ErrorRecord{Component: component, Msg: err.Error(), Time: time.Now()}
		if s.ConsecutiveArcadeFailures >= degradedAfterFailures {
			s.Lifecycle = LifecycleDegraded
		}
	})
}

// trackTPS publishes the accepted-broadcast count once per second.
func (e *Engine) trackTPS(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.acceptedTPS.Store(e.acceptedCount.Swap(0))
		}
	}
}

// positiveOr returns v when it is positive, and def otherwise.
func positiveOr[T int | time.Duration](v, def T) T {
	if v > 0 {
		return v
	}
	return def
}
