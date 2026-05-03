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
	lockScript := buildLockScript()
	scriptHash := scriptToHash(lockScript)
	logger.Info("lock script ready", "script_hash", scriptHash)

	utxos, err := store.LoadAll()
	if err != nil {
		logger.Error("restore UTXOs", "error", err)
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
	engine := newEngine(q, arcade, lockScript)
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
