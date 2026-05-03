# Improvement Plan

## Scope

This plan preserves the existing external API contract. In particular, `POST /config`
with bearer-token auth and a `{"tps": number}` body should keep working as it does
today. Any operational API additions should be backward-compatible wrappers or
read-only endpoints.

The transaction construction path is small and direct. Do not redesign the locking
script, unlocking script, EF encoding flow, or fee model unless tests prove they are
wrong. The useful work is around reliability, control, observability, and failure
handling for unattended 24/7 operation.

User selections (locked):
- Persistence: BoltDB (`go.etcd.io/bbolt`)
- Observability: Prometheus + `log/slog`
- Deployment: Docker image
- Tests: Critical path only (tx builders + engine state machine)

---

## Current Assessment

The app is a compact generator:

- It loads UTXOs for the fixed lock script from WhatsOnChain at startup.
- It bootstraps to 10,000 chains when it sees 1 or 100 starting UTXOs.
- It runs one goroutine per chain and broadcasts sustain transactions to Arcade.
- It exposes `POST /config` to set TPS, where `0` effectively pauses generation.
- It subscribes to Arcade tip and reorg SSE streams and logs events.

That is a good minimal core, but it is not operationally complete for a long-running
mainnet test load service. The UTXO queue is purely in-memory (crash loses all state),
logging is unstructured, health cannot distinguish "HTTP server is up" from "generator
is dead", TPS changes can take a long time to affect already-sleeping chains, the
bearer token is compared without constant-time, the HTTP transport uses `http.DefaultTransport`
defaults (MaxIdleConnsPerHost=2, catastrophic at 10k chains), there is no panic
recovery in chain goroutines, and no retry on Arcade failures.

---

## Recommended Work

### 1. BoltDB Persistence (`store.go` new, `queue.go` modify)

The UTXO queue is entirely in-memory. A `kill -9` or OOM loses all chain progress and
requires a fresh bootstrap.

- Open a `bbolt` DB at `STATE_PATH` (default `./state.db`); buckets: `utxos`, `meta`.
- `Queue` keeps the in-memory min-heap for ordering and adds write-through to BoltDB
  on every `Push`/`Pop` (single `Update` txn, UTXOs encoded as gob).
- `Restore()` loads all UTXOs from the bucket at startup; skips WoC fetch if any are
  found.
- `meta` bucket records bootstrap stage: `none` → `l1_done` → `l2_done`. Engine reads
  this at startup instead of inferring from queue length, removing the fragile
  qlen==1 / qlen==100 heuristic.
- Crash-safe broadcast: write a `pending/{txid}` record to BoltDB **before** broadcast;
  on success atomically replace pending with the new UTXO entry. On startup, replay
  any pending records as already-broadcast (arcade returned 202 — trust it).
- `db.Close()` called from the graceful-shutdown path in `main.go`.

### 2. Add an Explicit Engine State Model (`engine.go`)

Introduce an internal state snapshot owned by `Engine`:

- lifecycle state: `starting`, `bootstrapping`, `paused`, `running`, `degraded`,
  `stopping`, `stopped`, `failed`
- requested TPS and measured accepted TPS over recent windows
- active chain count, target chain count, exhausted chain count
- bootstrap progress and bootstrap failures
- consecutive Arcade broadcast failures and last successful broadcast time
- last WoC fetch time and result
- SSE connection status, last tip height, last tip time, last reorg time
- last significant error with timestamp and component

This is the foundation for health checks, metrics, Slack alerts, and useful `/status`
output. Without it the process can look alive while `engine.run` has returned after a
bootstrap failure.

### 3. Add Health, Readiness, and Status Endpoints (`server.go`)

Keep `POST /config` unchanged. Add:

- `GET /healthz` — unauthenticated liveness; 200 when the process and HTTP server are
  alive.
