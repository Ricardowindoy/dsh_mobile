import test from 'node:test'
import assert from 'node:assert/strict'
import http from 'node:http'
import os from 'node:os'
import path from 'node:path'
import fs from 'node:fs/promises'

import { Config, apply } from '../index.js'

/** Fake relay implementing the notify management surface + presence. */
async function fakeRelay({ online = true, watching } = {}) {
  const pushed = []
  let answerQueue = []
  let onlineValue = online
  // `watching` defaults to `online` so older tests keep their semantics.
  let watchingValue = watching === undefined ? online : watching
  const server = http.createServer((req, res) => {
    res.setHeader('content-type', 'application/json')
    if (req.headers['x-dsh-relay-token'] !== 'admin-sekrit') { res.statusCode = 401; res.end('{}'); return }
    if (req.url === '/__dsh_tunnel__/notify/online') { res.end(JSON.stringify({ online: onlineValue, watching: watchingValue })); return }
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
function mockCtx({ sessionTitle = '' } = {}) {
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
    get: (name) => (name === 'sessionTitle' && sessionTitle.length > 0
      ? { get: () => ({ title: sessionTitle }) }
      : undefined),
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

async function setup(t, relayOpts = {}, cfgPatch = {}, ctxOpts = {}) {
  const home = await fs.mkdtemp(path.join(os.tmpdir(), 'dsh-ap-'))
  const relay = await fakeRelay(relayOpts)
  const tokenFile = path.join(home, 'token')
  await fs.writeFile(tokenFile, 'admin-sekrit')
  const cfg = Config({
    relayUrl: `http://127.0.0.1:${relay.port}`,
    accessTokenFile: tokenFile,
    askTimeoutMs: 5000,
    questionTimeoutMs: 5000,
    pollWaitSeconds: 1,
    ...cfgPatch,
  })
  const ctx = mockCtx(ctxOpts)
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

// ── v1.0.15: one-tap question cards + completion cards ─────────────────────

const mkQuestionReq = (opts = {}) => ({
  agent: { session: { id: opts.session ?? 's6' } },
  questions: opts.questions ?? [{
    id: 'scope',
    question: '选哪个方案？',
    header: '范围',
    options: [{ label: '方案 A' }, { label: '方案 B' }],
    multiSelect: false,
  }],
  wait: { callId: opts.callId ?? 'call-6' },
})

// Mutation: claim a question the phone cannot answer inline → red (the
// desktop card disappears while the phone has no action to answer it).
test('one-tap question → card carries text/options/title; phone answer resolves it', async (t) => {
  const { ctx, relay } = await setup(t, { online: true }, {}, { sessionTitle: '重构登录页' })
  const pending = ctx.waterfall('user-questions/request', mkQuestionReq())
  await sleep(150)
  const card = relay.pushed.find((x) => x.message.kind === 'question')
  assert.ok(card, 'question card pushed')
  assert.equal(card.message.question, '选哪个方案？')
  assert.deepEqual(card.message.options, ['方案 A', '方案 B'])
  assert.equal(card.message.title, '重构登录页')
  assert.equal(card.message.questionId, 'scope')
  relay.enqueue({ id: card.message.id, answers: [{ id: 'scope', selected: ['方案 B'] }], deviceId: 'ph_1' })
  const out = await pending
  assert.deepEqual(out, { answers: [{ id: 'scope', selected: ['方案 B'] }] })
})

// Mutation: claim multi-question asks → red (no notification can express
// answers to several questions in one tap).
test('multi-question ask is not claimed but still nudges the phone', async (t) => {
  const { ctx, relay } = await setup(t, { online: true })
  const out = await ctx.waterfall('user-questions/request', mkQuestionReq({
    questions: [
      { id: 'a', question: '第一个问题', options: [{ label: 'A' }] },
      { id: 'b', question: '第二个问题', options: [{ label: 'B' }] },
    ],
    session: 's6b',
    callId: 'call-6b',
  }))
  assert.equal(out, 'unavailable', 'not claimed: desktop keeps the card')
  await sleep(150) // the nudge is fire-and-forget; let the push land
  const card = relay.pushed.find((x) => x.message.kind === 'question')
  assert.ok(card)
  assert.equal(card.message.options.length, 0, 'non-inline cards must not offer actions')
  assert.equal(card.message.question, '第一个问题')
})

// Mutation: >3 options claimed → red (Android shows at most three actions).
test('question with more than three options is not claimed', async (t) => {
  const { ctx, relay } = await setup(t, { online: true })
  const out = await ctx.waterfall('user-questions/request', mkQuestionReq({
    questions: [{ id: 'q', question: '选一个', options: ['A', 'B', 'C', 'D'].map((label) => ({ label })) }],
    session: 's6c',
    callId: 'call-6c',
  }))
  assert.equal(out, 'unavailable')
  await sleep(150)
  const card = relay.pushed.find((x) => x.message.kind === 'question')
  assert.equal(card.message.options.length, 0)
})

// Mutation: timeout resolves an empty answer → red (empty selected reads as
// "the user skipped", which would fabricate a human decision).
test('claimed question with no answer fails as ASK_TIMED_OUT, never as skipped', async (t) => {
  const { ctx } = await setup(t, { online: true }, { questionTimeoutMs: 5000 })
  await assert.rejects(
    () => ctx.waterfall('user-questions/request', mkQuestionReq({ session: 's7', callId: 'call-7' })),
    (e) => e && e.name === 'UserQuestionError' && e.code === 'ASK_TIMED_OUT',
  )
})

// Mutation: drop the subagent filter → red (every subagent finish would buzz).
// Mutation: per-turn done id → red (each reply stacks a new notification
// instead of updating one slot; this is the "repeatedly pushed" regression).
// Mutation: raw markdown preview → red (lock screen showed ** and ##).
test('turn completion pushes one per-session done card, markdown stripped', async (t) => {
  const { ctx, relay } = await setup(t, { online: true }, {}, { sessionTitle: '重构登录页' })
  const markdown = '## 结论\n\n**改完了**，测试全绿 | 项 | 值 |\n|---|---|\n| a | 1 |'
  ctx.emit('session/event', { id: 's8' }, {
    type: 'assistant/message', seq: 1,
    data: { turn: 3, message: { content: [{ type: 'text', text: markdown }] } },
  })
  ctx.emit('session/event', { id: 's8' }, { type: 'turn/end', seq: 2, data: { turn: 3, reason: { kind: 'completed' } } })
  await sleep(150)
  const card = relay.pushed.find((x) => x.message.kind === 'done')
  assert.ok(card, 'done card pushed')
  assert.equal(card.message.title, '重构登录页')
  assert.equal(card.message.id, 'done:s8', 'stable per-session id so Android replaces the slot')
  assert.ok(card.message.preview.includes('改完了'), card.message.preview)
  assert.ok(!/[*#|]/.test(card.message.preview), `markdown leaked: ${card.message.preview}`)
})

// Same session, later turn: the card id must stay identical (slot replacement),
// while a different session gets its own slot.
test('done card id is per-session, not per-turn', async (t) => {
  const { ctx, relay } = await setup(t, { online: true }, {}, { sessionTitle: 'S' })
  for (const [turn, seq] of [[1, 1], [2, 3]]) {
    ctx.emit('session/event', { id: 's10' }, {
      type: 'assistant/message', seq,
      data: { turn, message: { content: [{ type: 'text', text: `第 ${turn} 轮` }] } },
    })
    ctx.emit('session/event', { id: 's10' }, { type: 'turn/end', seq: seq + 1, data: { turn, reason: { kind: 'completed' } } })
  }
  ctx.emit('session/event', { id: 's11' }, { type: 'turn/end', seq: 1, data: { turn: 1, reason: { kind: 'completed' } } })
  await sleep(200)
  const ids = relay.pushed.filter((x) => x.message.kind === 'done').map((x) => x.message.id)
  // The second turn of the SAME session lands inside the 60s cooldown, so it is
  // suppressed; a different session gets its own (still per-session) slot.
  assert.deepEqual(ids, ['done:s10', 'done:s11'])
})

// Mutation: only notify completed/max-tokens -> red (an interrupted turn is
// exactly what the user wants to hear about when away from the desk).
test('interrupted turn pushes a done card with status=interrupted', async (t) => {
  const { ctx, relay } = await setup(t, { online: true }, {}, { sessionTitle: '长任务' })
  ctx.emit('session/event', { id: 's12' }, { type: 'turn/end', seq: 1, data: { turn: 4, reason: { kind: 'aborted', reason: { kind: 'user' } } } })
  await sleep(150)
  const card = relay.pushed.find((x) => x.message.kind === 'done')
  assert.ok(card, 'interrupted card pushed')
  assert.equal(card.message.status, 'interrupted')
  assert.equal(card.message.title, '长任务')
  assert.ok(card.message.preview.includes('中断'), card.message.preview)
})

test('subagent completion does not push a done card', async (t) => {
  const { ctx, relay } = await setup(t, { online: true })
  ctx.emit('session/event', { id: 'sub', origin: 'subagent', parentSession: 's9' },
    { type: 'turn/end', seq: 1, data: { turn: 1, reason: { kind: 'completed' } } })
  await sleep(150)
  assert.equal(relay.pushed.filter((x) => x.message.kind === 'done').length, 0)
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
// ── review fixes: strict presence gate, key collisions, cancellation ─────────

// Presence grace alone must not claim: a card pushed into a dead channel is
// never seen, and the claimed approval then fails closed as a rejection.
// Mutation: use `online` instead of `watching` → red.
test('online but not watching → approval is not claimed', async (t) => {
  const { ctx, relay } = await setup(t, { online: true, watching: false })
  const out = await ctx.waterfall('approval/request', mkReq('s20', 'bash', 'c20'))
  assert.equal(out, 'unavailable')
  assert.equal(relay.pushed.length, 0, 'must not push into a dead channel')
})

// Two approvals of the same tool in one session, both without a callId, must
// not share a pending slot (the second used to overwrite the first).
// Mutation: key without a disambiguator → red (first never resolves).
test('approvals without callId get distinct pending keys', async (t) => {
  const { ctx, relay } = await setup(t, { online: true })
  const a = { agent: { session: { id: 's21' } }, toolName: 'bash', reason: 'one' }
  const b = { agent: { session: { id: 's21' } }, toolName: 'bash', reason: 'two' }
  const pa = ctx.waterfall('approval/request', a)
  const pb = ctx.waterfall('approval/request', b)
  await sleep(150)
  const cards = relay.pushed.filter((x) => x.message.kind === 'approval').map((x) => x.message.id)
  assert.equal(cards.length, 2, `expected two cards, got ${JSON.stringify(cards)}`)
  assert.notEqual(cards[0], cards[1], 'pending keys collided')
  relay.enqueue({ id: cards[0], decision: 'allowed-once', deviceId: 'ph_1' })
  relay.enqueue({ id: cards[1], decision: 'rejected', deviceId: 'ph_1' })
  assert.equal(await pa, 'allowed-once')
  assert.equal(await pb, 'rejected')
})

// A cancelled ask must surface as ASK_CANCELLED, not ASK_TIMED_OUT.
// Mutation: map both to ASK_TIMED_OUT → red.
test('cancelled question reports ASK_CANCELLED', async (t) => {
  const { ctx } = await setup(t, { online: true }, { questionTimeoutMs: 30000 })
  const ac = new AbortController()
  const req = { ...mkQuestionReq({ session: 's22', callId: 'call-22' }), signal: ac.signal }
  const p = ctx.waterfall('user-questions/request', req)
  await sleep(120)
  ac.abort()
  await assert.rejects(p, (e) => e && e.name === 'UserQuestionError' && e.code === 'ASK_CANCELLED')
})
