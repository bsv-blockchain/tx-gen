// Package app wires the tx-gen components together and runs them until
// shutdown.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/tx-gen/internal/arcade"
	"github.com/bsv-blockchain/tx-gen/internal/config"
	"github.com/bsv-blockchain/tx-gen/internal/engine"
	"github.com/bsv-blockchain/tx-gen/internal/metrics"
	"github.com/bsv-blockchain/tx-gen/internal/notifier"
	"github.com/bsv-blockchain/tx-gen/internal/safe"
	"github.com/bsv-blockchain/tx-gen/internal/server"
	"github.com/bsv-blockchain/tx-gen/internal/sse"
	"github.com/bsv-blockchain/tx-gen/internal/store"
	"github.com/bsv-blockchain/tx-gen/internal/txbuilder"
	"github.com/bsv-blockchain/tx-gen/internal/woc"
)

const (
	// serverShutdownTimeout bounds the graceful HTTP shutdown.
	serverShutdownTimeout = 5 * time.Second

	// drainTimeout bounds the wait for chains and streams to stop.
	drainTimeout = 30 * time.Second
)

var (
	// ErrStateMismatch is returned when the state file belongs to a
	// different lock script than the one configured.
	ErrStateMismatch = errors.New("state lock script does not match the configured mode")

	// ErrLegacyState is returned when the state file has UTXOs but no record
	// of the lock script they belong to.
	ErrLegacyState = errors.New("state has persisted UTXOs without lock-script metadata")
)

// BuildInfo describes the running binary.
type BuildInfo struct {
	// Version is the release version, such as v1.2.3.
	Version string

	// Commit is the source commit.
	Commit string

	// Date is the build date.
	Date string
}

// NewLogger returns a logger writing to w at level ("debug", "info", "warn",
// or "error"; anything else means info) in format ("json" or "text").
func NewLogger(w io.Writer, level, format string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToLower(level))); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if strings.EqualFold(format, "text") {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// Run starts every component and blocks until ctx is canceled or the HTTP
// server fails, then shuts down in order: HTTP server, background work,
// state file.
//
// A bootstrap failure does not stop the process: the HTTP API stays up so
// that /status and /readyz can report what went wrong.
func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger, build BuildInfo) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	logger.Info("starting", "config", cfg.Redacted(), "commit", build.Commit, "build_date", build.Date)
	m := metrics.New()
	runner := safe.Runner{Logger: logger, OnPanic: m.IncPanic}

	mode, err := txbuilder.NewMode(cfg.PrivateKey, cfg.InstanceID, cfg.SustainFee)
	if err != nil {
		return fmt.Errorf("configure transaction mode: %w", err)
	}
	attrs := []any{"mode", mode.Name, "script_hash", mode.ScriptHash(), "sustain_fee", mode.SustainFee}
	if mode.Address != "" {
		attrs = append(attrs, "address", mode.Address)
	}
	logger.Info("lock script ready", attrs...)

	st, err := store.Open(cfg.StatePath)
	if err != nil {
		return err
	}
	defer closeStore(logger, st)

	wocClient := woc.New(woc.Options{
		BaseURL:  cfg.WOCBaseURL,
		Timeout:  cfg.HTTPTimeout,
		RetryMax: cfg.BroadcastRetryMax,
		Logger:   logger,
	})
	utxos, err := initialUTXOs(ctx, st, mode, wocClient, logger)
	if err != nil {
		return err
	}

	arcadeClient := arcade.New(arcade.Options{
		BaseURL:       cfg.ArcadeBaseURL,
		CallbackToken: cfg.ArcadeCallbackToken,
		Timeout:       cfg.HTTPTimeout,
		Concurrency:   cfg.BroadcastConcurrency,
		RetryMax:      cfg.BroadcastRetryMax,
		Logger:        logger,
		Metrics:       m,
	})
	eng, err := engine.New(engine.Options{
		Mode:         mode,
		Broadcaster:  arcadeClient,
		Source:       wocClient,
		Store:        st,
		InitialUTXOs: utxos,
		NumChains:    cfg.NumChains,
		FanoutSize:   cfg.FanoutSize,
		Metrics:      m,
		Logger:       logger,
		Runner:       runner,
	})
	if err != nil {
		return fmt.Errorf("start engine: %w", err)
	}
	srv := server.New(server.Options{
		Engine:  eng,
		Arcade:  arcadeClient,
		Config:  cfg,
		Metrics: m,
		Logger:  logger,
		Version: build.Version,
	})

	var reporter sse.ReorgReporter
	if cfg.SlackWebhookURL != "" {
		reporter = notifier.New(notifier.Options{
			WebhookURL:  cfg.SlackWebhookURL,
			Channel:     cfg.SlackChannel,
			Environment: cfg.Environment,
			InstanceID:  cfg.InstanceID,
			Logger:      logger,
		})
	}

	var wg sync.WaitGroup
	runner.Go(&wg, "engine", func() {
		srv.MarkEngineRunning()
		if runErr := eng.Run(ctx); runErr != nil && ctx.Err() == nil {
			logger.Error("engine stopped; the API stays up for diagnosis", "error", runErr)
		}
		srv.MarkEngineStopped(ctx.Err() == nil)
	})
	sse.Start(ctx, &wg, sse.Options{
		BaseURL:       cfg.ArcadeBaseURL,
		CallbackToken: cfg.ArcadeCallbackToken,
		ReconnectMin:  cfg.SSEReconnectMin,
		ReconnectMax:  cfg.SSEReconnectMax,
		Sink:          eng,
		Reporter:      reporter,
		Metrics:       m,
		Logger:        logger,
		Runner:        runner,
	})

	serveErr := make(chan error, 1)
	runner.Go(&wg, "http_server", func() {
		if listenErr := srv.ListenAndServe(":" + cfg.Port); listenErr != nil {
			serveErr <- listenErr
			cancel()
		}
	})

	<-ctx.Done()
	logger.Info("shutdown initiated")
	shutdown(ctx, logger, runner, srv, &wg)

	select {
	case err = <-serveErr:
		return err
	default:
		return nil
	}
}

