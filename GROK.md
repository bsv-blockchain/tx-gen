# GROK Agent Plan — Ops, Persistence, Observability, Safety, Deployment

**Scope**: All infrastructure, persistence, configuration, metrics, safety wrappers, Slack notifier (including reorg case-study capture), Docker packaging, runbook, architecture visualization, CI, and dependency management.

**Parallel Execution Rule**: You may only edit files listed in the "Files" section below. Do not touch engine.go, queue.go, tx.go, server.go, arcade.go, woc.go, sse.go, or main.go. Coordinate via interfaces defined in engine.go (Broadcaster, StateSnapshot, etc.) and the public API of the notifier.

---

## 1. External Configuration (`config.go` new)
- Create `Config` struct with all runtime values from env (ADMIN_TOKEN required, PORT, STATE_PATH, LOG_*, ARCADE_BASE_URL, WOC_BASE_URL, NUM_CHAINS, FANOUT_SIZE, SUSTAIN_FEE, MAX_TPS, BROADCAST_CONCURRENCY, BROADCAST_RETRY_MAX, HTTP_TIMEOUT, SSE_RECONNECT_*, SLACK_WEBHOOK_URL, SLACK_CHANNEL, ENVIRONMENT, INSTANCE_ID).
- Provide `LoadConfig() (*Config, error)` using `os.Getenv` + helpers. No extra deps.
- Redact secrets in `String()` / JSON for `/status`.
- Pass `*Config` to all components.

## 2. BoltDB Persistence (`store.go` new)
- Open `bbolt.DB` at `STATE_PATH` (default `./state.db`).
- Buckets: `utxos`, `meta`, `pending`.
- `Queue` (owned by CLAUDE) will call write-through methods; implement:
  - `SaveUTXO(utxo UTXO) error`
  - `DeleteUTXO(txid string, vout uint32) error`
  - `LoadAll() ([]UTXO, error)`
  - `GetMeta(key string) (string, error)`
  - `SetMeta(key, val string) error`
  - `SavePending(txid string, ef []byte) error`
  - `ClearPending(txid string) error`
  - `Close() error`
- UTXOs and pending records encoded with `gob`.
- `Restore()` used by engine at startup.

## 3. Prometheus Metrics (`metrics.go` new)
- Define all collectors from original plan + new `txgen_reorg_total{depth}`.
- `RegisterMetrics(mux *http.ServeMux)` or return a handler.
- Expose at `GET /metrics`.
- Include helpers: `IncBroadcast(kind, result)`, `ObserveBroadcastLatency(...)`, `SetChainsActive(n)`, `IncReorg(depth)`, etc.
- Gauges for queue depth, active chains, SSE connected, TPS target.

## 4. Safe Goroutine Wrapper (`safe.go` new)
- `func SafeGo(wg *sync.WaitGroup, name string, fn func())`
- Wraps `fn` with `defer recover()`, logs stack at error level, increments `txgen_panic_total{goroutine=name}`.
- Replaces every bare `go ...` in the system (CLAUDE and CODEX will call this).

## 5. Tuned HTTP Clients (`httpclient.go` new)
- `ArcadeTransport() *http.Transport` — MaxIdleConnsPerHost=128, MaxIdleConns=256, IdleConnTimeout=90s, ForceAttemptHTTP2, DialContext 5s/30s, TLS 5s, ResponseHeader 10s.
- `SSETransport() *http.Transport` — separate, ResponseHeaderTimeout=0, larger buffers.
- `NewHTTPClient(transport *http.Transport, timeout time.Duration) *http.Client`

## 6. Slack Notifier with Reorg Case Studies (`notifier.go` new)
- Configured by `SLACK_WEBHOOK_URL`, `SLACK_CHANNEL`, `ENVIRONMENT`, `INSTANCE_ID`.
- No-op when webhook unset.
- Two modes:
  - Problem alerts (deduped, with recovery messages) for the failure conditions in original plan.
  - Notable events: `ReportReorg(height, depth, chainsImpacted int, tipBefore, tipAfter string)` — always logs structured info; posts concise Slack message (different emoji/channel tone) when webhook present. This is for mainnet reorg case-study collection.
