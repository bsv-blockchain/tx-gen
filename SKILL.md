---
name: bsv-tx-gen
description: Deploy and operate the BSV transaction generator service. Use when asked to start, stop, configure, or troubleshoot the bsv-tx-gen stress-testing tool.
---

## What this service does

Generates BSV mainnet transactions at a configurable TPS. Bootstraps 10,000 UTXO chains from a single funded output, then drives them concurrently. TPS is controlled at runtime via HTTP.

## Prerequisites

- Go 1.25+ installed (`go version`)
- A BSV mainnet UTXO locked to `OP_NOP4` (0xB9), or a P2PKH UTXO matching `PRIVATE_KEY`
- `ADMIN_TOKEN` secret (any string; used for Bearer auth on `/config`)
- Outbound HTTPS to `api.whatsonchain.com` and `arcade-v2-us-1.bsvblockchain.tech`

## Deploy steps

```sh
cd /path/to/bsv-tx-gen

# 1. Set env vars
export ADMIN_TOKEN=<secret>
export PORT=8080          # optional, default 8080
# export PRIVATE_KEY=<wif-or-hex>  # optional; enables P2PKH mode

# 2. Build
go build -o bsv-tx-gen .

# 3. Run (blocks; use systemd/screen/tmux for persistence)
./bsv-tx-gen
```

Startup log confirms scripthash, UTXO count, and bootstrap path:
```
scripthash: <hash>
loaded N UTXOs
bootstrap: L1 + L2 fanout (1 → 100 → 10,000)   # or "resuming with N UTXOs"
bootstrap complete: 10000 UTXOs ready
started 10000 chains
```

## Start transactions

```sh
curl -s -X POST http://localhost:$PORT/config \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"tps": 16}'
```

Response: `{"tps":16}`

## Pause

```sh
curl -s -X POST http://localhost:$PORT/config \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"tps": 0}'
```

All 10,000 chain goroutines block immediately. No state is lost; resume by setting `tps > 0`.

## TPS range

- Minimum: `0` (paused)
- Maximum: `10000`
- Recommended for sustained load: `16` (≈ one tx per chain per 10 min)
- Higher values reduce the interval between hops, exhausting chains faster

## Bootstrap detection (automatic)

| Queue length at startup | Action |
|---|---|
| 1 | Full bootstrap: L1 fanout (1→100) then L2 (100→10,000) |
| 100 | L2 fanout only (100→10,000) |
| Other | Resume existing chains directly |

## Troubleshooting

| Symptom | Check |
|---|---|
| `ADMIN_TOKEN env var required` | Export `ADMIN_TOKEN` before running |
| `fetch UTXOs: ...` error | WhatOnChain connectivity; verify UTXO exists for the active mode's locking script |
| `arcade 4xx: ...` | EF format or connectivity issue with Arcade v2 |
| `bootstrap error: value N too small` | Funded UTXO too small; need `> 106` sats for L1 fanout |
| `started 0 chains` | Bootstrap produced no UTXOs; check arcade broadcast errors above |
| SSE disconnects logged | Normal; service reconnects automatically every 5s |

## Key constants (source: engine.go, tx.go)

- `numChains = 10_000`
- `fanoutSize = 100`
- OP_NOP4 sustain fee: `7` sats per hop
- P2PKH sustain fee: `16` sats per hop by default
- Default lock script: `0xB9` (OP_NOP10 in go-sdk, called OP_NOP4 in this codebase)
- Default unlock script: `0x51` (OP_TRUE)
- Arcade base URL: `https://arcade-v2-us-1.bsvblockchain.tech`