- `GET /readyz` — readiness; non-200 when engine failed to bootstrap, has zero usable
  chains, cannot reach Arcade, or has exceeded failure thresholds. Also non-200 while
  bootstrap stage is not `l2_done`.
- `GET /status` — authenticated JSON snapshot:
  `{tps, queueDepth, chainsActive, bootstrapStage, sse:{tip,reorg}, version, uptime,
  lastBroadcastError, lastBroadcastOk}`.
- Optional `POST /stop` — authenticated convenience wrapper that calls
  `SetTPS(0)`. `POST /start` does nothing new; `POST /config {"tps": N}` is enough.

### 4. Structured Logging (`main.go` + all log sites)

Replace every `log.Printf`/`log.Println` with `log/slog` (stdlib, no extra deps):

- `LOG_FORMAT=json|text` (default `json`); `LOG_LEVEL=debug|info|warn|error` (default `info`).
- Single root logger built in `main.go` with base attrs `service=bsv-tx-gen` and
  `version` (injected via ldflags).
- Key events to log at structured level: bootstrap stages, TPS changes, broadcast
  retries/failures, chain terminations, SSE connect/disconnect, panic recoveries.

### 5. Prometheus Metrics (`metrics.go` new)

Use `github.com/prometheus/client_golang`. Expose at `GET /metrics`.

| Type | Name | Labels |
|---|---|---|
| Counter | `txgen_broadcast_total` | `kind` (fanout/sustain), `result` (ok/error/retry) |
| Counter | `txgen_chain_terminated_total` | — |
| Counter | `txgen_panic_total` | `goroutine` |
| Counter | `txgen_auth_fail_total` | — |
| Counter | `txgen_sse_event_total` | `stream` (tip/reorg) |
| Counter | `txgen_sse_disconnect_total` | `stream` |
| Gauge | `txgen_tps_target` | — |
| Gauge | `txgen_chains_active` | — |
| Gauge | `txgen_queue_depth` | — |
| Gauge | `txgen_sse_connected` | `stream` |
| Histogram | `txgen_broadcast_latency_seconds` | `kind` |

### 6. Make TPS Control Immediate and Measurable (`engine.go`)

Replace the per-chain `time.After(chainInterval(tps))` control loop with a central
rate controller:

- Wake workers immediately on every TPS change, including pause and resume. Current
  code only broadcasts a wake on `tps > 0`; a chain sleeping on a slow old interval
  does not notice a higher TPS setting until its timer fires.
- Calculate rate from actual active chain count, not the hard-coded `numChains`
  constant.
- Track requested TPS, attempted TPS, accepted TPS, failed TPS, and current broadcast
  latency.
- Clamp or degrade gracefully when requested TPS exceeds current capacity.
- Report sustained under-delivery (accepted TPS < requested TPS for N seconds) as
  `degraded` health and Slack alert.

### 7. Harden Arcade Broadcasts (`arcade.go`)

Upgrade `arcadeClient.broadcast` into a context-aware client with clear result
classification:

- Signature: `Broadcast(ctx context.Context, ef []byte) error`.
- Tuned transport (`httpclient.go` new): `MaxIdleConnsPerHost=128`,
  `MaxIdleConns=256`, `IdleConnTimeout=90s`, `ForceAttemptHTTP2=true`,
  `DialContext` with `Timeout=5s, KeepAlive=30s`, `TLSHandshakeTimeout=5s`,
  `ResponseHeaderTimeout=10s`. Arcade and WoC share one transport; SSE uses a
  separate one with `ResponseHeaderTimeout=0`.
- Retry transient network errors, 429s, and 5xx responses: exponential backoff
  `100ms × 2^n + jitter`, max `BROADCAST_RETRY_MAX` attempts (default 3), capped at
  5s per attempt.
- Do not retry 4xx (except 429) — log at `warn` and drop.
- Bounded concurrency via `BROADCAST_CONCURRENCY` (default 64) buffered-chan semaphore
  acquired before every broadcast. Bootstrap L2 flows through the same semaphore.
