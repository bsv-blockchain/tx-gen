# tx-gen Runbook

Operating guide for running **tx-gen** unattended against BSV mainnet: starting and funding an instance, controlling the rate, reading its health, and recovering after crashes and reorgs.

For the full list of settings, see [Configuration](../README.md#-configuration) in the README.

<br/>

## 🚀 Start and Stop

**Binary**

```bash
ADMIN_TOKEN=secret INSTANCE_ID=runner1 tx-gen
```

**Docker** (the image stores state in the `/data` volume by default)

```bash
docker build -t tx-gen .
docker run -d --name tx-gen -p 8080:8080 -v tx-gen-data:/data \
  -e ADMIN_TOKEN=secret -e INSTANCE_ID=runner1 tx-gen
```

**Stop** with `SIGINT` or `SIGTERM` (`docker stop`). Shutdown is graceful: the HTTP server drains for up to 5s, chains and streams stop within 30s, then the state file is closed. Chains resume from their last accepted tips on the next start.

The startup log confirms the lock script and how the engine will start:

```text
lock script ready            mode=tagged_drop script_hash=<hash> sustain_fee=7
restored UTXOs               count=9600        # or: fetching UTXOs source=whatsonchain
engine starting              utxos=9600
resuming existing chains     utxos=9600        # or: waiting for funding UTXO / bootstrap: L1 and L2 fanout
chains started               count=9600
```

<br/>

## 💰 Fund a New Instance

A new instance has no UTXOs. It starts, logs its script hash, and polls WhatsOnChain every 10s for the funding output. Use the funding script while the server is running:

```bash
npm install
ADMIN_TOKEN=secret INSTANCE_ID=runner1 npm run fund                       # local server
ADMIN_TOKEN=secret INSTANCE_ID=runner1 npm run fund https://tx-gen.example  # deployed server
```

The script asks for the target TPS and run duration, derives `numChains` (TPS × `UTXO_IDLE_SECONDS`, default 600) and `fanoutSize` (about √numChains), applies them with `POST /config`, then funds the tagged-drop output from a local BSV Desktop wallet and broadcasts it to Arcade. The server detects it, bootstraps, and starts generating.

- The URL is the **tx-gen** server, not Arcade. It defaults to `TXGEN_BASE_URL`, then `http://localhost:8080`.
- Bootstrap parameters (`numChains`, `fanoutSize`, `sustainFee`) are accepted only **before** the funding UTXO is detected. Afterwards `POST /config` answers `409 Conflict` for them.
- `SKIP_TXGEN_CONFIG=1` skips the config call when you apply matching values yourself.
- Optional variables: `TXGEN_BASE_URL`, `ARCADE_BASE_URL`, `ARCADE_CALLBACK_TOKEN`, `WALLET_ORIGINATOR`, `FUNDING_BASKET`, `FANOUT_SIZE`, `UTXO_IDLE_SECONDS`, `SUSTAIN_FEE`, `MAX_BOOTSTRAP_TX_BYTES`.

P2PKH mode (`PRIVATE_KEY` set) is funded by sending a normal payment to the address in the `lock script ready` log line.

<br/>

## 🎚️ Control the Rate

```bash
# Start or change the rate (takes effect immediately, even for chains mid-sleep)
curl -X POST localhost:8080/config -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" -d '{"tps":16}'

# Pause every chain without losing state
curl -X POST localhost:8080/stop -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json"
```

| TPS | Effect |
|-----|--------|
| `0` | Paused; chains wait without broadcasting |
| `NUM_CHAINS / 600` | Each chain broadcasts once every 10 minutes (the funding script's default plan) |
| up to `MAX_TPS` | Higher rates shorten each chain's interval and exhaust chains sooner |

Each chain broadcasts every `chainsActive / tps` seconds, so the target is met by however many chains are still running.

<br/>

## 🩺 Health and Status

| Check | Command | Healthy |
|-------|---------|---------|
| Liveness | `curl localhost:8080/healthz` | `200 {"status":"ok"}` |
| Readiness | `curl localhost:8080/readyz` | `200` with `"ready":true` |
| Full state | `curl -H "Authorization: Bearer $ADMIN_TOKEN" localhost:8080/status \| jq .engine` | `lifecycle` is `running` or `paused` |
| One transaction | `curl -H "Authorization: Bearer $ADMIN_TOKEN" localhost:8080/arcade/tx/<txid>` | Arcade's status for that txid |

`/readyz` is ready only when the engine is running, `bootstrapStage` is `l2_done`, at least one chain is active, the last broadcast succeeded, and Arcade's `/health` answers. Its `checks` object shows which condition failed.

**Lifecycle states**

| State | Meaning | Action |
|-------|---------|--------|
| `starting` / `bootstrapping` | Waiting for funding, or fanning out | Wait; check `lastWoCResult` and the logs |
| `running` | Broadcasting at the target TPS | None |
| `paused` | TPS is 0 | Set a TPS to resume |
| `degraded` | 5+ consecutive broadcast failures | Check Arcade health and `lastError`; clears on the next success |
| `failed` | Bootstrap failed; no chains run | See [Recovery](#-recovery) |
| `stopped` | Shut down | Restart |

<br/>

## 📈 Monitoring and Alerts

`docker compose up -d --build` starts tx-gen with Prometheus (`:9090`) and Grafana (`:3000`, `admin`/`admin`), provisioned with the **tx-gen Overview** dashboard. Alert rules live in [`monitoring/prometheus-rules.yml`](../monitoring/prometheus-rules.yml):

| Alert | Fires when |
|-------|------------|
| `TxGenNoAcceptedBroadcasts` | TPS is positive but nothing was accepted for 5m |
| `TxGenNoActiveChains` | No chain is running for 1m |
| `TxGenBootstrapNotDone` | Bootstrap has not reached `l2_done` for 2m |
| `TxGenBroadcastErrors` | Broadcast errors increased in the last 5m |
| `TxGenSSEDisconnected` | An SSE stream has been down for 2m |

**Slack.** With `SLACK_WEBHOOK_URL` set, every mainnet reorg is posted (deduplicated per height for 5 minutes), flagged `:rotating_light:` when deeper than 3 blocks. Failures surface through the Prometheus alerts above and `/status`.

**Arcade transaction events.** With `ARCADE_CALLBACK_TOKEN` set, broadcasts carry `X-CallbackToken` and tx-gen subscribes to `${ARCADE_BASE_URL}/events?callbackToken=…`. Each event is logged as `Arcade tx SSE` (rejections at warn level) with its `txid` and `tx_status`.

<br/>

## 🔀 Reorgs

Reorgs are expected on mainnet and handled automatically: chains keep running, and every reorg is recorded for case studies:

- a structured `SSE reorg` log line (height, depth, orphaned hashes, chains impacted, tips)
- the `txgen_reorg_total{depth="N"}` counter
- `/status` → `engine.lastReorg`
- an optional Slack post

Report deep or unusual reorgs to the team with the height, depth, and tip hashes from the log.

<br/>

## 💾 Backup and Restore

The BoltDB file at `STATE_PATH` holds the active chain tips, pending broadcasts, and the bootstrap stage.

- **Backup** while stopped: `cp state.db state.db.bak`, or snapshot the `/data` volume.
- **Restore**: put the file back at `STATE_PATH` before starting; the engine resumes without a new bootstrap.
- A state file belongs to one lock script. Use a separate `STATE_PATH` for each `INSTANCE_ID`, for each P2PKH key, and when switching between tagged-drop and P2PKH. tx-gen refuses to start on a mismatched file rather than spend outputs it cannot verify.

<br/>

## 🛟 Recovery

Every broadcast is written to the state file before it is sent and committed when Arcade accepts it. A crash mid-broadcast is replayed on the next start, trusting Arcade's acceptance.

| Symptom | Cause | Action |
|---------|-------|--------|
| `bootstrapStage` is `l2_partial` | Some L2 fanouts failed | Keep `state.db`. Check `lastError` and Arcade, then restart: queued L1 outputs are fanned out again |
| `bootstrapStage` is `unknown` | A pending broadcast had no recorded state transition | Keep `state.db`. Reconcile the lock script's outpoints with WhatsOnChain before generating again; the engine stays not-ready rather than risk a double spend |
| `state lock script does not match the configured mode` | `STATE_PATH` belongs to another key or instance | Point `STATE_PATH` at the right file, or start a new one |
| `state has persisted UTXOs without lock-script metadata` | A state file from before lock-script tracking | Reconcile by hand, or start a new `STATE_PATH` |

Never delete `state.db` to "fix" a stuck bootstrap: the chains' tips would be lost, and refetching from WhatsOnChain could double-spend outputs that are still in flight.

<br/>

## 🔧 Troubleshooting

| Symptom | Check |
|---------|-------|
| `config: ADMIN_TOKEN is required` | Export `ADMIN_TOKEN` |
| `config: INSTANCE_ID is required` | Export a unique `INSTANCE_ID` (it also tags the lock script) |
| `config: invalid configuration value` | The named variable is set but unparsable (for example `HTTP_TIMEOUT=30` instead of `30s`) |
| `fetch UTXOs: …` at startup | WhatsOnChain connectivity |
| `waiting for funding UTXO` never ends | The funding output is not locked to the logged script hash; check `INSTANCE_ID` or `PRIVATE_KEY` |
| `arcade rejected the broadcast: HTTP 4xx` | EF encoding or fee policy; check the logged `response` |
| `UTXO value too small` during bootstrap | The funding output cannot pay the fanout fees; fund a larger amount |
| `chains started count=0` | Bootstrap produced no UTXOs; check the fanout broadcast errors above it |
| `SSE disconnected` | Normal; streams reconnect with backoff (1s up to 30s) |
