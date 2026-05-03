# Complete Production Plan

## Verdict

`PLAN.md` is not yet production-delivered. The implementation has meaningful pieces in
place, but the review findings below block the original reliability and unattended
operation goals.

This document is the source of truth for closing the gap. The project is production
complete only when every blocker is resolved and the validation checklist at the end
passes on a clean checkout.

## Non-Negotiable Contract

- Preserve `POST /config` with bearer auth and body `{"tps": N}`.
- `POST /stop` may exist as a convenience alias for `{"tps": 0}`.
- Do not replace the BSV transaction construction model unless tests prove it wrong.
- Persistence must survive process crash and restart without double-spending stale
  outputs or losing active chain tips.
- Reorg events must flow once through engine state, structured logs, metrics, and
  optional Slack notification.

## Blocking Findings To Resolve

### GROK-Owned Fixes

1. Docker healthcheck is invalid.
   - `Dockerfile` uses `CMD-SHELL` in a distroless image with no shell.
   - It also invokes `-health`, which the binary does not implement.
   - Fix by either removing Docker `HEALTHCHECK`, adding a real binary health subcommand,
     or using an exec-form helper that exists in the image. A distroless image cannot
     rely on shell, curl, or wget.

2. No-op notifier can panic.
   - `NewNotifier` returns a `Notifier` with `last == nil` when Slack is disabled.
   - `ReportReorg`, `AlertFailure`, and `AlertRecovery` can call `shouldSend`, which
     writes to `last`.
   - Fix by always initializing `last`, and make no-webhook mode log only without
     posting.

3. Pending broadcasts are never restored.
   - `SavePending` and `ClearPending` exist, but there is no `LoadPending` or startup
     replay path.
   - Fix store support for loading pending records and define the exact replay policy:
     pending accepted by Arcade is trusted as broadcast, then reconciled into durable
     UTXO state or marked unknown for WoC reconciliation.

4. Config defaults diverge from `PLAN.md`.
   - Required defaults: `LOG_FORMAT=json`, `SUSTAIN_FEE=7`, `MAX_TPS=10000`,
     `BROADCAST_CONCURRENCY=64`, `BROADCAST_RETRY_MAX=3`,
     `SSE_RECONNECT_MIN=1s`, `SSE_RECONNECT_MAX=30s`, `ENVIRONMENT=production`.
   - Use the `SSE_RECONNECT_MIN` env name, or support it while accepting the old alias.

5. SSE metrics are missing stream labels.
   - `txgen_sse_connected` must be labeled by `stream=tip|reorg`.
   - Add helpers that accept the stream name, and coordinate call-site updates in
     `sse.go`.

6. Docs reference nonexistent endpoints.
   - README and RUNBOOK must document `POST /config`, not `POST /admin/tps`.
   - Include `POST /stop` only as an optional convenience.

### CLAUDE-Owned Fixes

1. Successful chain state is not persisted.
   - After a sustain broadcast succeeds, the new UTXO remains only in the goroutine's
     local variable.
   - Fix by atomically clearing the pending broadcast and saving the new UTXO as the
     active durable chain tip.

2. Restore can duplicate UTXOs.
   - `Queue.Restore` appends store data into the existing heap.
   - Current startup loads store UTXOs before `newEngine`, and `newEngine` calls
     `Restore` again.
   - Fix startup ownership: restore exactly once, clear the heap before restore, and
     do not fetch WoC if store data exists.

3. Bootstrap failure can hang.
   - `run` waits for `trackTPS`, which exits only on context cancellation.
   - Fix by canceling the internal tracker on bootstrap failure or by not waiting for
     a context-bound tracker before returning from failed startup.

4. Reorg metric is double-counted.
   - `RecordReorg` increments `txgen_reorg_total`, and `sse.go`/notifier also
     increments it.
   - Fix by choosing exactly one owner. Preferred: SSE/notifier owns external event
     metrics; engine only records state.

