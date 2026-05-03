# BSV TX Gen Runbook

## Environment Variables

Required:
- ADMIN_TOKEN: bearer token for POST /config and GET /status

Optional with defaults:
- PORT=8080
- STATE_PATH=./state.db
- LOG_LEVEL=info
- LOG_FORMAT=json
- ARCADE_BASE_URL=https://arcade-v2-us-1.bsvblockchain.tech
- WOC_BASE_URL=https://api.whatsonchain.com/v1/bsv/main
- PRIVATE_KEY= (empty = OP_NOP4 mode; WIF or 32-byte hex = P2PKH mode)
- NUM_CHAINS=10000
- FANOUT_SIZE=100
- SUSTAIN_FEE=7 (defaults to 16 when PRIVATE_KEY is set)
- MAX_TPS=10000
- BROADCAST_CONCURRENCY=64
- BROADCAST_RETRY_MAX=3
- HTTP_TIMEOUT=30s
- SSE_RECONNECT_MIN=1s
- SSE_RECONNECT_MAX=30s
- SLACK_WEBHOOK_URL= (empty = no Slack)
- SLACK_CHANNEL=#alerts
- ENVIRONMENT=production
- INSTANCE_ID=local

## Start / Stop

Bare metal:
```
ADMIN_TOKEN=secret ./bsv-tx-gen
```

Docker:
```
docker run -d --name txgen -p 8080:8080 -v $(pwd)/data:/data -e ADMIN_TOKEN=secret -e STATE_PATH=/data/state.db bsv-tx-gen
```

Stop with SIGINT/SIGTERM.

## Endpoints

- GET /healthz : liveness (200 if running)
- GET /readyz : readiness (after bootstrap)
- GET /status : JSON snapshot of engine state, redacted config
- GET /metrics : Prometheus metrics including txgen_reorg_total{depth}
- POST /config : set target TPS (auth Bearer ADMIN_TOKEN, body {"tps":N})
- POST /stop : optional alias for {"tps":0}

## Slack Alerts vs Notable Events

Problem alerts (deduped 5m): failures, recoveries. Sent to SLACK_CHANNEL with :x: :white_check_mark:

Notable reorg events: always logged structured, optional Slack post with :warning: or :rotating_light: for depth>3. Used for mainnet reorg case-study collection. Includes TPS, chains, instance, timestamp.

## Reorg Handling

On reorg: engine records in state, notifier logs + optional Slack, metrics inc txgen_reorg_total, chains continue automatically. Check logs for "reorg" and /status for lastReorg.

Expected: chains resume, queue may have stale but WoC reconcile on next.

## Backup / Restore

Before restart: cp state.db state.db.bak

Periodic: rsync or volume snapshot of /data/state.db

Restore: place state.db before start, engine resumes without full bootstrap if data present.

State files are tied to the configured locking script. Use a separate STATE_PATH when
switching between OP_NOP4 and P2PKH modes, or between different P2PKH keys.

If bootstrapStage is l2_partial or unknown after a crash, do not delete state.db or force
restart from WoC. Keep the DB, inspect /status lastError, and reconcile the configured
locking-script outpoints with WoC; the engine intentionally stays not-ready rather than
risking a stale double-spend.

## Degraded States

- degraded: if TPS under-delivery or bootstrap reconciliation fails
- First actions: check /status, logs for lastError, increase MAX_TPS? check Arcade health.

## Collecting Reorg Evidence

- Structured logs at info level
- Prometheus txgen_reorg_total
- Slack posts if webhook set
- /status lastReorg fields
- state.db meta for bootstrap stage

Report to team with height/depth/tip data for case studies.
