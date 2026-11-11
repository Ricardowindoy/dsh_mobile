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
import Schema from './src/schema.js'

export const Config = Schema.object({
  // Relay base URL (no trailing slash), e.g. https://8.148.8.249:24000
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
  // Claim ask_user_question only when the phone can answer it in one tap
  // (one single-select question with 1..3 options). Other questions are
  // still mirrored as informational cards but stay on the desktop.
  claimQuestions: Schema.boolean().default(true),
  // A claimed question nobody answers fails as ASK_TIMED_OUT after this long.
  questionTimeoutMs: Schema.number().default(110_000).min(5_000).max(300_000),
  // Push a card with the session title when a turn finishes.
  notifyDone: Schema.boolean().default(true),
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
/**
 * Turn-end reasons that deserve a card, mapped to the card's status word.
 * completed/max-tokens = normal finish; aborted = the user or the runtime
 * interrupted the turn; error/blocked = the turn could not finish.
 */
const TURN_END_NOTICES = new Map([
  ['completed', 'completed'],
  ['max-tokens', 'completed'],
  ['aborted', 'interrupted'],
  ['error', 'error'],
  ['blocked', 'blocked'],
])
/** Cooldown between completion cards of the SAME session. */
const DONE_COOLDOWN_MS = 60_000
/** Max one-tap options an Android notification can show as actions. */
const MAX_INLINE_OPTIONS = 3

/** Join an assistant message's text blocks into one collapsed line. */
function textOfMessage(message) {
  if (message === null || typeof message !== 'object') return ''
  const blocks = Array.isArray(message.content) ? message.content : []
  let out = ''
  for (const block of blocks) {
    if (block !== null && typeof block === 'object' && block.type === 'text' && typeof block.text === 'string') {
      out += block.text
    }
  }
  return out.replace(/\s+/g, ' ').trim()
}

/** Ellipsize a preview to maxChars characters (code points, not UTF-16 units). */
function clipPreview(text, maxChars = 160) {
  if (typeof text !== 'string') return ''
  const chars = Array.from(text)
  if (chars.length <= maxChars) return text
  return `${chars.slice(0, maxChars).join('')}…`
}

/**
 * One-line summary for a completion card: the first real paragraph of the
 * reply, markdown stripped, clipped hard. The card is a nudge ("something
 * finished"), not a transcript — pushing the whole reply is the complaint
 * this replaces.
 */
function summarizeReply(markdown, maxChars = 60) {
  if (typeof markdown !== 'string') return ''
  const blocks = markdown.split(/\n\s*\n/)
  for (const block of blocks) {
    const plain = plainText(block).replace(/^\s*#+\s*/, '').trim()
    if (plain.length > 0) return clipPreview(plain, maxChars)
  }
  return ''
}

/**
 * Turn an assistant reply into plain notification text.
 *
 * The completion card used to show the raw Markdown source (`**bold**`,
 * `##`, table pipes, code fences), which reads as noise on a lock screen.
 * Strip the syntax but keep the words, numbers and line order.
 */
function plainText(markdown) {
  if (typeof markdown !== 'string') return ''
  let out = markdown
  out = out.replace(/```[\s\S]*?```/g, ' ')       // fenced code blocks
  out = out.replace(/`([^`]*)`/g, '$1')            // inline code
  out = out.replace(/!\[[^\]]*\]\([^)]*\)/g, ' ')  // images
  out = out.replace(/\[([^\]]*)\]\([^)]*\)/g, '$1') // links -> label
  out = out.replace(/^\s{0,3}#{1,6}\s+/gm, '')     // headings
  out = out.replace(/^\s{0,3}>\s?/gm, '')          // blockquotes
  out = out.replace(/^\s*[-*+]\s+/gm, '')          // list bullets
  out = out.replace(/^\s*\d+\.\s+/gm, '')          // ordered list markers
  out = out.replace(/^\s*\|?[\s:-]*\|[\s:|-]*$/gm, ' ') // table separator rows
  out = out.replace(/\|/g, ' ')                    // table cell separators
  out = out.replace(/\*\*([^*]*)\*\*/g, '$1')      // bold
  out = out.replace(/(^|[^*])\*([^*\n]+)\*/g, '$1$2') // italics
  out = out.replace(/~~([^~]*)~~/g, '$1')          // strikethrough
  return out.replace(/\s+/g, ' ').trim()
}

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

  // ── pending one-tap questions: card id → {resolve, timer} ──
  const pendingQuestions = new Map()
  // Completion-card bookkeeping: latest assistant text per `session#turn`,
  // and the last completion card time per session (anti-spam cooldown).
  const turnText = new Map()
  const lastDoneAt = new Map()

  // Session titles arrive from the session-title service. Read it lazily:
  // `ctx.get` can be undefined during early activation, and a cached
  // undefined would silently degrade every card to the session id.
  const titleOf = (session, sessionId) => {
    try {
      const svc = ctx.get('sessionTitle')
      if (svc === undefined || svc === null) return ''
      const snap = svc.get(session)
      if (snap !== null && typeof snap === 'object' && typeof snap.title === 'string' && snap.title.length > 0) {
        return snap.title
      }
    } catch { /* title is cosmetic; never fail a card over it */ }
    return ''
  }

  // callId 缺失时（部分 asker 不提供）只用 session/tool 会碰撞：同一会话
  // 里两个同工具审批会共用一个 pending 槽，后一个覆盖前一个，前者永远
  // 等不到结果（泄漏 + 超时）。补一个自增序号保证唯一。
  // 代价：无 callId 的请求无法与 approval/asked 的审计 id 关联，GUI 先答
  // 的 retract 关联退化为不可用（该场景本就罕见，且不影响 fail-closed）。
  let approvalKeySeq = 0
  const pendingKey = (req) => {
    const base = `${req.agent?.session?.id ?? '?'}/${req.callId ?? req.toolName}`
    return req.callId === undefined || req.callId === null ? `${base}#${++approvalKeySeq}` : base
  }

  let onlineCache = { at: 0, value: false }
  const phoneOnline = async () => {
    const now = Date.now()
    if (now - onlineCache.at < ONLINE_CACHE_MS) return onlineCache.value
    try {
      const res = await getJson(`${base}/__dsh_tunnel__/notify/online`, 5000, ca, headers)
      // `watching` 才是「真的有活跃 SSE 通道」；`online` 带 90s 宽限，用它在
      // 手机刚断线时认领审批，卡片谁都没收到，超时后还会被自动拒绝。
      // 老 relay 不返回 watching → 退回 online（向后兼容）。
      const body = res.status === 200 ? JSON.parse(res.body) : {}
      const value = body.online === true && (body.watching === undefined || body.watching === true)
      onlineCache = { at: now, value }
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

  /** Resolve a claimed question card with the phone's structured answer. */
  const settleQuestion = (cardId, answers) => {
    const p = pendingQuestions.get(cardId)
    if (!p) return false
    pendingQuestions.delete(cardId)
    clearTimeout(p.timer)
    p.resolve({ kind: 'answer', answer: { answers } })
    return true
  }

  // ── claim answerer ──
  if (config.claimApprovals) {
    ctx.on('approval/request', async (req, next) => {
      if (!(await phoneOnline())) return next()
      const key = pendingKey(req)
      const approvalSessionId = String(req.agent?.session?.id ?? '')
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
          sessionId: approvalSessionId,
          title: titleOf(req.agent?.session, approvalSessionId),
          toolName: String(req.toolName ?? ''),
          reason: String(req.displayReason?.zh ?? req.displayReason?.en ?? req.reason ?? '').slice(0, 200),
          ts: Date.now(),
        })
      })
      return outcome ?? next()
    })
  }

  // ── question answerer (one-tap answers from the notification) ──
  //
  // The question waterfall is agent-scoped; a host-composition listener is
  // untagged, and scopeTarget admits untagged listeners for every agent, so
  // this receives every session's questions. We claim ONLY when the phone can
  // answer in one tap: exactly one single-select question with 1..3 options
  // (Android shows at most three notification actions). Everything else is
  // mirrored as an informational card and stays on the desktop, because a
  // claimed question would remove the desktop card while giving the phone no
  // way to answer it.
  if (config.claimQuestions) {
    ctx.on('user-questions/request', async (req, next) => {
      const questions = Array.isArray(req.questions) ? req.questions : []
      const q = questions.length === 1 ? questions[0] : undefined
      const options = Array.isArray(q?.options)
        ? q.options.map((o) => String(o?.label ?? '')).filter((s) => s.length > 0)
        : []
      const sessionId = String(req.agent?.session?.id ?? '')
      const inline = q !== undefined
        && q.multiSelect !== true
        && options.length >= 1
        && options.length <= MAX_INLINE_OPTIONS

      if (!inline) {
        // No claim: keep the desktop card. Still nudge the phone with the
        // question text so the user knows something is waiting.
        if (await phoneOnline()) {
          void pushCard({
            kind: 'question',
            id: `qn:${sessionId}:${req.wait?.callId ?? Date.now()}`,
            sessionId,
            title: titleOf(req.agent?.session, sessionId),
            question: String(questions[0]?.question ?? ''),
            options: [],
            ts: Date.now(),
          })
        }
        return next()
      }

      if (!(await phoneOnline())) return next()

      const cardId = `q:${sessionId}:${req.wait?.callId ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`}`
      const outcome = await new Promise((resolve) => {
        const timer = setTimeout(() => {
          pendingQuestions.delete(cardId)
          resolve({ kind: 'timeout' }) // fail-closed: never fabricate an answer
        }, config.questionTimeoutMs)
        pendingQuestions.set(cardId, { resolve, timer })
        req.signal?.addEventListener?.('abort', () => {
          // 取消 ≠ 超时：问请求被撤销时应如实报 cancelled，让上层知道
          // 不是「用户没理」，而是这次提问根本没机会被回答。
          if (pendingQuestions.delete(cardId)) { clearTimeout(timer); resolve({ kind: 'cancelled' }) }
        }, { once: true })
        void pushCard({
          kind: 'question',
          id: cardId,
          sessionId,
          title: titleOf(req.agent?.session, sessionId),
          questionId: String(q.id ?? ''),
          question: String(q.question ?? ''),
          options,
          multiSelect: false,
          ts: Date.now(),
        })
      })
      if (outcome && outcome.kind === 'answer') return outcome.answer
      // Silence is not an answer. An empty selection would read as "the user
      // skipped", so a timeout must surface as a timeout instead.
      const cancelled = outcome && outcome.kind === 'cancelled'
      throw Object.assign(new Error(cancelled
        ? 'the question was cancelled before an answer arrived'
        : 'the phone did not answer the question in time'), {
        name: 'UserQuestionError',
        code: cancelled ? 'ASK_CANCELLED' : 'ASK_TIMED_OUT',
      })
    })
  }

  // ── event source: audit pairs + questions + completions ──
  const lastSeq = new Map()
  const handleEvent = (sessionId, event, session) => {
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
    } else if (event?.type === 'assistant/message') {
      // Keep the last non-empty assistant text of each turn as the completion
      // card's preview (same convention as the Windows popup bridge).
      const d = event.data ?? event.payload ?? {}
      const turn = Number.isFinite(d.turn) ? d.turn : -1
      if (turn < 0) return
      const text = textOfMessage(d.message)
      if (text.length > 0) {
        turnText.set(`${sessionId}#${turn}`, text)
        if (turnText.size > 500) {
          const first = turnText.keys().next().value
          turnText.delete(first)
        }
      }
    } else if (event?.type === 'turn/end') {
      if (!config.notifyDone) return
      const d = event.data ?? event.payload ?? {}
      const reason = d.reason
      const kind = reason !== null && typeof reason === 'object' ? reason.kind : ''
      const status = TURN_END_NOTICES.get(kind)
      if (status === undefined) return
      const turn = Number.isFinite(d.turn) ? d.turn : -1
      if (turn < 0) return
      const key = `${sessionId}#${turn}`
      let preview = turnText.get(key) || ''
      turnText.delete(key)
      if (status === 'interrupted' && preview.length === 0) preview = '会话已中断'
      if (status === 'error' && preview.length === 0) preview = '本轮执行出错'
      if (status === 'blocked' && preview.length === 0) preview = '本轮被阻塞'
      // Subagent sessions finish constantly; their cards are noise on a phone.
      if (session?.origin === 'subagent' || session?.parentSession !== undefined) return
      const now = Date.now()
      // Cooldown only damps normal completions; an interruption/error is rare
      // and worth surfacing even if a completion just happened.
      if (status === 'completed' && now - (lastDoneAt.get(sessionId) || 0) < DONE_COOLDOWN_MS) return
      lastDoneAt.set(sessionId, now)
      if (lastDoneAt.size > 200) {
        const first = lastDoneAt.keys().next().value
        lastDoneAt.delete(first)
      }
      void pushCard({
        kind: 'done',
        status,
        // Stable per SESSION, not per turn: Android replaces a notification
        // with the same id, so a long session updates one slot instead of
        // stacking one card per reply (the "repeatedly pushed" complaint).
        id: `done:${sessionId}`,
        sessionId,
        title: titleOf(session, sessionId),
        preview: summarizeReply(preview, 60),
        ts: now,
      })
    }
  }

  ctx.on('session/event', (session, event) => {
    try {
      if (session?.id !== undefined) handleEvent(String(session.id), event, session)
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
          // Structured question answer: {id: cardId, answers:[{id, selected, custom}]}
          if (Array.isArray(a.answers)) {
            settleQuestion(String(a.id), a.answers.map((item) => ({
              id: String(item?.id ?? ''),
              selected: Array.isArray(item?.selected) ? item.selected.map(String) : [],
              ...(typeof item?.custom === 'string' && item.custom.length > 0 ? { custom: item.custom } : {}),
            })))
            continue
          }
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
    for (const [, p] of pendingQuestions) { clearTimeout(p.timer); p.resolve(null) }
    pendingQuestions.clear()
  })

  console.log('[approval-push] mounted', JSON.stringify({
    relay: base,
    claim: config.claimApprovals,
    timeoutMs: config.askTimeoutMs,
    claimQuestions: config.claimQuestions,
    questionTimeoutMs: config.questionTimeoutMs,
    notifyDone: config.notifyDone,
  }))
}

/** Test-only seams. */
export const internals = { postJson, getJson, plainText, clipPreview, summarizeReply }