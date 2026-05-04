# bsv-tx-gen

High-throughput BSV transaction generator. Bootstraps a configurable number of independent UTXO chains from a single funded output, then drives them at a configurable TPS rate.

## What it does

1. **Bootstrap** — fetches unspent outputs for the configured locking script (scripthash lookup via WhatOnChain), then fans out:
   - 1 UTXO → 1 tx → `ceil(NUM_CHAINS/FANOUT_SIZE)` L1 outputs
   - L1 outputs → parallel fanout txs → `NUM_CHAINS` chain outputs
2. **Sustain** — runs `NUM_CHAINS` independent chains. Each chain fires one transaction per `NUM_CHAINS/TPS` seconds. Every tx spends the configured sustain fee; the chain ends when its output reaches 0.
3. **Resume** — if the queue already has UTXOs (from a previous run), skips bootstrap and resumes chains directly.

By default transactions use an instance-tagged push/drop locking script and `OP_TRUE` unlocking script. The lock script is `OP_PUSHBYTES_6 <INSTANCE_ID normalized to six UTF-8 bytes> OP_DROP`, so each instance has a distinct scripthash. If `PRIVATE_KEY` is set, the service switches to P2PKH: outputs lock to the key's mainnet address, inputs are signed with that key, and WhatOnChain lookup uses the P2PKH scripthash. Broadcast uses [Arcade v2](https://arcade-v2-us-1.bsvblockchain.tech) in Extended Format (EF). Chain tip and reorg events are logged via SSE.

## Prerequisites

- Go 1.25+
- A BSV mainnet UTXO locked to the configured tagged-drop script, or a P2PKH UTXO for the configured `PRIVATE_KEY`
- Enough satoshis to sustain your intended chain length (`value / 7` transactions per chain for tagged-drop, `value / 20` for default P2PKH)
- Network access to WhatOnChain and the Arcade v2 node

## Setup

```sh
git clone <repo>
cd bsv-tx-gen
cp .env.example .env
# edit .env: set ADMIN_TOKEN and INSTANCE_ID
go build -o bsv-tx-gen .
```

## Configuration

| Env var | Required | Default | Description |
|---|---|---|---|
| `ADMIN_TOKEN` | yes | — | Bearer token for the `/config` endpoint |
| `INSTANCE_ID` | yes | — | Per-instance tag used in logs/alerts and, in default mode, normalized to exactly six UTF-8 bytes for the tagged-drop lock script. |
| `PORT` | no | `8080` | HTTP listen port |
| `PRIVATE_KEY` | no | empty | Enables P2PKH mode when set. Accepts WIF or 32-byte hex private keys. |
| `SUSTAIN_FEE` | no | `7` or `20` | Satoshis per sustain hop. Defaults to `7` for tagged-drop and `20` for P2PKH. |
| `ARCADE_CALLBACK_TOKEN` | no | empty | Enables Arcade transaction-event SSE. Broadcasts include this as `X-CallbackToken`; the service subscribes to `/events?callbackToken=...`. |

## Running

```sh
source .env
./bsv-tx-gen
```

On startup the service logs its scripthash, fetches UTXOs, and begins bootstrap (or resumes). TPS starts at 0 — no transactions are sent until you set a rate.

## Initial Funding

Create the first tagged-drop UTXO from a local BSV Desktop wallet, then post it to Arcade as EF:

```sh
npm install
INSTANCE_ID=deggen npm run fund:initial
```

Requires Node 22.6+. The script also loads `.env` from the repo root. It prompts for target TPS and duration, derives `NUM_CHAINS` from `TPS * UTXO_IDLE_SECONDS` (default `600`, or 10 minutes), estimates the initial output value using `FANOUT_SIZE` and `SUSTAIN_FEE`, asks BSV Desktop to create a no-send wallet action, converts the returned Atomic BEEF with `Transaction.fromBEEF(tx).toEF()`, and posts it to `${ARCADE_BASE_URL}/tx`. The printed `NUM_CHAINS` and `FANOUT_SIZE` values should be used when starting the Go service.

Optional env vars: `ARCADE_BASE_URL`, `ARCADE_CALLBACK_TOKEN`, `WALLET_ORIGINATOR`, `FUNDING_BASKET`, `FANOUT_SIZE`, `UTXO_IDLE_SECONDS`, `SUSTAIN_FEE`.

## Local Monitoring

Docker Compose starts Prometheus and Grafana alongside the service:

```sh
docker compose up -d --build
```

- Grafana: http://localhost:3000, default `admin` / `admin`
- Prometheus: http://localhost:9090
- tx-gen metrics: http://localhost:8080/metrics

