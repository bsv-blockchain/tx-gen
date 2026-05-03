# CLAUDE Agent Plan — Core Engine, Queue, Transaction Logic & Tests

**Scope**: The heart of the generator — transaction builders, engine state machine, rate controller, bootstrap logic, queue behavior with persistence integration, and the critical-path tests. You own the domain model and the algorithms that must be correct.

**Parallel Execution Rule**: You may only edit files listed below. Do not touch config.go, store.go, metrics.go, safe.go, httpclient.go, notifier.go, server.go, arcade.go, woc.go, sse.go, main.go, Dockerfile, or any docs/CI files. Define the public interfaces (Broadcaster, UTXOSource, StateProvider) that GROK and CODEX will implement against.

---

## 1. Engine State Model (`engine.go`)
- Introduce `EngineState` struct (or snapshot) owned by Engine:
  - lifecycle: starting | bootstrapping | paused | running | degraded | stopping | stopped | failed
  - requestedTPS, acceptedTPS (recent window)
  - chainsActive, chainsTarget, chainsExhausted
  - bootstrapStage (none / l1_done / l2_done), bootstrapFailures
  - lastBroadcastOk, consecutiveArcadeFailures
  - lastWoC time/result
  - sse: {connected, tipHeight, tipTime, lastReorg {height, depth, time, chainsImpacted}}
  - lastError {component, msg, time}
- Methods: `Snapshot() EngineState`, `SetState(...)`, `RecordReorg(height, depth int, tipBefore, tipAfter string)`.
- This snapshot is read by server.go (CODEX) for `/status` and by notifier (GROK) for alerts.

## 2. Central Rate Controller & TPS Control (`engine.go`)
- Replace per-chain `time.After` with a single controller + `notify` channel (or cond).
- On `SetTPS(n)`: update target, wake all chains immediately (even from long sleep).
- Calculate per-chain interval from current `chainsActive` (not hard-coded 10000).
- Track attempted/accepted/failed TPS; degrade gracefully if requested > capacity.
- Surface under-delivery as `degraded` state (GROK notifier will alert).

## 3. Bootstrap as Explicit State Machine (`engine.go`)
- Track each parent: pending | building | broadcasting | accepted | failed.
- Retry transient failures with backoff.
- Persist stage in `meta` bucket via store (GROK).
- After L2, reconcile with WoC and mark unhealthy on partial completion.
- Do not proceed to running if L2 not fully successful.

## 4. Queue with Write-Through Persistence (`queue.go`)
- Keep min-heap in memory for ordering.
- Every `Push` / `Pop` also calls the corresponding store method (GROK's `store.go`).
- `Restore()` at startup loads from BoltDB; if non-empty, skip WoC fetch.
- Pending broadcast records: write before broadcast, clear on success (crash recovery trusts Arcade 202).
- Expose `Len()`, `Pop()`, `Push(UTXO)` for engine.

## 5. Transaction Builders (`tx.go`)
- Keep `buildFanoutTx`, `buildSustainTx`, `EncodeEF`, locking/unlocking scripts unchanged unless tests prove wrong.
- Export any internals needed by tests (e.g. `buildFanoutTx` for table tests).
- No behavioral change to fee model or output count.

## 6. Safe Goroutine Usage
- Every `go ...` in engine (chain runners, bootstrap workers) must use `safe.SafeGo(...)` from GROK's `safe.go`.
- Panic in one chain must not kill the engine.

## 7. Interfaces for Parallel Agents (`engine.go`)
- Define and export:
  ```go
  type Broadcaster interface {
      Broadcast(ctx context.Context, ef []byte) error
  }
  type UTXOSource interface {
      FetchUTXOs(ctx context.Context, scriptHash string) ([]UTXO, error)
  }
  ```
- Engine accepts these as dependencies (CODEX will provide Arcade + WoC impls).
- Also expose `StateProvider` or just let server read the snapshot.

## 8. Critical-Path Tests (`tx_test.go`, `engine_test.go` new)
- `tx_test.go` (table + fuzz):
  - `buildFanoutTx` various inputs → correct output count, value split, valid EF.
  - `buildSustainTx` normal, chain-end (value==fee), zero-value error, bad txid error.
  - `FuzzBuildSustainTx` — no panics, new value ≤ old, EF non-empty.
- `engine_test.go`:
  - `fakeBroadcaster` + `fakeUTXOSource` for injection.
  - `bootstrapFull` (NUM_CHAINS=4, FANOUT=2) → 3 broadcasts, queue ends with 4 UTXOs.
  - Resume from `meta=l1_done`.
  - Chain termination after N sustains.
  - Pause/resume via `SetTPS(0)` then `>0` (immediate wake).
  - Panic in broadcaster recovered by SafeGo, engine still alive, panic counter incremented.

---

## Files (CLAUDE owns these exclusively)

**Modified**:
- engine.go
- queue.go
- tx.go

**New**:
- tx_test.go
- engine_test.go

**Interfaces you define** (others implement):
- `Broadcaster`
- `UTXOSource`
- `EngineState` snapshot (consumed by GROK notifier/metrics and CODEX server)

**Do not edit**: config.go, store.go, metrics.go, safe.go, httpclient.go, notifier.go, main.go, server.go, arcade.go, woc.go, sse.go, Dockerfile, RUNBOOK.md, ARCHITECTURE.html, .github/workflows/ci.yml, README.md, .gitignore.

---

## Success Criteria (CLAUDE portion)
- All tx builders produce valid, correctly valued transactions.
- Engine state machine covers the full lifecycle including reorg recording.
- Queue survives restart via store without re-bootstrap when data present.
- TPS changes are immediate (no old sleep intervals).
- `go test ./...` passes (including fuzz for 30s with no panics).
- No file conflicts with GROK or CODEX agents.
- Bootstrap is retryable and leaves the system unhealthy on partial L2.

---

**Execution Note**: Stabilize the interfaces (`Broadcaster`, `EngineState`, `SetTPS` signature) in the first 20% of your work so GROK and CODEX can proceed in parallel. Use `safe.SafeGo` once GROK publishes it.
