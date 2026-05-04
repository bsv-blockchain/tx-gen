#!/usr/bin/env node
import { readFileSync, existsSync } from 'node:fs'
import { resolve } from 'node:path'
import { createInterface } from 'node:readline/promises'
import type { Interface as ReadlineInterface } from 'node:readline/promises'
import { stdin as input, stdout as output, exit } from 'node:process'
import { WalletClient, Script, Transaction, Utils } from '@bsv/sdk'

const DEFAULT_ARCADE_BASE_URL = 'https://arcade-v2-us-1.bsvblockchain.tech'
const DEFAULT_WALLET_ORIGINATOR = 'test'
const DEFAULT_FANOUT_SIZE = 100
const DEFAULT_SUSTAIN_FEE = 7
const DEFAULT_UTXO_IDLE_SECONDS = 600
const LOCK_SCRIPT_TAG_BYTES = 6
const SATOSHIS_PER_KB = 100

type FundingPlan = {
  tps: number
  durationSeconds: number
  totalSustainTxs: bigint
  txsPerChain: bigint
  leafValue: bigint
  initialSatoshis: bigint
  l1FanoutFee: bigint
  l1FanoutSize: number
  maxL1OutputValue: bigint
  maxFanoutSize: number
  numChains: number
  utxoIdleSeconds: number
  sustainFee: bigint
}

loadDotEnv()

async function main() {
  const instanceID = requiredEnv('INSTANCE_ID')
  const tagBytes = normalizeInstanceID(instanceID)
  const tagHex = Utils.toHex(Array.from(tagBytes))
  const tagDisplay = Buffer.from(tagBytes).toString('utf8')
  const arcadeBaseURL = trimTrailingSlash(process.env.ARCADE_BASE_URL || DEFAULT_ARCADE_BASE_URL)
  const walletOriginator = process.env.WALLET_ORIGINATOR || DEFAULT_WALLET_ORIGINATOR
  const maxFanoutSize = envInt('FANOUT_SIZE', DEFAULT_FANOUT_SIZE)
  const utxoIdleSeconds = envInt('UTXO_IDLE_SECONDS', DEFAULT_UTXO_IDLE_SECONDS)
  const sustainFee = BigInt(envInt('SUSTAIN_FEE', DEFAULT_SUSTAIN_FEE))

  const rl = createInterface({ input, output })
  try {
    const tps = await askPositiveInt(rl, 'Target TPS? ')
    const durationSeconds = await askDurationSeconds(rl, 'How long should tx generation run? (examples: 10m, 1h, 90s) ')
    const plan = buildFundingPlan({ tps, durationSeconds, maxFanoutSize, utxoIdleSeconds, sustainFee })

    printPlan(plan, {
      instanceID,
      tagDisplay,
      tagHex,
      arcadeBaseURL,
      walletOriginator
    })

    const confirmed = await askYesNo(rl, 'Create the wallet action and broadcast the EF to Arcade? [y/N] ')
    if (!confirmed) {
      console.log('Aborted before requesting wallet funding.')
      return
    }

    const satoshis = toSafeSatoshiNumber(plan.initialSatoshis)
    const lockingScript = Script.fromASM(`${tagHex} OP_DROP`).toHex()

    const wallet = new WalletClient('auto', walletOriginator)
    await wallet.connectToSubstrate()

    const action = await wallet.createAction({
      description: `Initial tx-gen funding for ${tagDisplay.trimEnd() || tagHex}`,
      version: 2,
      outputs: [{
        satoshis,
        lockingScript,
        basket: process.env.FUNDING_BASKET || `push ${tagDisplay.trimEnd() || tagHex}`,
        outputDescription: 'tx-gen initial funding output'
      }]
    })

    if (!action.tx) {
      throw new Error('Wallet did not return Atomic BEEF. The funding action was not broadcast by this script.')
    }

    const ef = Transaction.fromBEEF(action.tx).toEF()
    const response = await postEF(arcadeBaseURL, ef)

    console.log('')
    console.log(`Funding txid: ${action.txid ?? '(wallet did not return txid)'}`)
    console.log(`Arcade status: ${response.status}`)
    if (response.body) {
      console.log(`Arcade response: ${response.body}`)
    }
  } finally {
    rl.close()
  }
}

