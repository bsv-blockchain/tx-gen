# CODEX Agent Plan — Server, External Clients, SSE, Main Orchestration

**Scope**: HTTP server (endpoints, auth, health), external integrations (Arcade broadcaster, WoC UTXO source, Arcade SSE streams), main entrypoint, graceful shutdown wiring, and reorg event ingestion that feeds the engine state and notifier.

**Parallel Execution Rule**: You may only edit files listed below. Do not touch engine.go, queue.go, tx.go, config.go, store.go, metrics.go, safe.go, httpclient.go, notifier.go, Dockerfile, or any docs/CI files. Implement against the interfaces and state snapshot defined in engine.go (CLAUDE). Use SafeGo from GROK.

---

## 1. Main Entry Point & Lifecycle (`main.go`)
- Load `*Config` (from GROK).
- Open BoltDB via store (GROK) and call `Restore()`.
- Initialize structured logger (`slog`) with `LOG_FORMAT`/`LOG_LEVEL`.
- Create Engine (CLAUDE) with injected dependencies:
  - `Broadcaster` = new Arcade client (this file)
  - `UTXOSource` = new WoC client (this file)
  - Notifier (GROK)
  - Metrics (GROK)
- Start SSE subscribers (tip + reorg) using hardened client from GROK.
- Wire HTTP server (this file) with healthz, readyz, status, metrics, config.
- Handle SIGINT/SIGTERM: cancel context → `server.Shutdown(5s)` → `wg.Wait(30s)` → `db.Close()` → exit.
- Use `safe.SafeGo` (GROK) for every background goroutine.
- Log lifecycle transitions at info level.

## 2. HTTP Server (`server.go`)
- Keep existing `POST /config` (bearer auth, JSON `{"tps": N}`) unchanged.
- Add:
  - `GET /healthz` — 200 if process alive (no deep checks).
  - `GET /readyz` — 200 only after `l2_done` bootstrap, >0 usable chains, no consecutive failures, Arcade reachable.
  - `GET /status` — authenticated JSON snapshot of `EngineState` + config (redacted) + uptime + version.
  - `GET /metrics` — Prometheus handler from GROK.
- Security:
  - `subtle.ConstantTimeCompare` for token.
  - `http.MaxBytesReader(1024)` on authenticated POSTs.
  - Reject non-`application/json` on POST.
  - Log auth failures at warn + increment metric.
- Replace `ListenAndServe` with `*http.Server` + `Shutdown`.
- Optional convenience `POST /stop` (alias for TPS=0).

## 3. Arcade Broadcaster (`arcade.go`)
- Implement `Broadcaster` interface (from CLAUDE engine.go).
- Context-aware `Broadcast(ctx, ef []byte) error`.
- Use tuned transport from GROK `httpclient.go`.
- Retry transient/429/5xx with exponential backoff + jitter (max 3 attempts).
- Never retry 4xx (except 429).
- Bounded concurrency via semaphore (`BROADCAST_CONCURRENCY`).
- Record metrics (broadcast total, latency) on every attempt.
- On success, clear pending record in store (GROK).

## 4. WoC UTXO Source (`woc.go`)
- Implement `UTXOSource` interface (from CLAUDE).
- `FetchUTXOs(ctx, scriptHash string) ([]UTXO, error)`.
- Use tuned transport + timeout from GROK.
- Retry on transient errors; surface permanent failures.
- Used only at bootstrap or reconciliation; results feed engine.

## 5. SSE Hardening & Reorg Capture (`sse.go`)
- Subscribe to Arcade tip and reorg streams.
- Exponential backoff reconnect (`SSE_RECONNECT_MIN` → `SSE_RECONNECT_MAX`, jitter).
- Increase `bufio.Scanner` buffer to 1 MiB for large reorg payloads.
- Per-request read deadline (60s idle → force reconnect).
- On **reorg event**:
  - Parse height, depth, affected tip.
  - Call `engine.RecordReorg(height, depth, tipBefore, tipAfter)` (updates state snapshot).
  - Increment `txgen_sse_event_total{stream=reorg}` and `txgen_reorg_total{depth}` (GROK metrics).
  - Log structured info with full details.
  - Call `notifier.ReportReorg(...)` (GROK) so the rare mainnet event is captured for case studies.
- On tip event: update engine state tip height/time.
- Track connection status gauges.
- Use separate SSE transport (no response timeout).

## 6. Reorg Case-Study Path (critical for this agent)
- Every reorg must flow: SSE → engine state → structured log → metrics counter → optional Slack notable-event post.
- Confirm that normal chain operation continues with zero data loss.
- This produces the evidence the team wants for mainnet case studies.

---

## Files (CODEX owns these exclusively)

**Modified**:
- main.go
- server.go
- arcade.go
- woc.go
- sse.go

**Interfaces you implement**:
- `Broadcaster` (for engine)
- `UTXOSource` (for engine)

**You consume** (defined elsewhere):
- `EngineState` / `RecordReorg` (from engine.go)
- `SafeGo` (from safe.go)
- `*Config`, store helpers, metrics, notifier (from GROK)

**Do not edit**: engine.go, queue.go, tx.go, config.go, store.go, metrics.go, safe.go, httpclient.go, notifier.go, tx_test.go, engine_test.go, Dockerfile, RUNBOOK.md, ARCHITECTURE.html, .github/workflows/ci.yml, README.md, .gitignore.

---

## Success Criteria (CODEX portion)
- Existing `POST /config` continues to work exactly as before.
- `/healthz`, `/readyz`, `/status`, `/metrics` all respond correctly.
- Reorg events are fully captured (log + metric + engine state + Slack case-study post) with no impact on running chains.
- SSE reconnects survive network blips; tip/reorg data stays fresh.
- Graceful shutdown completes within 30s with no leaked goroutines or DB corruption.
- No file conflicts with GROK or CLAUDE agents.
- `go test ./...` passes (your integration points are covered by CLAUDE's engine tests).

---

**Execution Note**: The reorg capture path is the new high-value feature for this agent. Stabilize the `RecordReorg` call and notifier integration early so the other agents can verify end-to-end.
