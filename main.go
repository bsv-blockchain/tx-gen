package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "dev"

const (
	metaLockScriptHash = "lock_script_hash"
	metaTxMode         = "tx_mode"
)

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	logger := newLogger(cfg).With("service", "bsv-tx-gen", "version", version)
	slog.SetDefault(logger)
	logger.Info("starting", "config", redactedConfig(cfg))

	store, err := NewStore(cfg.StatePath)
	if err != nil {
		logger.Error("open store", "error", err)
		os.Exit(1)
	}

	rootCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	woc := newWOCClientWithConfig(cfg, logger)
	txMode, err := NewTxModeFromConfig(cfg)
	if err != nil {
		logger.Error("configure transaction mode", "error", err)
		closeStore(logger, store)
		os.Exit(1)
	}
	scriptHash := scriptToHash(txMode.LockScript)
	attrs := []any{"mode", txMode.Name, "script_hash", scriptHash, "sustain_fee", txMode.SustainFee}
	if txMode.Address != "" {
		attrs = append(attrs, "address", txMode.Address)
	}
	logger.Info("lock script ready", attrs...)

	utxos, err := store.LoadAll()
	if err != nil {
		logger.Error("restore UTXOs", "error", err)
		closeStore(logger, store)
		os.Exit(1)
	}
	if err := ensureStateMatchesTxMode(store, txMode, len(utxos) > 0); err != nil {
		logger.Error("state transaction mode mismatch", "error", err)
		closeStore(logger, store)
		os.Exit(1)
	}
	if len(utxos) > 0 {
		logger.Info("restored UTXOs", "count", len(utxos))
	} else {
		logger.Info("fetching UTXOs", "source", "whatsonchain")
		utxos, err = woc.FetchUTXOs(rootCtx, scriptHash)
		if err != nil {
			logger.Error("fetch UTXOs", "error", err)
			closeStore(logger, store)
			os.Exit(1)
		}
	}
	logger.Info("loaded UTXOs", "count", len(utxos))

	q := newQueue()
	for _, u := range utxos {
		q.Push(u)
	}
	SetQueueDepth(q.Len())

	arcade := newArcadeClientWithConfig(cfg, store, logger)
	engine := newEngineWithMode(q, arcade, txMode)
	engine.configure(cfg.NumChains, cfg.FanoutSize)
	engine.SetUTXOSource(woc)
	server := newServerWithConfig(engine, cfg, store, logger)
	var notifier *Notifier
	if cfg.SlackWebhookURL != "" {
		notifier = NewNotifier(cfg)
	}

	var wg sync.WaitGroup
	SafeGo(&wg, "engine", func() {
		server.markEngineRunning()
		engine.run(rootCtx)
		server.markEngineStopped(rootCtx.Err() == nil)
	})
	startSSESubscribers(rootCtx, engine, cfg, logger, notifier, &wg)

	SafeGo(&wg, "http_server", func() {
		if err := server.start(":" + cfg.Port); err != nil {
			logger.Error("server stopped unexpectedly", "error", err)
			stopSignals()
		}
	})

	<-rootCtx.Done()
	logger.Info("shutdown initiated")

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("server shutdown", "error", err)
	} else {
		logger.Info("server stopped")
	}
	cancelShutdown()

	if waitFor(&wg, 30*time.Second) {
		logger.Info("background work drained")
	} else {
		logger.Warn("background work did not drain before timeout")
	}

	closeStore(logger, store)
	logger.Info("shutdown complete")
}

func newLogger(cfg *Config) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToLower(cfg.LogLevel))); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(cfg.LogFormat, "json") {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func waitFor(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	var waiter sync.WaitGroup
	SafeGo(&waiter, "shutdown_wait", func() {
		wg.Wait()
		close(done)
	})
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func closeStore(logger *slog.Logger, store *Store) {
	if store == nil {
		return
	}
	if err := store.Close(); err != nil {
		logger.Warn("close store", "error", err)
		return
	}
	logger.Info("store closed")
}

func ensureStateMatchesTxMode(store *Store, mode *TxMode, hasUTXOs bool) error {
	if store == nil || mode == nil {
		return nil
	}
	currentHash := scriptToHash(mode.LockScript)
	storedHash, err := store.GetMeta(metaLockScriptHash)
	if err != nil {
		return fmt.Errorf("read %s: %w", metaLockScriptHash, err)
	}
	if storedHash != "" && storedHash != currentHash {
		return fmt.Errorf("state lock script hash %s does not match configured %s; use a separate STATE_PATH or reconcile the existing state", storedHash, currentHash)
	}
	if storedHash == "" && hasUTXOs && mode.Name == txModeP2PKH {
		return fmt.Errorf("state has persisted UTXOs without lock-script metadata; refusing P2PKH mode because existing outpoint scripts cannot be verified")
	}
	if storedHash == "" {
		if err := store.SetMeta(metaLockScriptHash, currentHash); err != nil {
			return fmt.Errorf("write %s: %w", metaLockScriptHash, err)
		}
	}
	if err := store.SetMeta(metaTxMode, mode.Name); err != nil {
		return fmt.Errorf("write %s: %w", metaTxMode, err)
	}
	return nil
}