function loadDotEnv() {
  const envPath = resolve(process.cwd(), '.env')
  if (!existsSync(envPath)) {
    return
  }
  const lines = readFileSync(envPath, 'utf8').split(/\r?\n/)
  for (const line of lines) {
    const trimmed = line.trim()
    if (!trimmed || trimmed.startsWith('#')) {
      continue
    }
    const assignment = trimmed.startsWith('export ') ? trimmed.slice('export '.length).trim() : trimmed
    const eq = assignment.indexOf('=')
    if (eq <= 0) {
      continue
    }
    const key = assignment.slice(0, eq).trim()
    const rawValue = assignment.slice(eq + 1).trim()
    if (process.env[key] !== undefined) {
      continue
    }
    process.env[key] = unquoteEnvValue(rawValue)
  }
}

function unquoteEnvValue(value: string): string {
  if ((value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"))) {
    return value.slice(1, -1)
  }
  return value
}

function requiredEnv(key: string): string {
  const value = process.env[key]?.trim()
  if (!value) {
    throw new Error(`${key} is required`)
  }
  return value
}

function envInt(key: string, fallback: number): number {
  const raw = process.env[key]?.trim()
  if (!raw) {
    return fallback
  }
  const parsed = Number.parseInt(raw, 10)
  if (!Number.isSafeInteger(parsed) || parsed <= 0) {
    throw new Error(`${key} must be a positive integer`)
  }
  return parsed
}

function normalizeInstanceID(value: string): Uint8Array {
  const tag = new Uint8Array(LOCK_SCRIPT_TAG_BYTES)
  tag.fill(0x20)

  let offset = 0
  for (const char of value.trim()) {
    const encoded = new TextEncoder().encode(char)
    if (offset + encoded.length > tag.length) {
      break
    }
    tag.set(encoded, offset)
    offset += encoded.length
    if (offset === tag.length) {
      break
    }
  }
  return tag
}

async function askPositiveInt(rl: ReadlineInterface, prompt: string): Promise<number> {
  for (;;) {
    const answer = (await rl.question(prompt)).trim()
    const value = Number(answer)
    if (Number.isSafeInteger(value) && value > 0) {
      return value
    }
    console.log('Enter a positive integer.')
  }
}

async function askDurationSeconds(rl: ReadlineInterface, prompt: string): Promise<number> {
  for (;;) {
    const answer = (await rl.question(prompt)).trim()
    const seconds = parseDurationSeconds(answer)
    if (seconds > 0) {
      return seconds
    }
    console.log('Enter a positive duration like 90s, 10m, 1.5h, or 2d.')
  }
}

async function askYesNo(rl: ReadlineInterface, prompt: string): Promise<boolean> {
  const answer = (await rl.question(prompt)).trim().toLowerCase()
  return answer === 'y' || answer === 'yes'
}

function parseDurationSeconds(value: string): number {
  const match = value.match(/^(\d+(?:\.\d+)?)\s*([smhd]?)$/i)
  if (!match) {
    return 0
  }
  const amount = Number(match[1])
  const unit = match[2].toLowerCase()
  const multiplier = unit === 'd' ? 86_400 : unit === 'h' ? 3_600 : unit === 'm' ? 60 : 1
  return Math.ceil(amount * multiplier)
}

function buildFundingPlan(input: {
  tps: number
  durationSeconds: number
  maxFanoutSize: number
  utxoIdleSeconds: number
  sustainFee: bigint
}): FundingPlan {
  const numChains = input.tps * input.utxoIdleSeconds
  if (!Number.isSafeInteger(numChains) || numChains <= 0) {
    throw new Error(`computed NUM_CHAINS is outside the safe integer range: ${numChains}`)
  }

  const totalSustainTxs = BigInt(input.tps) * BigInt(input.durationSeconds)
  const chains = BigInt(numChains)
  const txsPerChain = divCeil(totalSustainTxs, chains)
  const leafValue = txsPerChain * input.sustainFee
  const l1FanoutSize = Math.ceil(numChains / input.maxFanoutSize)
  const l1FanoutFee = estimateTaggedDropFanoutFee(l1FanoutSize)
  const maxL1OutputValue = maxRequiredL1OutputValue(numChains, l1FanoutSize, leafValue)

  const initialSatoshis = BigInt(l1FanoutSize) * maxL1OutputValue + l1FanoutFee

  return {
    tps: input.tps,
    durationSeconds: input.durationSeconds,
    totalSustainTxs,
    txsPerChain,
    leafValue,
    initialSatoshis,
    l1FanoutFee,
    l1FanoutSize,
    maxL1OutputValue,
    maxFanoutSize: input.maxFanoutSize,
    numChains,
    utxoIdleSeconds: input.utxoIdleSeconds,
    sustainFee: input.sustainFee
  }
}

function maxRequiredL1OutputValue(numChains: number, l1FanoutSize: number, leafValue: bigint): bigint {
  let max = 0n
  for (let i = 0; i < l1FanoutSize; i++) {
    const l2FanoutSize = l2FanoutSizeForParent(numChains, l1FanoutSize, i)
    const required = BigInt(l2FanoutSize) * leafValue + estimateTaggedDropFanoutFee(l2FanoutSize)
    if (required > max) {
      max = required
    }
  }
  return max
}

function l2FanoutSizeForParent(numChains: number, l1FanoutSize: number, parentIndex: number): number {
  const base = Math.floor(numChains / l1FanoutSize)
  const remainder = numChains % l1FanoutSize
  return base + (parentIndex < remainder ? 1 : 0)
}

function estimateTaggedDropFanoutFee(fanoutSize: number): bigint {
  const inputSize = 32 + 4 + varIntSize(1) + 1 + 4
  const lockScriptLen = 1 + LOCK_SCRIPT_TAG_BYTES + 1
  const outputSize = 8 + varIntSize(lockScriptLen) + lockScriptLen
  const rawSize = 4 + varIntSize(1) + inputSize + varIntSize(fanoutSize) + fanoutSize * outputSize + 4
  return divCeil(BigInt(rawSize * SATOSHIS_PER_KB), 1000n)
}

function varIntSize(n: number): number {
  if (n < 0xfd) {
    return 1
  }
  if (n <= 0xffff) {
    return 3
  }
  if (n <= 0xffffffff) {
    return 5
  }
  return 9
}

function divCeil(numerator: bigint, denominator: bigint): bigint {
  return (numerator + denominator - 1n) / denominator
}

function printPlan(plan: FundingPlan, context: {
  instanceID: string
  tagDisplay: string
  tagHex: string
  arcadeBaseURL: string
  walletOriginator: string
}) {
  console.log('')
  console.log('Initial funding plan')
  console.log(`  INSTANCE_ID: ${context.instanceID}`)
  console.log(`  lock tag: ${JSON.stringify(context.tagDisplay)} (${context.tagHex})`)
  console.log(`  target: ${plan.tps} TPS for ${formatDuration(plan.durationSeconds)}`)
  console.log(`  UTXO rest target: ${formatDuration(plan.utxoIdleSeconds)}`)
  console.log(`  sustain txs: ${plan.totalSustainTxs.toString()} total, ${plan.txsPerChain.toString()} per chain`)
  console.log(`  bootstrap: 1 -> ${plan.l1FanoutSize} -> ${plan.numChains} UTXOs`)
  console.log(`  sustain fee: ${plan.sustainFee.toString()} sat/tx`)
  console.log(`  L1 fanout fee estimate: ${plan.l1FanoutFee.toString()} sat`)
  console.log(`  L1 output value: ${plan.maxL1OutputValue.toString()} sat`)
  console.log(`  initial output: ${plan.initialSatoshis.toString()} sat`)
  console.log(`  service env: NUM_CHAINS=${plan.numChains} FANOUT_SIZE=${plan.maxFanoutSize}`)
  console.log(`  wallet originator: ${context.walletOriginator}`)
  console.log(`  arcade: ${context.arcadeBaseURL}/tx`)
  console.log('')
}

function formatDuration(seconds: number): string {
  if (seconds % 86_400 === 0) {
    return `${seconds / 86_400}d`
  }
  if (seconds % 3_600 === 0) {
    return `${seconds / 3_600}h`
  }
  if (seconds % 60 === 0) {
    return `${seconds / 60}m`
  }
  return `${seconds}s`
}

function toSafeSatoshiNumber(value: bigint): number {
  if (value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new Error(`initial output ${value.toString()} exceeds JavaScript safe integer range`)
  }
  return Number(value)
}

async function postEF(baseURL: string, ef: number[]) {
  const headers: Record<string, string> = {
    'Content-Type': 'application/octet-stream'
  }
  if (process.env.ARCADE_CALLBACK_TOKEN?.trim()) {
    headers['X-CallbackToken'] = process.env.ARCADE_CALLBACK_TOKEN.trim()
  }

  const response = await fetch(`${baseURL}/tx`, {
    method: 'POST',
    headers,
    body: Uint8Array.from(ef)
  })
  const body = await response.text()
  if (response.status !== 202) {
    throw new Error(`Arcade rejected EF with HTTP ${response.status}: ${body}`)
  }
  return { status: response.status, body }
}

function trimTrailingSlash(value: string): string {
  return value.replace(/\/+$/, '')
}

main().catch((err: unknown) => {
  console.error(err instanceof Error ? err.message : err)
  exit(1)
})