// initialUTXOs returns the persisted UTXOs or, for a fresh state file, the
// ones WhatsOnChain reports for the lock script. It refuses state that
// belongs to a different lock script.
func initialUTXOs(ctx context.Context, st *store.Store, mode *txbuilder.Mode, source engine.UTXOSource, logger *slog.Logger) ([]txbuilder.UTXO, error) {
	utxos, err := st.LoadAll()
	if err != nil {
		return nil, fmt.Errorf("restore UTXOs: %w", err)
	}
	if err = ensureStateMatchesMode(st, mode, len(utxos) > 0); err != nil {
		return nil, err
	}
	if len(utxos) > 0 {
		logger.Info("restored UTXOs", "count", len(utxos))
		return utxos, nil
	}
	logger.Info("fetching UTXOs", "source", "whatsonchain")
	utxos, err = source.FetchUTXOs(ctx, mode.ScriptHash())
	if err != nil {
		return nil, fmt.Errorf("fetch UTXOs: %w", err)
	}
	logger.Info("loaded UTXOs", "count", len(utxos))
	return utxos, nil
}

// ensureStateMatchesMode guards against resuming chains with the wrong key
// or instance tag: the state file records the lock-script hash it was
// created with, and must match the configured mode.
func ensureStateMatchesMode(st *store.Store, mode *txbuilder.Mode, hasUTXOs bool) error {
	current := mode.ScriptHash()
	stored, err := st.GetMeta(store.MetaLockScriptHash)
	if err != nil {
		return fmt.Errorf("read %s: %w", store.MetaLockScriptHash, err)
	}
	switch {
	case stored != "" && stored != current:
		return fmt.Errorf("%w: state has %s, configured %s; use a separate STATE_PATH or reconcile the existing state",
			ErrStateMismatch, stored, current)
	case stored == "" && hasUTXOs:
		return fmt.Errorf("%w; refusing to continue because existing outpoint scripts cannot be verified", ErrLegacyState)
	case stored == "":
		if err = st.SetMeta(store.MetaLockScriptHash, current); err != nil {
			return fmt.Errorf("write %s: %w", store.MetaLockScriptHash, err)
		}
	}
	if err = st.SetMeta(store.MetaTxMode, mode.Name); err != nil {
		return fmt.Errorf("write %s: %w", store.MetaTxMode, err)
	}
	return nil
}

// shutdown stops the HTTP server and waits for background work to drain.
// ctx is already canceled; only its values are kept.
func shutdown(ctx context.Context, logger *slog.Logger, runner safe.Runner, srv *server.Server, wg *sync.WaitGroup) {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serverShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("server shutdown", "error", err)
	} else {
		logger.Info("server stopped")
	}

	if waitTimeout(runner, wg, drainTimeout) {
		logger.Info("background work drained")
	} else {
		logger.Warn("background work did not drain before timeout")
	}
}

// waitTimeout waits for wg, giving up after timeout. It reports whether wg
// finished in time.
func waitTimeout(runner safe.Runner, wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	var waiter sync.WaitGroup
	runner.Go(&waiter, "shutdown_wait", func() {
		wg.Wait()
		close(done)
	})
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// closeStore closes the state file, logging any error.
func closeStore(logger *slog.Logger, st *store.Store) {
	if err := st.Close(); err != nil {
		logger.Warn("close store", "error", err)
		return
	}
	logger.Info("store closed")
}
