import test from 'node:test'
import assert from 'node:assert/strict'
import http from 'node:http'
import os from 'node:os'
import path from 'node:path'
import fs from 'node:fs/promises'

import { Config, apply } from '../index.js'

/** Fake relay implementing the notify management surface + presence. */
async function fakeRelay({ online = true } = {}) {
  const pushed = []
  let answerQueue = []
  let onlineValue = online
  const server = http.createServer((req, res) => {
    res.setHeader('content-type', 'application/json')
    if (req.headers['x-dsh-relay-token'] !== 'admin-sekrit') { res.statusCode = 401; res.end('{}'); return }
    if (req.url === '/__dsh_tunnel__/notify/online') { res.end(JSON.stringify({ online: onlineValue })); return }
    if (req.url === '/__dsh_tunnel__/pair/devices') { res.end(JSON.stringify({ devices: [{ deviceId: 'ph_1' }] })); return }
    if (req.url === '/__dsh_tunnel__/notify/push') {
      let body = ''
      req.on('data', (c) => { body += c })
      req.on('end', () => { pushed.push(JSON.parse(body)); res.end(JSON.stringify({ delivered: 1 })) })
      return
    }
    if (req.url.startsWith('/__dsh_tunnel__/notify/answers')) {
      const out = answerQueue
      answerQueue = []
      res.end(JSON.stringify({ answers: out }))
      return
    }
    res.statusCode = 404; res.end('{}')
  })
  await new Promise((r) => server.listen(0, '127.0.0.1', r))
  return {
    server, port: server.address().port, pushed,
    enqueue: (a) => { answerQueue.push(a) },
    setOnline: (v) => { onlineValue = v },
  }
}

/** Mock cordis ctx capturing event listeners and waterfall answerers. */
function mockCtx() {
  const listeners = new Map()
  const disposers = []
  const ctx = {
    on: (event, fn) => {
      if (!listeners.has(event)) listeners.set(event, [])
      listeners.get(event).push(fn)
      return () => {}
    },
    effect: (fn) => { disposers.push(fn()) },
    emit: (event, ...args) => (listeners.get(event) || []).map((fn) => fn(...args)),
    waterfall: async (event, req) => {
      // one answerer chain: first non-next() return wins; next() = undefined sentinel
      const NEXT = Symbol('next')
      for (const fn of listeners.get(event) || []) {
        const out = await fn(req, () => NEXT)
        if (out !== NEXT) return out
      }
      return 'unavailable'
    },
    dispose: () => disposers.forEach((d) => d?.()),
  }
  return ctx
}

async function setup(t, relayOpts = {}, cfgPatch = {}) {
  const home = await fs.mkdtemp(path.join(os.tmpdir(), 'dsh-ap-'))
  const relay = await fakeRelay(relayOpts)
  const tokenFile = path.join(home, 'token')
  await fs.writeFile(tokenFile, 'admin-sekrit')
  const cfg = Config({
    relayUrl: `http://127.0.0.1:${relay.port}`,
    accessTokenFile: tokenFile,
    askTimeoutMs: 5000,
    pollWaitSeconds: 1,
    ...cfgPatch,
  })
  const ctx = mockCtx()
  apply(ctx, cfg)
  t.after(async (t) => {
    ctx.dispose()
    await new Promise((r) => relay.server.close(r))
    await fs.rm(home, { recursive: true, force: true })
  })
  return { ctx, relay }
}

const mkReq = (session = 's1', tool = 'bash', callId = 'c1') => ({
  agent: { session: { id: session } },
  toolName: tool,
  callId,
  reason: 'needs permission',
})

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// Mutation: claim without checking presence → this test goes red (the ask
// would hang on a phone that is not there instead of staying on the desktop).
test('no subscriber → answerer passes through (GUI keeps the ask)', async (t) => {
  const { ctx, relay } = await setup(t, { online: false })
  const out = await ctx.waterfall('approval/request', mkReq())
  assert.equal(out, 'unavailable') // our answerer called next(); no other answerer → fail-closed default
  assert.equal(relay.pushed.length, 0, 'must not push a card with no subscriber')
})

