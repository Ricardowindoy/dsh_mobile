/**
 * dsh-approval-push — host plugin: approval/question cards to the phone.
 *
 * Semantics borrowed from the upstream dsh-remote-bridge (see
 * docs/reference/dsh-remote-bridge-README.md), adapted
 * to our byte-proxy architecture:
 *
 *  - CLAIM: we register an `approval/request` waterfall answerer, but claim
 *    an ask ONLY while at least one phone is subscribed (relay notify/online).
 *    No subscriber → next(): the GUI answerer keeps its behaviour exactly.
 *  - FAIL-CLOSED: a claimed ask nobody answers resolves after askTimeoutMs
 *    (default 110s, inside the harness's 2-minute window) by returning
 *    undefined → the approval service normalizes to 'unavailable' = refusal.
 *    Silence is never consent. A cancelled signal resolves immediately.
 *  - FIRST ANSWER WINS: the GUI answering first appends approval/decided;
 *    we see it via session/event, resolve our pending promise as 'settled
 *    elsewhere' (return next() is impossible mid-wait, so we resolve with
 *    undefined — the service treats a non-vocabulary answer as unavailable,
 *    BUT the GUI already answered upstream in the waterfall... see below).
 *
 * Waterfall ordering note: answerers run in registration order; ours claims
 * by returning a promise instead of next(). If the GUI answerer sits BEFORE
 * us, it answers first and we never see the ask (fine). If ours runs first
 * and the GUI answers while we wait, the durable approval/decided event tells
 * us; we then return undefined and the service normalizes 'unavailable' —
 * but the tool call already got the GUI's outcome? No: the waterfall is ONE
 * chain; our returned value IS the outcome. Therefore on GUI-decided we must
 * return the GUI's actual outcome from the decided event, not undefined.
 * The decided event carries {id, outcome}; we resolve with that outcome.
 *
 *  - PRIVACY: question cards carry no question text (same rule as the
 *    approval-popup-bridge): "有新提问，去 dsh 查看".
 *  - TRANSPORT: plugin → relay over HTTPS with the management token (same
 *    postJson pattern as remote-access-plugin, private-CA aware); phone →
 *    relay SSE with its device credential; answers come back via the
 *    plugin's long poll of notify/answers.
 */

import { readFileSync } from 'node:fs'
import { request as httpsRequest } from 'node:https'
import { request as httpRequest } from 'node:http'
import Schema from '@deepseek-ai/schemastery'

export const Config = Schema.object({
  // Relay base URL (no trailing slash), e.g. https://relay.example.com:24000
  relayUrl: Schema.string().required(),
  // Management credential file (same token the remote-access plugin uses).
  accessTokenFile: Schema.string().required(),
  // Private CA for the IP entrance; empty = system trust (domain entrance).
  caFile: Schema.string().default(''),
  // Which paired phone receives cards. Empty = broadcast to every paired
  // device (relay pair/devices list, refreshed every 60s).
  deviceId: Schema.string().default(''),
  // Claim gate: only claim asks while a phone subscribed within the relay's
  // presence window. Default true; false = never claim (push-only mode).
  claimApprovals: Schema.boolean().default(true),
  // A claimed ask nobody answers resolves unavailable after this long.
  askTimeoutMs: Schema.number().default(110_000).min(5_000).max(300_000),
  // Long-poll cadence guard.
  pollWaitSeconds: Schema.number().default(25).min(1).max(25),
})

// No service injection: this plugin only observes `approval/request` and
// `session/event`, which are global events. There is no `sessionEvents`
// service in DSH (the session store is `sessions`), so injecting it here
// leaves the whole plugin pending and silently disables every push.
export const inject = []

const ONLINE_CACHE_MS = 3_000
const DEVICES_CACHE_MS = 60_000

/** POST JSON, direct socket (no env proxy / global dispatcher) — the exact
 *  transport that measured working in remote-access-plugin (2026-11-08). */
