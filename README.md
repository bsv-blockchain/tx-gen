# bsv-tx-gen

High-throughput BSV transaction generator. Bootstraps 10,000 independent UTXO chains from a single funded output, then drives them at a configurable TPS rate.

## What it does

1. **Bootstrap** — fetches unspent outputs locked with `OP_NOP4` (scripthash lookup via WhatOnChain), then fans out:
   - 1 UTXO → 1 tx → 100 outputs
   - 100 UTXOs → 100 txs → 10,000 outputs (runs in parallel)
2. **Sustain** — runs 10,000 independent chains. Each chain fires one transaction per `10000/TPS` seconds. Every tx spends 7 sats in fees; the chain ends when its output reaches 0.
3. **Resume** — if the queue already has UTXOs (from a previous run), skips bootstrap and resumes chains directly.

Transactions use `OP_NOP4` locking script and `OP_TRUE` unlocking script. Broadcast via [Arcade v2](https://arcade-v2-us-1.bsvblockchain.tech) in Extended Format (EF). Chain tip and reorg events are logged via SSE.

## Prerequisites

- Go 1.25+
- A BSV mainnet UTXO locked to `OP_NOP4` (0xB9) with enough satoshis to sustain your intended chain length (`value / 7` transactions per chain)
- Network access to WhatOnChain and the Arcade v2 node

## Setup

```sh
git clone <repo>
cd bsv-tx-gen
cp .env.example .env
# edit .env: set ADMIN_TOKEN
go build -o bsv-tx-gen .
```

## Configuration

| Env var | Required | Default | Description |
|---|---|---|---|
| `ADMIN_TOKEN` | yes | — | Bearer token for the `/config` endpoint |
| `PORT` | no | `8080` | HTTP listen port |

## Running

```sh
source .env
./bsv-tx-gen
```

On startup the service logs its scripthash, fetches UTXOs, and begins bootstrap (or resumes). TPS starts at 0 — no transactions are sent until you set a rate.

## Setting TPS

```sh
curl -X POST http://localhost:8080/config \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"tps": 16}'
```

Valid range: `0`–`10000`. Setting `0` pauses all chains without losing state.

## Chain math

| Metric | Formula | Example (1000 sat output) |
|---|---|---|
| Fee per hop | 7 sat | 7 sat |
| Chain length | `value / 7` txs | ~142 txs |
| Interval per chain at 16 TPS | `10000 / 16` ≈ 625 s | ~10 min |
| Sustained TPS with 10k chains | `10000 / interval` | 16 TPS |

## Docker

Multi-stage build to distroless image.

```sh
docker build -t bsv-tx-gen .
docker run -d -p 8080:8080 -v $(pwd)/data:/data \
  -e ADMIN_TOKEN=your-secret \
  -e STATE_PATH=/data/state.db \
  -e SLACK_WEBHOOK_URL=https://hooks.slack... \
  bsv-tx-gen
```

Volume mount for `state.db` persistence across restarts.

## Full Environment Variables

See RUNBOOK.md for complete table. Key ops vars:

| Env var | Default | Notes |
|---------|---------|-------|
| `STATE_PATH` | `./state.db` | BoltDB location; backup before restart |
| `NUM_CHAINS` | `10000` | Target chains (fanout * sustain) |
| `FANOUT_SIZE` | `100` | L1 fanout width |
| `SUSTAIN_FEE` | `7` | Satoshis per sustain hop |
| `MAX_TPS` | `10000` | Hard cap (degrade if requested > capacity) |
| `SLACK_WEBHOOK_URL` | (empty) | Enables alerts + reorg case-study posts |
| `ENVIRONMENT` | `production` | Tags all Slack / logs |
| `INSTANCE_ID` | `local` | Unique per deployment for dedupe |

## Endpoints

- `GET /healthz` — 200 liveness (always after start)
- `GET /readyz` — 200 after bootstrap complete
- `GET /status` — EngineState snapshot (TPS, chains, lastReorg, bootstrapStage, lastError)
- `GET /metrics` — Prometheus (txgen_broadcast_total, txgen_reorg_total{depth}, queue_depth, chains_active, tps_target, panic_total etc.)
- `POST /config` — SetTPS (auth: Bearer ADMIN_TOKEN, body `{"tps": N}`)
- `POST /stop` — convenience alias for `{"tps":0}` (optional)
 
## Ops Quick Reference
 
- Pause: `POST /config -d '{"tps":0}'`
- Resume: `POST /config -d '{"tps":16}'`
- Check degraded: `curl /status | jq .state`
- Reorg evidence: logs "reorg", `/metrics` txgen_reorg_total, Slack post (if webhook), `/status.lastReorg`
- Restart with resume: keep `state.db` (queue + pending replay, no re-bootstrap)

## Reorg Case-Study Note

Every mainnet reorg produces:
- Structured info log with height/depth/chains/tips
- `txgen_reorg_total{depth=...}` increment
- Optional Slack informational post (different channel tone)
- Snapshot in EngineState for `/status`

This data is rare and valuable — collected automatically for post-mortem analysis.