// Mutation: push but never resolve on answer → red (ask hangs to timeout).
test('subscriber online → card pushed; phone answer resolves the ask', async (t) => {
  const { ctx, relay } = await setup(t, { online: true })
  const req = mkReq()
  const outcomeP = ctx.waterfall('approval/request', req)
  await sleep(150) // let the push land
  assert.equal(relay.pushed.length, 1)
  const card = relay.pushed[0].message
  assert.equal(card.kind, 'approval')
  assert.equal(card.toolName, 'bash')
  assert.equal(card.id, 's1/c1')
  // Phone answers via the relay queue.
  relay.enqueue({ id: card.id, decision: 'allowed-once', deviceId: 'ph_1' })
  const out = await outcomeP
  assert.equal(out, 'allowed-once')
})

// Mutation: timeout resolves allowed → red. Silence must never consent.
test('claimed ask times out fail-closed (unavailable), never allowed', async (t) => {
  const { ctx, relay } = await setup(t, { online: true }, { askTimeoutMs: 5000 })
  // Use the minimum allowed timeout (5s) — the test waits it out.
  const out = await ctx.waterfall('approval/request', mkReq('s2', 'bash', 'c2'))
  assert.equal(out, 'unavailable')
  assert.ok(relay.pushed.length >= 1)
})

// Mutation: ignore approval/decided → red (double answer / stale card).
test('GUI decides first → durable outcome adopted, retract pushed', async (t) => {
  const { ctx, relay } = await setup(t, { online: true })
  const req = mkReq('s3', 'bash', 'c3')
  const outcomeP = ctx.waterfall('approval/request', req)
  await sleep(150)
  const card = relay.pushed[0].message
  // The audit pair: asked (correlates) then decided (GUI answered).
  ctx.emit('session/event', { id: 's3' }, { type: 'approval/asked', seq: 1, data: { id: 'audit-1', toolName: 'bash', callId: 'c3' } })
  ctx.emit('session/event', { id: 's3' }, { type: 'approval/decided', seq: 2, data: { id: 'audit-1', outcome: 'rejected' } })
  const out = await outcomeP
  assert.equal(out, 'rejected')
  await sleep(100)
  const retract = relay.pushed.find((p) => p.message.kind === 'retract')
  assert.ok(retract, 'retract card pushed after GUI decided')
  assert.equal(retract.message.id, card.id)
})

// Mutation: include question text → red (privacy rule).
test('question card carries no question text', async (t) => {
  const { ctx, relay } = await setup(t, { online: true })
  ctx.emit('session/event', { id: 's4' }, {
    type: 'tool/call', seq: 1,
    data: { name: 'ask_user_question', callId: 'q1', arguments: { question: 'SECRET TEXT' } },
  })
  await sleep(150)
  const q = relay.pushed.find((p) => p.message.kind === 'question')
  assert.ok(q, 'question card pushed')
  assert.ok(!JSON.stringify(q).includes('SECRET TEXT'), 'question text leaked')
})

// Mutation: claimApprovals=false still claims → red.
test('claimApprovals=false → push-only, answerer absent', async (t) => {
  const { ctx } = await setup(t, { online: true }, { claimApprovals: false })
  const out = await ctx.waterfall('approval/request', mkReq())
  assert.equal(out, 'unavailable') // no answerer registered at all
})

// Rogue decision values never resolve an ask.
test('rogue decision in answer queue is ignored', async (t) => {
  const { ctx, relay } = await setup(t, { online: true }, { askTimeoutMs: 5000 })
  const outcomeP = ctx.waterfall('approval/request', mkReq('s5', 'bash', 'c5'))
  await sleep(150)
  relay.enqueue({ id: 's5/c5', decision: 'always-allow', deviceId: 'ph_1' })
  const out = await outcomeP
  assert.equal(out, 'unavailable') // rogue ignored → timeout fail-closed
})