5. WoC reconciliation is not implemented.
   - `SetUTXOSource` exists, but bootstrap marks `l2_done` based only on successful
     broadcasts.
   - Fix by fetching script UTXOs after bootstrap attempts and comparing discovered
     outputs to expected outputs before marking `l2_done`.

6. Persistence errors are ignored.
   - `Queue.Push` and `Queue.Pop` mutate memory while dropping store errors.
   - Fix by returning errors from persistence-aware operations or by adding explicit
     `PushPersisted`/`PopPersisted` APIs used by the engine. Memory and BoltDB must not
     diverge silently.

### CODEX Integration Follow-Ups

- Update `sse.go` call sites when GROK changes SSE metric helpers to include stream
  labels.
- Ensure auth failures increment `txgen_auth_fail_total` once that metric exists.
- Ensure `/readyz` uses the final engine snapshot fields and the final bootstrap stage
  source, not duplicate fallback state.
- Ensure the main startup path restores store state exactly once after CLAUDE/GROK
  settle the queue/store API.
- Remove duplicate reorg metric increments after CLAUDE chooses the single owner.

## Delivery Matrix Against `PLAN.md`

| PLAN.md Area | Required Delivery | Current Status | Exit Criteria |
|---|---|---|---|
| BoltDB persistence | Durable queue, meta stage, pending replay, close on shutdown | Blocked | Restart after crash resumes only current UTXO tips, no duplicates, no stale spends |
| Engine state model | Snapshot with lifecycle, TPS, chains, bootstrap, SSE, reorg, last error | Partial | `/status` exposes final `EngineState` and accurately changes through failures/recovery |
| Health/readiness/status | `healthz`, `readyz`, authenticated `status`, compatible `config` | Partial | Readiness fails for bootstrap failure, zero chains, Arcade failure, stale bootstrap |
| Structured logging | `slog`, JSON default, structured lifecycle/failure/reorg logs | Partial | No core path relies on unstructured `log.Printf` for operational events |
| Prometheus metrics | Full metric table including auth, SSE event/disconnect, chain terminated | Partial | `/metrics` exposes all names and labels from `PLAN.md` |
| TPS control | Immediate pause/resume, measured accepted TPS, degradation | Partial | TPS changes wake sleeping chains; under-delivery enters degraded state |
| Arcade broadcasts | Context, retries, semaphore, tuned transport, metrics, no 4xx retries | Partial | Broadcast tests cover 202, 429/5xx retry, 4xx no retry, context cancellation |
| Bootstrap audit | Parent state, retry, partial unhealthy, WoC reconciliation | Blocked | Partial L2 never reaches ready; reconciliation gates `l2_done` |
| Slack notifier | No-op safe, dedupe, failure/recovery, notable reorg posts | Blocked | No webhook never panics; webhook payload includes required operational context |
| Graceful shutdown | Cancel, server shutdown, goroutine wait, DB close | Partial | Shutdown drains or times out cleanly without hanging on bootstrap failure |
| External config | Correct env names/defaults, redacted status | Blocked | Defaults match `PLAN.md`; invalid envs are surfaced, not silently ignored |
| Security | Constant-time auth, body limit, content-type, auth metric | Partial | Bad auth logs warning and increments metric; POSTs enforce JSON and 1 KiB limit |
| SSE hardening | Backoff, 1 MiB buffer, idle reconnect, labeled metrics, reorg path | Partial | Tip/reorg streams reconnect independently and metrics are per stream |
| Critical tests | Tx builders, fuzz, engine bootstrap, pause/resume, panic recovery | Partial | Adds persistence/restart, metric, readiness, and reconciliation tests |
| Deployment/docs | Working Docker image, accurate README/RUNBOOK, CI | Blocked | Docker healthcheck works or is removed; docs match real endpoints |

## Required Remediation Sequence

1. Stabilize persistence semantics.
   - Define the store API for current UTXOs, pending broadcasts, and bootstrap meta.
   - Make queue restore idempotent and single-owned.
   - Persist active chain tips after every successful sustain broadcast.
   - Add crash/restart tests before touching readiness.