function postJson(url, timeoutMs, caFile, headers, payload) {
  return new Promise((resolve, reject) => {
    let settled = false
    const finish = (fn, v) => { if (!settled) { settled = true; fn(v) } }
    const u = new URL(url)
    const lib = u.protocol === 'https:' ? httpsRequest : httpRequest
    const body = JSON.stringify(payload)
    const options = {
      method: 'POST', hostname: u.hostname,
      port: u.port || (u.protocol === 'https:' ? 443 : 80),
      path: u.pathname + u.search, agent: false, timeout: timeoutMs,
      headers: { ...(headers || {}), 'content-type': 'application/json', 'content-length': Buffer.byteLength(body) },
    }
    if (u.protocol === 'https:') {
      options.rejectUnauthorized = true
      if (caFile) options.ca = readFileSync(caFile)
    }
    const req = lib(options, (res) => {
      let out = ''
      res.setEncoding('utf8')
      res.on('data', (c) => { out += c })
      res.on('end', () => finish(resolve, { status: res.statusCode ?? 0, body: out }))
    })
    req.on('timeout', () => req.destroy(new Error(`timeout after ${timeoutMs}ms`)))
    req.on('error', (e) => finish(reject, e instanceof Error ? e : new Error(String(e))))
    req.end(body)
  })
}

function getJson(url, timeoutMs, caFile, headers) {
  return new Promise((resolve, reject) => {
    let settled = false
    const finish = (fn, v) => { if (!settled) { settled = true; fn(v) } }
    const u = new URL(url)
    const lib = u.protocol === 'https:' ? httpsRequest : httpRequest
    const options = {
      method: 'GET', hostname: u.hostname,
      port: u.port || (u.protocol === 'https:' ? 443 : 80),
      path: u.pathname + u.search, agent: false, timeout: timeoutMs,
      headers: headers || {},
    }
    if (u.protocol === 'https:') {
      options.rejectUnauthorized = true
      if (caFile) options.ca = readFileSync(caFile)
    }
    const req = lib(options, (res) => {
      let out = ''
      res.setEncoding('utf8')
      res.on('data', (c) => { out += c })
      res.on('end', () => finish(resolve, { status: res.statusCode ?? 0, body: out }))
    })
    req.on('timeout', () => req.destroy(new Error(`timeout after ${timeoutMs}ms`)))
    req.on('error', (e) => finish(reject, e instanceof Error ? e : new Error(String(e))))
    req.end()
  })
}

