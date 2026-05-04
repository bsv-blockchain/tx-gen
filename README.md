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
- Node 22.6+ for the funding script
- BSV Desktop running locally with enough wallet funds for a fresh tagged-drop setup, or an existing BSV mainnet UTXO locked to the configured script for resume
- Enough satoshis to sustain your intended chain length. The funding script estimates this for tagged-drop mode; P2PKH mode uses `value / 20` transactions per chain by default.
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

## Running The System

Use two terminals for a new tagged-drop run.

Terminal 1 starts the server:

```sh
source .env
./bsv-tx-gen
```

On startup the service logs its scripthash and checks WhatOnChain for matching UTXOs. For a fresh instance, no UTXO exists yet, so the server stays up and waits for the initial funding output. The server must be running before the funding script can apply its computed `POST /config` values.

Terminal 2 runs the funding setup:

```sh
npm install
source .env
npm run fund
```

For a deployed tx-gen instance, pass the service URL as the optional first argument:

```sh
ADMIN_TOKEN=secret INSTANCE_ID=deggen npm run fund https://tx-gen.example.com
```

The URL is the tx-gen server URL, not the Arcade URL. If no URL is provided, the script uses `TXGEN_BASE_URL` or defaults to `http://localhost:8080`.

## Initial Funding

The funding script creates the first tagged-drop UTXO from a local BSV Desktop wallet, posts the server configuration to tx-gen, and broadcasts the wallet transaction to Arcade as EF.

Flow:

1. Prompts for target TPS and run duration.
2. Derives `NUM_CHAINS` from `TPS * UTXO_IDLE_SECONDS`; default idle target is 600 seconds.
3. Derives `FANOUT_SIZE` from roughly `sqrt(NUM_CHAINS)` unless `FANOUT_SIZE` is explicitly set.
4. Validates planned bootstrap fanout transactions against `MAX_BOOTSTRAP_TX_BYTES`, default `100000000`.
5. Applies `tps`, `numChains`, `fanoutSize`, and `sustainFee` to the running tx-gen server with authenticated `POST /config`.
6. Requests BSV Desktop wallet funding for the initial tagged-drop output.
7. Converts the returned Atomic BEEF with `Transaction.fromBEEF(tx).toEF()` and posts it to `${ARCADE_BASE_URL}/tx`.
8. The running tx-gen server detects the funded output through WhatOnChain, bootstraps the configured chains, and begins sustaining the requested TPS.

Requires Node 22.6+, a local BSV Desktop wallet, `INSTANCE_ID`, and `ADMIN_TOKEN` when applying config to a running server. If `ADMIN_TOKEN` is missing, the script prints the config payload but cannot apply it. If the tx-gen server is not reachable, the config POST fails before wallet funding. Set `SKIP_TXGEN_CONFIG=1` only when you intend to apply matching server config yourself.

Optional env vars: `TXGEN_BASE_URL`, `ADMIN_TOKEN`, `SKIP_TXGEN_CONFIG`, `ARCADE_BASE_URL`, `ARCADE_CALLBACK_TOKEN`, `WALLET_ORIGINATOR`, `FUNDING_BASKET`, `FANOUT_SIZE` (override), `UTXO_IDLE_SECONDS`, `SUSTAIN_FEE`, `MAX_BOOTSTRAP_TX_BYTES`.

## Local Monitoring

Docker Compose starts Prometheus and Grafana alongside the service:

```sh
docker compose up -d --build
```

- Grafana: http://localhost:3000, default `admin` / `admin`
- Prometheus: http://localhost:9090
- tx-gen metrics: http://localhost:8080/metrics

Grafana is provisioned with the `tx-gen Overview` dashboard under the `Local` folder. Override ports or Grafana credentials with `GRAFANA_PORT`, `PROMETHEUS_PORT`, `GRAFANA_ADMIN_USER`, and `GRAFANA_ADMIN_PASSWORD`.

## Runtime Config

```sh
curl -X POST http://localhost:8080/config \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"tps": 16, "numChains": 9600, "fanoutSize": 100, "sustainFee": 7}'
```

All fields are optional, but at least one must be supplied. `tps` can be changed at any time; setting it to `0` pauses all chains without losing state. Bootstrap fields (`numChains`, `fanoutSize`, `sustainFee`) are accepted only before the initial funding UTXO is detected. The funding script normally sends these bootstrap fields for you.

After bootstrap, use TPS-only updates:

```sh
curl -X POST http://localhost:8080/config \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"tps": 16}'
```

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
| `FANOUT_SIZE` | `100` | Max outputs per bootstrap fanout transaction. The funding script derives roughly `sqrt(NUM_CHAINS)` unless this is set explicitly. |
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
- `POST /config` — Set TPS and pre-bootstrap parameters (auth: Bearer ADMIN_TOKEN, body can include `tps`, `numChains`, `fanoutSize`, `sustainFee`)
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