- Increment `txgen_broadcast_total{result=ok|error|retry}` and record latency.
- Define `Broadcaster` interface in `engine.go` for test injection.

### 8. Make Bootstrap Retryable and Auditable (`engine.go`)

Turn fanout bootstrap into an explicit state machine:

- Track each parent as pending, building, broadcasting, accepted, failed, or unknown.
- Retry transient failures with backoff.
- Do not silently continue as healthy after partial L2 fanout.
- Surface partial bootstrap in `/readyz`, `/status`, and Slack.
- Add a reconciliation step that reloads script UTXOs from WoC after bootstrap
  attempts and compares expected outputs to discovered outputs.
- Persist bootstrap stage in BoltDB `meta` bucket so partial bootstrap survives
  restart.

### 9. Add Slack Issue Reporting (`notifier.go` new)

Configured by env vars: `SLACK_WEBHOOK_URL`, `SLACK_CHANNEL`, `ENVIRONMENT`,
`INSTANCE_ID`. No-op if `SLACK_WEBHOOK_URL` is unset.

Alert on:

- bootstrap failure or partial bootstrap
- generator entered `failed` or `degraded`
- accepted TPS remains below requested TPS for a configured window
- consecutive Arcade failures exceed threshold
- no accepted transactions for a configured window while TPS > 0
- SSE stream disconnected or tip data stale beyond threshold
- chain exhaustion approaching zero usable chains

The notifier deduplicates and rate-limits (one alert per condition per window). Sends a
recovery message when the condition clears. Payload includes component, severity,
current TPS, active chains, last error, and a short operator action hint.

### 10. Graceful Shutdown and Lifecycle (`main.go`, `server.go`, `engine.go`)

- Replace `http.ListenAndServe` with `*http.Server` + `Shutdown(ctx)` (5s grace).
- `safe.go` (new): `safeGo(wg *sync.WaitGroup, name string, fn func())` wraps `fn`
  in `defer recover()` that logs at `error` with goroutine name and stack, increments
  `txgen_panic_total{goroutine=name}`. Replaces every bare `go ...`.
- Top-level `sync.WaitGroup` tracks all goroutines: HTTP server, `engine.run`, SSE
  subscribers, every `runChain`, every bootstrap worker.
- Shutdown sequence on SIGINT/SIGTERM: cancel root ctx → `server.Shutdown` →
  `wg.Wait()` (30s deadline) → `db.Close()` → exit.
- Log lifecycle transitions at `info`: `shutdown initiated`, `server stopped`,
  `engine drained`, `db closed`.

### 11. Externalize Runtime Configuration (`config.go` new)

Move all hard-coded constants to env vars. No extra deps (`os.Getenv` + helpers).

| Env | Default | Purpose |
|---|---|---|
| `ADMIN_TOKEN` | required | bearer auth |
| `PORT` | `8080` | HTTP listen |
| `STATE_PATH` | `./state.db` | BoltDB file path |
| `LOG_FORMAT` | `json` | `json` or `text` |
| `LOG_LEVEL` | `info` | slog level |
| `ARCADE_BASE_URL` | `https://arcade-v2-us-1.bsvblockchain.tech` | broadcaster |
| `WOC_BASE_URL` | `https://api.whatsonchain.com/v1/bsv/main` | UTXO source |
| `NUM_CHAINS` | `10000` | total chains |
| `FANOUT_SIZE` | `100` | per-fanout outputs |
| `SUSTAIN_FEE` | `7` | sats per hop |
| `MAX_TPS` | `10000` | config endpoint upper bound |
| `BROADCAST_CONCURRENCY` | `64` | semaphore size |
| `BROADCAST_RETRY_MAX` | `3` | retry attempts |
| `HTTP_TIMEOUT` | `30s` | client read/write timeout |
| `SSE_RECONNECT_MIN` | `1s` | backoff floor |
| `SSE_RECONNECT_MAX` | `30s` | backoff ceiling |
| `SLACK_WEBHOOK_URL` | — | if set, enables Slack alerts |
| `SLACK_CHANNEL` | — | target channel |
| `ENVIRONMENT` | `production` | included in alert messages |
| `INSTANCE_ID` | hostname | included in alert messages |