2. Fix event accounting.
   - Make engine state recording side-effect free for Prometheus counters.
   - Count `txgen_reorg_total` exactly once per SSE reorg event.
   - Add a regression test that injects one reorg and asserts one metric increment.

3. Complete metrics.
   - Add missing collectors: `txgen_chain_terminated_total`,
     `txgen_auth_fail_total`, `txgen_sse_event_total{stream}`,
     `txgen_sse_disconnect_total{stream}`, `txgen_sse_connected{stream}`.
   - Update call sites to pass `stream`.

4. Complete bootstrap audit.
   - Persist `bootstrap_stage`.
   - Track parent states through pending, building, broadcasting, accepted, failed, and
     unknown.
   - Reconcile expected L2 outputs with WoC before `l2_done`.
   - Keep readiness false for `none`, `l1_done`, `l2_partial`, failed, or unknown.

5. Harden config and docs.
   - Correct defaults and env names.
   - Update README/RUNBOOK endpoint references to `POST /config`.
   - Document TLS as reverse-proxy delegated.

6. Fix Docker.
   - Remove invalid healthcheck or implement a real binary health mode.
   - Confirm `docker run` starts with `ADMIN_TOKEN` and a writable `/data` volume.

7. Clean lifecycle behavior.
   - Failed bootstrap must return or remain explicitly failed without blocking shutdown.
   - Safe goroutine wrappers should use structured logging or the configured logger
     where practical.

## Production Acceptance Tests

Run these on a clean checkout after all fixes:

```sh
go mod tidy
git diff --exit-code -- go.mod go.sum
gofmt -w .
go test ./...
go test -race ./... -count=1
go test -run '^$' -fuzz=FuzzBuildSustainTx -fuzztime=30s
go vet ./...
docker build -t bsv-tx-gen:prod-check .
```

Manual runtime checks:

```sh
ADMIN_TOKEN=test STATE_PATH="$(mktemp -d)/state.db" LOG_FORMAT=json go run .
curl -f http://localhost:8080/healthz
curl -f -H 'Authorization: Bearer test' http://localhost:8080/status
curl -f http://localhost:8080/metrics
curl -f -X POST http://localhost:8080/config \
  -H 'Authorization: Bearer test' \
  -H 'Content-Type: application/json' \
  -d '{"tps":0}'
```

Persistence acceptance:

- Start with a seeded queue and writable `STATE_PATH`.
- Run at low TPS until at least one sustain transaction succeeds.
- Kill the process without graceful shutdown.
- Restart with the same `STATE_PATH`.
- Assert the queue restores current chain tips only once, with no duplicate old UTXOs
  and no empty active state.

Reorg acceptance:

- Inject one valid reorg event into the SSE handler.
- Assert `EngineState.LastReorg` updates once.
- Assert exactly one `txgen_reorg_total{depth=...}` increment.
- Assert Slack-disabled mode logs and does not panic.
- Assert Slack-enabled mode posts one notable-event payload subject to rate limit.

Readiness acceptance:

- Bootstrap `none`, `l1_done`, `l2_partial`, and `failed` all return non-200 from
  `/readyz`.
- `l2_done` with active chains and reachable Arcade returns 200.
- Consecutive Arcade failures or zero active chains returns non-200.

Docker acceptance:

- `docker build` succeeds.
- `docker run -e ADMIN_TOKEN=test -e STATE_PATH=/data/state.db -v "$PWD/data:/data"`
  starts cleanly.
- If a Docker healthcheck exists, `docker inspect` shows healthy after startup.
- The healthcheck must not depend on `/bin/sh`, `curl`, or any binary absent from the
  final distroless image.

## Final Delivery Gate

`PLAN.md` is successfully delivered only when:

- All blocking findings in this document are closed.
- The delivery matrix has no `Blocked` or `Partial` rows.
- The production acceptance tests pass.
- README and RUNBOOK match the real API and runtime behavior.
- A reviewer can reproduce crash/restart, readiness, metrics, Docker, and reorg
  acceptance without reading implementation internals.