export function apply(ctx, config) {
  const base = String(config.relayUrl || '').replace(/\/+$/, '')
  const ca = config.caFile || undefined
  let token = ''
  try { token = readFileSync(config.accessTokenFile, 'utf8').trim() } catch (e) {
    console.error('[approval-push] cannot read accessTokenFile:', e.message)
    return
  }
  if (!token) { console.error('[approval-push] accessTokenFile is empty'); return }
  const headers = { 'X-DSH-Relay-Token': token }

  // ── pending asks: id → {resolve, timer, sessionId, toolName} ──
  const pending = new Map()
  // approval/asked id → request id correlation: the waterfall gives us the
  // request BEFORE the audit event lands; the audit id is minted inside
  // approval.request(). We key pending by sessionId+toolName+callId and
  // reconcile with the asked event when it arrives.
  const askedIndex = new Map() // auditId → pendingKey

  const pendingKey = (req) => `${req.agent?.session?.id ?? '?'}/${req.callId ?? req.toolName}`

  let onlineCache = { at: 0, value: false }
  const phoneOnline = async () => {
    const now = Date.now()
    if (now - onlineCache.at < ONLINE_CACHE_MS) return onlineCache.value
    try {
      const res = await getJson(`${base}/__dsh_tunnel__/notify/online`, 5000, ca, headers)
      onlineCache = { at: now, value: res.status === 200 && JSON.parse(res.body).online === true }
    } catch {
      onlineCache = { at: now, value: false } // unreachable relay = no subscriber = never claim
    }
    return onlineCache.value
  }

  let devicesCache = { at: 0, ids: [] }
  const targetDevices = async () => {
    if (config.deviceId) return [config.deviceId]
    const now = Date.now()
    if (now - devicesCache.at < DEVICES_CACHE_MS && devicesCache.ids.length) return devicesCache.ids
    try {
      const res = await getJson(`${base}/__dsh_tunnel__/pair/devices`, 5000, ca, headers)
      const body = JSON.parse(res.body)
      devicesCache = { at: now, ids: (body.devices || []).map((d) => d.deviceId).filter(Boolean) }
    } catch { /* keep last known list; a stale list beats no push */ }
    return devicesCache.ids
  }

  const pushCard = async (message) => {
    const ids = await targetDevices()
    for (const deviceId of ids) {
      try {
        await postJson(`${base}/__dsh_tunnel__/notify/push`, 8000, ca, headers, { deviceId, message })
      } catch (e) {
        console.warn('[approval-push] push failed:', e.message)
      }
    }
  }

  const settlePending = (key, outcome) => {
    const p = pending.get(key)
    if (!p) return false
    pending.delete(key)
    clearTimeout(p.timer)
    p.resolve(outcome)
    return true
  }

  // ── claim answerer ──
  if (config.claimApprovals) {
    ctx.on('approval/request', async (req, next) => {
      if (!(await phoneOnline())) return next()
      const key = pendingKey(req)
      const outcome = await new Promise((resolve) => {
        const timer = setTimeout(() => {
          pending.delete(key)
          resolve(undefined) // fail-closed: service normalizes to 'unavailable'
        }, config.askTimeoutMs)
        pending.set(key, { resolve, timer, sessionId: String(req.agent?.session?.id ?? ''), toolName: req.toolName })
        req.signal?.addEventListener?.('abort', () => {
          if (pending.delete(key)) { clearTimeout(timer); resolve('cancelled') }
        }, { once: true })
        void pushCard({
          kind: 'approval',
          id: key,
          sessionId: String(req.agent?.session?.id ?? ''),
          toolName: String(req.toolName ?? ''),
          reason: String(req.displayReason?.zh ?? req.displayReason?.en ?? req.reason ?? '').slice(0, 200),
          ts: Date.now(),
        })
      })
      return outcome ?? next()
    })
  }

  // ── event source: audit pairs + questions ──
  const lastSeq = new Map()
  const handleEvent = (sessionId, event) => {
    const seq = Number(event?.seq)
    if (Number.isFinite(seq)) {
      const wm = lastSeq.get(sessionId) ?? -1
      if (seq <= wm) return
      lastSeq.set(sessionId, seq)
      if (lastSeq.size > 500) {
        const first = lastSeq.keys().next().value
        lastSeq.delete(first)
      }
    }
    if (event?.type === 'approval/asked') {
      // correlate audit id with our pending key for answer routing
      const d = event.data ?? event.payload ?? {}
      const key = `${sessionId}/${d.callId ?? d.toolName}`
      if (pending.has(key)) askedIndex.set(String(d.id), key)
    } else if (event?.type === 'approval/decided') {
      const d = event.data ?? event.payload ?? {}
      const key = askedIndex.get(String(d.id))
      askedIndex.delete(String(d.id))
      if (key && pending.has(key)) {
        // GUI (or another answerer) decided first: adopt the durable outcome
        // and retract the phone card.
        const outcome = ['allowed-once', 'rejected', 'cancelled'].includes(d.outcome) ? d.outcome : undefined
        settlePending(key, outcome)
        void pushCard({ kind: 'retract', id: key, ts: Date.now() })
      }
    } else if (event?.type === 'tool/call') {
      const d = event.data ?? event.payload ?? {}
      const name = d.name ?? d.toolName
      if (name === 'ask_user_question') {
        // Privacy: no question text on the card.
        void pushCard({ kind: 'question', id: `${sessionId}/${d.callId ?? seq}`, sessionId, ts: Date.now() })
      }
    }
  }

  ctx.on('session/event', (session, event) => {
    try {
      if (session?.id !== undefined) handleEvent(String(session.id), event)
    } catch (e) { console.error('[approval-push] event error:', e.message) }
  })

  // ── answer long poll ──
  let stopped = false
  const pollLoop = async () => {
    while (!stopped) {
      try {
        const res = await getJson(`${base}/__dsh_tunnel__/notify/answers?wait=${config.pollWaitSeconds}`, (config.pollWaitSeconds + 10) * 1000, ca, headers)
        if (res.status !== 200) { await sleep(5000); continue }
        const body = JSON.parse(res.body)
        for (const a of body.answers || []) {
          const decision = a.decision === 'allowed-once' ? 'allowed-once' : a.decision === 'rejected' ? 'rejected' : null
          if (!decision) continue
          // Answer id is the pending key (we pushed it as the card id).
          if (!settlePending(String(a.id), decision)) {
            // Maybe it names the audit id (older card): try the index.
            const key = askedIndex.get(String(a.id))
            if (key) settlePending(key, decision)
          }
        }
      } catch {
        await sleep(5000) // relay unreachable: back off, never crash the host
      }
    }
  }
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
  void pollLoop()

  ctx.effect(() => () => {
    stopped = true
    for (const [, p] of pending) { clearTimeout(p.timer); p.resolve(undefined) }
    pending.clear()
  })

  console.log('[approval-push] mounted', JSON.stringify({ relay: base, claim: config.claimApprovals, timeoutMs: config.askTimeoutMs }))
}

/** Test-only seams. */
export const internals = { postJson, getJson }