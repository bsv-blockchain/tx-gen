# Improvement Plan

## Scope

This plan preserves the existing external API contract. In particular, `POST /config`
with bearer-token auth and a `{"tps": number}` body should keep working as it does
today. Any operational API additions should be backward compatible wrappers or
read-only endpoints.

The transaction construction path is small and direct. I would not redesign the
locking script, unlocking script, EF encoding flow, or fee model unless tests prove
they are wrong. The useful work is mostly around reliability, control, observability,
and failure handling for unattended 24/7 operation.

## Current Assessment

The app is a compact generator:

- It loads UTXOs for the fixed lock script from WhatsOnChain at startup.
- It bootstraps to 10,000 chains when it sees 1 or 100 starting UTXOs.
- It runs one goroutine per chain and broadcasts sustain transactions to Arcade.
- It exposes `POST /config` to set TPS, where `0` effectively pauses generation.
- It subscribes to Arcade tip and reorg SSE streams and logs events.

That is a good minimal core, but it is not yet operationally complete for a
long-running mainnet test load service. Important failures are currently only log
lines, health cannot distinguish "HTTP server is up" from "generator is dead", and
TPS changes can take a long time to affect already-sleeping chains.

## Recommended Work

### 1. Add an Explicit Engine State Model

Introduce an internal state snapshot owned by `Engine`:

- lifecycle state: `starting`, `bootstrapping`, `paused`, `running`, `degraded`,
  `stopping`, `stopped`, `failed`
- requested TPS and measured accepted TPS over recent windows
- active chain count, target chain count, exhausted chain count
- bootstrap progress and bootstrap failures
- consecutive Arcade broadcast failures and last successful broadcast time
- last WOC fetch time and result
- SSE connection status, last tip height, last tip time, last reorg time
- last significant error, with timestamp and component

This is the foundation for health checks, Slack alerts, and useful DevOps status.
Without this, the process can look alive while `engine.run` has returned after a
bootstrap failure.

### 2. Add Health and Status Endpoints

Keep `POST /config` unchanged, and add backward-compatible operational endpoints:

- `GET /healthz`: unauthenticated liveness; returns 200 when the process and HTTP
  server are alive.
- `GET /readyz`: readiness; returns non-200 when the engine failed to bootstrap,
  has zero usable chains, cannot reach Arcade, or has exceeded failure thresholds.
- `GET /status`: authenticated JSON snapshot for operators and automation.
- Optional `POST /start` and `POST /stop`: authenticated convenience wrappers over
  existing TPS control. `POST /stop` should call the same internal path as
  `POST /config {"tps":0}`.

The key behavior is that DevOps can tell the difference between "the process is up",
"the generator is ready to produce load", and "the requested load is actually being
accepted by Arcade".

### 3. Make TPS Control Immediate and Measurable

Replace the per-chain `time.After(chainInterval(tps))` control loop with a central
rate controller:

- Wake workers immediately on every TPS change, including pause and resume.
- Calculate rate from the actual active chain count, not only the hard-coded
  `numChains` constant.
- Track requested TPS, attempted TPS, accepted TPS, failed TPS, and current
  broadcast latency.
- Clamp or degrade gracefully when requested TPS exceeds current capacity.
- Report sustained under-delivery as degraded health and Slack alerts.

The current code uses `numChains` for interval calculation even if fewer chains
actually started. It also only wakes paused chains; a running chain that is sleeping
after a very low TPS setting may not notice a higher TPS setting until its old timer
fires.

### 4. Harden Arcade Broadcasts

Upgrade `arcadeClient.broadcast` into a context-aware client with clear result
classification:

- Use `http.NewRequestWithContext`.
- Tune the HTTP transport for high request volume: idle connection counts, per-host
  connection limits, TLS reuse, and sensible timeouts.
- Retry transient network errors, 429s, and 5xx responses with bounded exponential
  backoff and jitter.
- Treat known idempotent duplicate/already-seen responses as success if Arcade's
  contract supports that.
- Preserve enough information to reconcile unknown outcomes, especially when the
  client times out after Arcade may have accepted the transaction.
- Record structured counters for accepted, rejected, retried, timed out, and unknown
  broadcasts.

This is a high-priority reliability item. A single transient broadcast error during
L2 bootstrap currently logs the failure and loses that parent from the in-memory
bootstrap flow, reducing chain capacity without making the process unhealthy.

### 5. Make Bootstrap Retryable and Auditable

Turn fanout bootstrap into an explicit state machine:

- Track each parent as pending, building, broadcasting, accepted, failed, or
  unknown.