`Config` struct loaded once in `main.go`, passed by pointer to all components. Include
effective config (secrets redacted) in `/status` output.

### 12. Security Hardening (`server.go`)

- `subtle.ConstantTimeCompare([]byte(token), []byte(adminToken)) == 1` for auth.
- `r.Body = http.MaxBytesReader(w, r.Body, 1024)` before JSON decode on all
  authenticated endpoints.
- Reject if `Content-Type` is not `application/json` on POST routes.
- Log auth failures at `warn` with remote IP; increment `txgen_auth_fail_total`.
- TLS delegated to reverse proxy (Caddy/nginx) — document this assumption in README.

### 13. SSE Hardening (`sse.go`)

- Replace fixed 5s reconnect sleep with exponential backoff (`SSE_RECONNECT_MIN` →
  `SSE_RECONNECT_MAX`), jittered, reset on successful event receipt.
- Increase `bufio.Scanner` buffer to 1 MiB to handle large reorg events.
- Use per-request read deadline: a goroutine resets a `time.AfterFunc(60s)` on every
  line received; fires `resp.Body.Close()` on idle to force reconnect.
- Increment `txgen_sse_event_total` per event; `txgen_sse_disconnect_total` per
  reconnect; set `txgen_sse_connected{stream}` gauge.

### 14. Critical-Path Tests (`tx_test.go`, `engine_test.go` new)

**`tx_test.go`**
- Table tests for `buildFanoutTx`: various input values, assert `len(outputs)==n`,
  `perOutput*n + fee == inputValue`, txid non-empty, EF bytes parseable.
- Table tests for `buildSustainTx`: normal decrement, chain-end (value==fee → 0-sat
  output), zero-value error, invalid txid error.
- `FuzzBuildSustainTx(f *testing.F)`: fuzz `(txhash, txpos, value)` — no panics,
  `newUTXO.Value <= utxo.Value`, EF length > 0.

**`engine_test.go`**
- `fakeBroadcaster`: records every EF call, configurable error injection.
- Test `bootstrapFull` (scaled-down: `NUM_CHAINS=4, FANOUT_SIZE=2`): produces 3
  broadcasts (1 L1 + 2 L2), queue ends with 4 entries.
- Test `bootstrapL2` resume: starts with `meta=l1_done` + 2 queued UTXOs, skips L1.
- Test `runChain` termination: chain with `value = sustainFee * N` → N broadcasts then
  exits; `txgen_chain_terminated_total` increments.
- Test pause/resume: `SetTPS(0)` → chains block; `SetTPS(>0)` → chains wake via
  `notify.Broadcast`.
- Test panic recovery: a broadcaster that panics → `safeGo` recovers, increments
  `txgen_panic_total`, engine still running.

### 15. Deployment (`Dockerfile`, `.dockerignore` new, README/SKILL update)

**`Dockerfile`** — multi-stage:
1. `FROM golang:1.25-alpine AS build` — `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$VERSION"`.
2. `FROM gcr.io/distroless/static-debian12:nonroot` — copy binary, `EXPOSE 8080`,
   `ENTRYPOINT ["/bsv-tx-gen"]`. Volume mount point `VOLUME /data` for `STATE_PATH`.

**`.dockerignore`** — excludes `state.db`, `bsv-tx-gen` binary, `.env`, `.git`.

**Repo hygiene:**
- Delete committed `bsv-tx-gen` binary from repo.
- Add `state.db`, `bsv-tx-gen`, `.env` to `.gitignore`.

