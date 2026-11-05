# CLAUDE.md

## 📦 Project Summary

**tx-gen** is a high-throughput Bitcoin SV (BSV) transaction generator for load and stress testing. It bootstraps thousands of independent UTXO chains from a single funded output, then drives them at a target transactions-per-second rate that can be changed at runtime over an authenticated HTTP API.

### Core Capabilities
- **Two-level bootstrap**: One funding UTXO fans out to `ceil(NUM_CHAINS/FANOUT_SIZE)` L1 outputs, then to `NUM_CHAINS` chain UTXOs, as a resumable state machine
- **Rate control**: A single TPS target that wakes every chain immediately when changed; `0` pauses without losing state
- **Crash safety**: BoltDB write-through persistence with pending-broadcast replay, so restarts resume from the last accepted chain tips
- **Transaction modes**: Instance-tagged push/drop scripts (default) or signed P2PKH, broadcast to Arcade v2 in Extended Format (EF)
- **Observability**: Prometheus metrics, Grafana dashboard, structured `slog` logs, health/readiness probes, and Slack reorg reports

### Target Use Cases
Mainnet load testing of Arcade and Teranode, sustained-throughput soak tests, and reorg case-study collection.

### Layout
- `cmd/tx-gen` – the binary entry point
- `internal/engine` – bootstrap, chains, and rate control (start here)
- `internal/txbuilder` – fanout and sustain transaction builders
- `internal/store` – BoltDB persistence (the gob field names are an on-disk format)
- `internal/{arcade,woc,sse}` – network clients; `internal/server` – the HTTP API
- `docs/runbook.md` – operations guide

## 🤖 Welcome, Claude

This repository uses **`AGENTS.md`** as the single source of truth for:

* Coding conventions (naming, formatting, commenting, testing)
* Contribution workflows (branch prefixes, commit message style, PR templates)
* Release, CI, and dependency‑management policies
* Security reporting and governance links

> **TL;DR:** **Read `AGENTS.md` first.**
> All technical or procedural questions are answered there.

### Quick Checklist for Claude

1. **Study `AGENTS.md`**
   Make sure every automated change or suggestion respects those rules.
2. **Follow branch‑prefix and commit‑message standards**
   They drive Mergify auto‑labeling and CI gates.
3. **Never tag releases**
4. **Pass CI**
   Run `go fmt`, `goimports`, `go vet`, `staticcheck`, and `golangci‑lint` locally before opening a PR.

If you encounter conflicting guidance elsewhere, `AGENTS.md` wins.
Questions or ambiguities? Open a discussion or ping a maintainer instead of guessing.

Happy hacking!