Grafana is provisioned with the `tx-gen Overview` dashboard under the `Local` folder. Override ports or Grafana credentials with `GRAFANA_PORT`, `PROMETHEUS_PORT`, `GRAFANA_ADMIN_USER`, and `GRAFANA_ADMIN_PASSWORD`.

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
| Fee per hop | 7 sat tagged-drop, 20 sat P2PKH | 7 or 20 sat |
| Chain length | `value / fee` txs | ~142 txs at 7 sat |
| Interval per chain at 16 TPS and 9600 chains | `9600 / 16` = 600 s | 10 min |
| Sustained TPS with N chains | `NUM_CHAINS / interval` | 16 TPS when `9600 / 600` |

## Docker

Multi-stage build to distroless image.

```sh
docker build -t bsv-tx-gen .
docker run -d -p 8080:8080 -v $(pwd)/data:/data \
  -e ADMIN_TOKEN=your-secret \
  -e INSTANCE_ID=runner1 \
  -e STATE_PATH=/data/state.db \
  -e SLACK_WEBHOOK_URL=https://hooks.slack... \
  bsv-tx-gen
```

Volume mount for `state.db` persistence across restarts. Terminate TLS at a reverse proxy such as Caddy or nginx; the service itself listens HTTP.
Use a separate `STATE_PATH` when switching between tagged-drop and P2PKH modes, between different `INSTANCE_ID` values, or between different P2PKH keys.

## Full Environment Variables

See RUNBOOK.md for complete table. Key ops vars:

| Env var | Default | Notes |
|---------|---------|-------|
| `STATE_PATH` | `./state.db` | BoltDB location; backup before restart |
| `NUM_CHAINS` | `10000` | Target chain count. The funding script derives this from target TPS and `UTXO_IDLE_SECONDS`. |
| `FANOUT_SIZE` | `100` | Max outputs per bootstrap fanout transaction. |
| `ARCADE_CALLBACK_TOKEN` | (empty) | Enables Arcade tx event SSE and `X-CallbackToken` broadcast header |
| `PRIVATE_KEY` | (empty) | Enables P2PKH lock/unlock and P2PKH scripthash lookup |
| `SUSTAIN_FEE` | `7` or `20` | Satoshis per sustain hop; P2PKH default is `20` |
| `MAX_TPS` | `10000` | Hard cap (degrade if requested > capacity) |
| `SLACK_WEBHOOK_URL` | (empty) | Enables alerts + reorg case-study posts |
| `ENVIRONMENT` | `production` | Tags all Slack / logs |
| `INSTANCE_ID` | required | Unique per deployment; default lock script uses the first six UTF-8 bytes, padded with spaces if shorter |

## Endpoints

- `GET /healthz` — 200 liveness (always after start)
- `GET /readyz` — 200 after bootstrap complete
- `GET /status` — EngineState snapshot (TPS, chains, lastReorg, bootstrapStage, lastError)
- `GET /arcade/tx/{txid}` — Authenticated proxy to Arcade's transaction status endpoint for a specific txid
- `GET /metrics` — Prometheus (txgen_broadcast_total, txgen_reorg_total{depth}, queue_depth, chains_active, tps_target, panic_total etc.)
- `POST /config` — SetTPS (auth: Bearer ADMIN_TOKEN, body `{"tps": N}`)
- `POST /stop` — convenience alias for `{"tps":0}` (optional)
 
## Ops Quick Reference
 
- Pause: `POST /config -d '{"tps":0}'`
- Resume: `POST /config -d '{"tps":16}'`
- Check degraded: `curl /status | jq .state`
- Check Arcade status: `curl -H "Authorization: Bearer $ADMIN_TOKEN" /arcade/tx/<txid>`
- Reorg evidence: logs "reorg", `/metrics` txgen_reorg_total, Slack post (if webhook), `/status.lastReorg`
- Restart with resume: keep `state.db` (active tips + structured pending replay, no re-bootstrap after `l2_done`)
- Bootstrap `l2_partial` or `unknown`: keep `state.db`, check `/status.lastError`, and reconcile with WoC before restarting generation

When `ARCADE_CALLBACK_TOKEN` is set, broadcasts include `X-CallbackToken` and the service subscribes to Arcade transaction events at `/events?callbackToken=...`. Those events are logged as `Arcade tx SSE` with the raw Arcade payload plus parsed `txid` and `tx_status` when present. Immediate `/tx` broadcast responses are logged at debug level and status can also be checked later with `GET /arcade/tx/{txid}`.

## Reorg Case-Study Note

Every mainnet reorg produces:
- Structured info log with height/depth/chains/tips
- `txgen_reorg_total{depth=...}` increment
- Optional Slack informational post (different channel tone)
- Snapshot in EngineState for `/status`

This data is rare and valuable — collected automatically for post-mortem analysis.