**README/SKILL additions:** Docker run example with volume mount, all env vars table,
`/metrics` + `/healthz` + `/readyz` + `/status` docs, ops section (backup `state.db`,
read panic counter, what `readyz` failure means, Slack alert meanings).

### 16. Operator Runbook (`RUNBOOK.md` new)

Document in the repo:

- Required and optional environment variables.
- Start/stop commands (bare metal and Docker).
- How to set TPS, check health, and inspect status.
- What each Slack alert means and the recommended first action.
- How to safely restart and reconcile chain state from `state.db`.
- Expected behavior when TPS is 0, Arcade is degraded, and WoC is unavailable.
- How to back up and restore `state.db`.

---

## Priority Order

1. Add tests for current transaction construction and queue behavior.
2. Add BoltDB persistence; restore queue on restart.
3. Add engine state snapshots plus `/healthz`, `/readyz`, and authenticated `/status`.
4. Replace TPS timing with immediate central rate controller.
5. Harden Arcade broadcasts: context, retries, tuned transport, concurrency semaphore.
6. Add structured logging (`slog`) and Prometheus `/metrics`.
7. Add Slack alerting wired to engine state and failure thresholds.
8. Make bootstrap retryable, auditable, and unhealthy on partial completion.
9. Add graceful shutdown, `safeGo` panic recovery, and lifecycle management.
10. Externalize all configuration.
11. Security hardening: constant-time auth, body limit, content-type check.
12. SSE hardening: exp backoff, scanner buffer, read deadline.
13. Docker image, `.dockerignore`, repo hygiene.
14. Operator runbook.

---

## Files

### New
- `config.go` — env-driven `Config` struct and loader
- `store.go` — BoltDB wrapper (`Open`, `SaveUTXO`, `DeleteUTXO`, `LoadAll`, `GetMeta`, `SetMeta`, `Close`)
- `metrics.go` — prometheus collectors and `RegisterMetrics(mux)`
- `safe.go` — `safeGo` panic-recovery goroutine helper
- `httpclient.go` — tuned `http.Transport` factories for arcade/woc and SSE
- `notifier.go` — Slack webhook notifier with dedup/rate-limit
- `tx_test.go`, `engine_test.go`
- `Dockerfile`, `.dockerignore`, `RUNBOOK.md`

### Modified
- `main.go` — slog init, config load, BoltDB open, signal handling, graceful shutdown
- `engine.go` — `Broadcaster`/`UTXOSource` interfaces, state model, central rate controller, ctx, semaphore, retries, metrics, `safeGo`
- `queue.go` — BoltDB write-through
- `arcade.go` — ctx, retries, tuned transport
- `woc.go` — ctx, retries, tuned transport
- `sse.go` — exp backoff, scanner buffer, read deadline, metrics
- `server.go` — constant-time auth, body limit, content-type check, `*http.Server`, `/metrics`, `/healthz`, `/readyz`, `/status`
- `tx.go` — expose internals needed by tests
- `go.mod` — add `go.etcd.io/bbolt`, `github.com/prometheus/client_golang`
- `README.md`, `SKILL.md` — Docker, all endpoints, ops guidance
- `.gitignore`, `.env.example` — add `state.db`, `bsv-tx-gen`

---

## Success Criteria

- Existing `POST /config` callers continue to work unchanged.
- Operators can start, stop, set TPS, and inspect status without reading logs.
- Health checks fail when transaction generation is not actually functional.
- Slack receives actionable, deduplicated issue and recovery reports.
- TPS changes take effect within seconds, not after old per-chain sleep intervals.
- A transient Arcade or network failure does not silently reduce chain capacity.
- A crash followed by restart restores UTXO queue from `state.db` without re-bootstrap.
- The app can run unattended for 24/7 load tests with clear evidence of requested TPS,
  accepted TPS, failures, and recovery behavior.
- `go test ./...` passes; `FuzzBuildSustainTx` finds no panics in 30s.
- `docker build` produces a working image; `docker run` with correct env starts cleanly.
