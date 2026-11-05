// Command tx-gen is a high-throughput BSV transaction generator.
//
// It bootstraps a configurable number of independent UTXO chains from a
// single funded output, then drives them at a target TPS that can be changed
// at runtime over HTTP. Configuration comes from environment variables; see
// the README for the full list.
//
// Usage:
//
//	ADMIN_TOKEN=secret INSTANCE_ID=runner tx-gen
//	tx-gen -version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/bsv-blockchain/tx-gen/internal/app"
	"github.com/bsv-blockchain/tx-gen/internal/config"
)

// Build metadata, set by goreleaser with -ldflags "-X main.version=...".
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes tx-gen with args and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("tx-gen", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print version information and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	build := buildInfo()
	if *showVersion {
		_, _ = fmt.Fprintf(stdout, "tx-gen %s (commit %s, built %s)\n", build.Version, build.Commit, build.Date)
		return 0
	}

	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "config: %v\n", err)
		return 1
	}

	logger := app.NewLogger(stdout, cfg.LogLevel, cfg.LogFormat).With("service", "tx-gen", "version", build.Version)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err = app.Run(ctx, cfg, logger, build); err != nil {
		logger.Error("tx-gen stopped", "error", err)
		return 1
	}
	logger.Info("shutdown complete")
	return 0
}

// buildInfo returns the ldflags build metadata, falling back to the module
// version recorded by "go install" when the binary was not built by
// goreleaser.
func buildInfo() app.BuildInfo {
	info := app.BuildInfo{Version: version, Commit: commit, Date: buildDate}
	if info.Version != "dev" {
		return info
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		info.Version = bi.Main.Version
	}
	return info
}