- Rate limit both types independently.
- Payload always includes TPS, active chains, instance id, timestamp.
- Expose `Notifier` interface or concrete type for engine to call.

## 7. Architecture Diagram (`ARCHITECTURE.html` new)
- Self-contained dark-themed HTML + SVG (use architecture-diagram skill patterns).
- Show: Engine (state machine) → persistent Queue (bbolt) → 10k SafeGo chains → tuned HTTP client → Arcade.
- Sidecars: WoC UTXO source, SSE tip/reorg streams, Prometheus, Slack notifier.
- Highlight reorg path and "case study" capture.
- Include legend, summary cards, and metadata.

## 8. Dockerfile & Docker Support (new files + README)
- Multi-stage: `golang:1.25-alpine` build (CGO=0, -trimpath, ldflags with version) → `gcr.io/distroless/static-debian12:nonroot`.
- `EXPOSE 8080`, `VOLUME /data`, `ENTRYPOINT ["/bsv-tx-gen"]`.
- Add `HEALTHCHECK` using `/healthz`.
- `.dockerignore`: state.db, binary, .env, .git.
- Update README with Docker run example including volume mount for state.db and all env vars.
- Add `docker build` success to criteria.

## 9. CI Pipeline (`.github/workflows/ci.yml` new)
- On push/PR: `go mod tidy`, `golangci-lint run`, `go test ./... -race -count=1`, `go build`, `docker build`.
- Cache Go modules.
- Fail fast on any step.

## 10. Repo Hygiene & go.mod
- Add `state.db`, `bsv-tx-gen`, `.env`, `ARCHITECTURE.html` (generated) to `.gitignore`.
- Delete any committed binary if present.
- `go.mod`: add `go.etcd.io/bbolt`, `github.com/prometheus/client_golang`.
- Run `go mod tidy` and commit the result.

## 11. Operator Runbook (`RUNBOOK.md` new)
- Document:
  - All env vars (required/optional).
  - Start/stop (bare metal + Docker).
  - Setting TPS, checking `/healthz` `/readyz` `/status` `/metrics`.
  - Slack alert vs notable-event meanings (especially reorg case-study posts).
  - Backup/restore of `state.db` (before restart, periodic rsync or volume snapshot).
  - Reorg handling: expected behavior, where logs and Slack messages appear, confirmation that chains continue.
  - Expected degraded states and first actions.
  - How to collect reorg evidence for case studies.

## 12. README Updates (ops sections only)
- Add Docker section, full env table, endpoint docs (`/metrics`, `/healthz`, `/readyz`, `/status`), ops quick reference, reorg case-study note.

## 13. Reorg Case-Study Capture (cross-cutting)
- Ensure notifier + metrics + engine state work together so every mainnet reorg produces:
  - Structured log (info level) with height, depth, chainsImpacted.
  - `txgen_reorg_total{depth}` increment.
  - Optional Slack informational post.
- This data is rare and valuable — make it first-class.

---

## Files (GROK owns these exclusively)

**New**:
- config.go
- store.go
- metrics.go
- safe.go
- httpclient.go
- notifier.go
- Dockerfile
- .dockerignore
- RUNBOOK.md
- ARCHITECTURE.html
- .github/workflows/ci.yml

**Modified**:
- go.mod / go.sum
- README.md (Docker, ops, endpoints, reorg section)
- .gitignore

**Interfaces you implement / consume**:
- `Notifier` (provide to engine)
- `Metrics` helpers (called by CLAUDE engine and CODEX sse/server)
- `SafeGo` (called by CLAUDE and CODEX)

**Do not edit**: engine.go, queue.go, tx.go, main.go, server.go, arcade.go, woc.go, sse.go, tx_test.go, engine_test.go.

---

## Success Criteria (GROK portion)
- `docker build` produces working image with HEALTHCHECK.
- `go test ./...` + CI pipeline green.
- `/metrics` exposes all counters/gauges including `txgen_reorg_total`.
- Reorg events produce structured logs + optional Slack case-study posts.
- `state.db` survives restart with queue + pending replay.
- Runbook and ARCHITECTURE.html exist and are accurate.
- No file conflicts with CLAUDE or CODEX agents.

---

**Execution Note**: Implement in any order as long as interfaces are stable early. Notify the other agents when notifier and metrics packages are ready for import.