- Retry transient failures with backoff.
- Do not silently continue as healthy after partial L2 fanout.
- Surface partial bootstrap in `/readyz`, `/status`, and Slack.
- Add a reconciliation step that reloads script UTXOs from WOC after bootstrap
  attempts and compares expected outputs to discovered outputs.

This avoids a class of failures where the service starts with fewer chains than
expected and therefore cannot produce the requested long-running TPS.

### 6. Add Slack Issue Reporting

Add a small internal notifier, configured by environment variables such as
`SLACK_WEBHOOK_URL`, `SLACK_CHANNEL`, `ENVIRONMENT`, and `INSTANCE_ID`.

Alert on:

- bootstrap failure or partial bootstrap
- generator entered `failed` or `degraded`
- accepted TPS remains below requested TPS for a configured window
- consecutive Arcade failures exceed threshold
- broadcast unknown outcomes exceed threshold
- no accepted transactions for a configured window while requested TPS is above 0
- SSE stream disconnected or tip data stale beyond threshold
- chain exhaustion approaching zero usable chains

The notifier should deduplicate and rate-limit alerts so one outage does not flood
Slack. Include component, severity, current TPS, active chains, last error, and a
short operator action hint. Also send a recovery message when the condition clears.

### 7. Improve Graceful Shutdown and Process Lifecycle

Use a single lifecycle manager for the HTTP server, engine, SSE subscribers, and
network clients:

- Shut down `http.Server` with `Shutdown(ctx)` on SIGINT/SIGTERM.
- Pass cancellation through WOC fetches, Arcade broadcasts, and SSE connects.
- Wait for goroutines with `sync.WaitGroup` or `errgroup`.
- Stop accepting new transactions before shutdown, then allow in-flight broadcasts
  to finish within a bounded timeout.
- Mark state as `stopping` and then `stopped` for health and logs.

This reduces ambiguous shutdowns and makes restarts easier to reason about during
operational incidents.

### 8. Externalize Runtime Configuration

Keep current defaults, but move hard-coded operational values to configuration:

- Arcade base URL
- WOC base URL
- target chain count, defaulting to 10,000
- fanout size, defaulting to 100
- maximum TPS, defaulting to 10,000
- HTTP timeouts and retry limits
- health thresholds
- Slack settings

Validate configuration at startup and include the effective config in `/status`
without exposing secrets.

### 9. Add Focused Tests Before Refactoring

The repo currently has no test files. Add tests around the behavior that must not
change:

- transaction fee and output-value calculations
- generated output count and txid propagation for fanout transactions
- sustain transaction value decrement and terminal zero-output behavior
- queue ordering for confirmed and unconfirmed UTXOs
- `POST /config` auth, validation, and response shape
- rate controller behavior on start, stop, and TPS changes
- broadcast retry/result classification with `httptest`
- bootstrap partial-failure handling
- Slack notifier deduplication and recovery messages

Also add a fake Arcade server for soak and chaos tests: delayed responses, 202s,
429s, 5xxs, connection resets, duplicate responses, and SSE disconnects.

### 10. Add Operator Runbooks

Document common operations in the repo:

- required environment variables
- start and stop commands
- how to set TPS with the existing `/config` API
- how to check liveness, readiness, and detailed status
- what Slack alerts mean and what to do first
- how to safely restart and reconcile chain state
- expected behavior when TPS is 0, when Arcade is degraded, and when WOC is
  unavailable

This does not change the app behavior, but it makes the generator usable by people
who are not reading the Go source during an incident.

## Priority Order

1. Add tests for current transaction construction, queue behavior, and `/config`.
2. Add engine state snapshots plus `/healthz`, `/readyz`, and authenticated
   `/status`.
3. Add Slack alerting wired to the engine state and failure thresholds.
4. Replace TPS timing with an immediate central rate controller.
5. Harden Arcade broadcasts with retries, context, tuned transport, and result
   classification.
6. Make bootstrap retryable, auditable, and unhealthy on partial completion.
7. Add graceful shutdown and lifecycle management.
8. Externalize configuration while preserving existing defaults.
9. Add fake-Arcade soak and chaos tests.
10. Write the operator runbook.

## Success Criteria

- Existing `POST /config` callers continue to work unchanged.
- Operators can start, stop, set TPS, and inspect status without reading logs.
- Health checks fail when transaction generation is not actually functional.
- Slack receives actionable, deduplicated issue and recovery reports.
- TPS changes take effect within seconds, not after old per-chain sleep intervals.
- A transient Arcade or network failure does not silently reduce chain capacity.
- The app can run unattended for 24/7 load tests with clear evidence of requested
  TPS, accepted TPS, failures, and recovery behavior.
