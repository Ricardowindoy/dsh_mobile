/**
 * Host half of the dsh remote-access management plugin.
 *
 * Management plane only: it observes the relay/probe and the local service
 * manager, exposes agent tools, a /remote-status command, a Settings section
 * and same-origin Fetch routes. It never owns the tunnel process lifecycle.
 *
 * Cross-platform: the same plugin runs on Linux (systemd user/system units)
 * and Windows (Windows services via sc.exe). Node (controlled machine)
 * switching is relay-side: each node is addressed as `<relayUrl>/d/<deviceId>/`.
 */

import { execFile } from 'node:child_process'
import { readFile, readdir, writeFile, mkdir, stat as fsStat } from 'node:fs/promises'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { homedir } from 'node:os'
import { get as httpsGet, request as httpsRequest } from 'node:https'
import { get as httpGet, request as httpRequest } from 'node:http'
import Schema from '@deepseek-ai/schemastery'

import {
  TUNNEL_PLATFORM_PREFIX, binaryName, channelPlatform, installRoot,
  fetchChannelEntry, installBinary, renderUnit, installUnitLinux,
  renderWindowsInstallScript, uninstallUnitLinux, UNIT_NAME,
} from './tunnel-install.mjs'
import { pairingPayload, pairingSvg, caKindOf } from './pairing.mjs'

// CommandDefinitionId is an identity function at runtime; a plain string
// avoids a compile-time-only dependency before installation.
const CommandDefinitionId = (id) => id

const PLATFORM = process.platform // 'linux' | 'win32' | 'darwin' | ...
const IS_WINDOWS = PLATFORM === 'win32'
const PROBE_TIMEOUT_MS = 8000
const CTL_TIMEOUT_MS = 8000

/** State file: DSH_HOME wins; Windows falls back to %APPDATA%\dsh. */
function stateFilePath() {
  const base = process.env.DSH_HOME
    || (IS_WINDOWS ? join(process.env.APPDATA || homedir(), 'dsh') : join(homedir(), '.dsh'))
  return join(base, 'remote-access-state.json')
}

/** Run a command, resolving stdout; non-zero exits carry stderr. */
function run(file, args, timeoutMs) {
  return new Promise((resolve, reject) => {
    execFile(file, args, { timeout: timeoutMs, encoding: 'utf8', windowsHide: true }, (error, stdout, stderr) => {
      if (error) {
        const reason = (stderr || error.message || '').trim().slice(0, 400)
        reject(new Error(`${file} failed: ${reason || `exit ${error.code}`}`))
        return
      }
      resolve(stdout)
    })
  })
}

/** HTTP(S) GET with a hard timeout. Supports plain HTTP for LAN endpoints and
 *  tests; HTTPS may pin a private CA via caFile. */
function requestUrl(url, timeoutMs, caFile, headers) {
  return new Promise((resolve, reject) => {
    let settled = false
    const finish = (fn, value) => { if (!settled) { settled = true; fn(value) } }
    const isHttps = url.startsWith('https:')
    const getter = isHttps ? httpsGet : httpGet
    const options = { timeout: timeoutMs, headers: headers || {} }
    if (isHttps) {
      options.rejectUnauthorized = true
      if (caFile) options.ca = readFileSync(caFile)
    }
    const req = getter(url, options, (res) => {
      let body = ''
      res.setEncoding('utf8')
      res.on('data', (chunk) => { body += chunk })
      res.on('end', () => finish(resolve, { status: res.statusCode ?? 0, body }))
    })
    req.on('timeout', () => { req.destroy(new Error(`timeout after ${timeoutMs}ms`)) })
    req.on('error', (error) => finish(reject, error instanceof Error ? error : new Error(String(error))))
  })
}

/** POST JSON to a relay control endpoint (private-CA aware). Returns {status, body}
 *  or throws; used to sync the relay-side default node on node switch.
 *  Transport history (all measured 2026-11-08, dsh-web process):
 *   - httpsRequest(URL, {method:'POST'}) with NODE_USE_ENV_PROXY=1: method was
 *     downgraded to GET on the wire (relay's GET echo answered 200 → silent no-op).
 *   - fetch + per-request undici dispatcher: "fetch failed" — dsh installs a
 *     global fetch/policy layer and the dispatcher option is not honored there.
 *   - What works: httpsRequest with a PLAIN OPTIONS OBJECT (no URL arg) and
 *     agent:false — a direct socket, which bypasses both the env-proxy layer
 *     and any global dispatcher. Direct egress to the relay IP is fine. */
function postJson(url, timeoutMs, caFile, headers, payload) {
  return new Promise((resolve, reject) => {
    let settled = false
    const finish = (fn, value) => { if (!settled) { settled = true; fn(value) } }
    const u = new URL(url)
    const lib = u.protocol === 'https:' ? httpsRequest : httpRequest
    const body = JSON.stringify(payload)
    const options = {
      method: 'POST',
      hostname: u.hostname,
      port: u.port || (u.protocol === 'https:' ? 443 : 80),
      path: u.pathname + u.search,
      agent: false, // direct socket: no env proxy, no global dispatcher, no pooling
      timeout: timeoutMs,
      headers: { ...(headers || {}), 'content-type': 'application/json', 'content-length': Buffer.byteLength(body) },
    }
    if (u.protocol === 'https:') {
      options.rejectUnauthorized = true
      if (caFile) options.ca = readFileSync(caFile)
    }
    const req = lib(options, (res) => {
      let out = ''
      res.setEncoding('utf8')
      res.on('data', (chunk) => { out += chunk })
      res.on('end', () => finish(resolve, { status: res.statusCode ?? 0, body: out }))
    })
    req.on('timeout', () => { req.destroy(new Error(`timeout after ${timeoutMs}ms`)) })
    req.on('error', (error) => finish(reject, error instanceof Error ? error : new Error(String(error))))
    req.end(body)
  })
}

/** Set the relay-side default node (root-path routing target). Best-effort:
 *  the local switch always applies; the relay sync failing (old relay without
 *  the endpoint, network hiccup) is reported but not fatal. */
async function setRelayDefaultNode(node, deviceId, token) {
  const base = String(node.relayUrl || '').replace(/\/+$/, '')
  const url = `${base}/__dsh_tunnel__/default-node`
  const headers = token ? { 'X-DSH-Relay-Token': token } : undefined
  try {
    const res = await postJson(url, PROBE_TIMEOUT_MS, node.caFile || undefined, headers, { deviceId })
    if (res.status !== 200) {
      console.warn(`[remote-access] default-node sync ${res.status} url=${url} token=${token ? 'present(' + token.length + ')' : 'MISSING'} body=${res.body.slice(0, 80)}`)
      return { ok: false, status: res.status, body: res.body.slice(0, 120) }
    }
    return { ok: true, body: res.body.slice(0, 120) }
  } catch (error) {
    console.warn(`[remote-access] default-node sync error url=${url} token=${token ? 'present(' + token.length + ')' : 'MISSING'}: ${error.message}`)
    return { ok: false, error: error.message }
  }
}

/** Read the relay's current default node (the authoritative "active node"
 *  for App routing). Returns the paired deviceId, or '' when the relay has
 *  none / is unreachable / lacks the endpoint. Never invents state. */
async function relayDefaultNode(node, token) {
  const base = String(node.relayUrl || '').replace(/\/+$/, '')
  const url = `${base}/__dsh_tunnel__/default-node`
  const headers = token ? { 'X-DSH-Relay-Token': token } : undefined
  try {
    const res = await requestUrl(url, PROBE_TIMEOUT_MS, node.caFile || undefined, headers)
    if (res.status !== 200) return ''
    const data = JSON.parse(res.body)
    return typeof data?.deviceId === 'string' ? data.deviceId : ''
  } catch {
    return ''
  }
}

/** Parse one relay probe payload. Never invents state. */
function parseProbe(status, body) {
  try {
    const data = JSON.parse(body)
    if (data && typeof data === 'object' && 'ok' in data) {
      return {
        reachable: true,
        httpStatus: status,
        authRequired: data.authRequired === true,
        deviceOnline: data.deviceOnline === true,
        deviceCount: typeof data.deviceCount === 'number' ? data.deviceCount : null,
        devices: Array.isArray(data.devices)
          ? data.devices
            .filter((d) => d && typeof d.id === 'string')
            .map((d) => ({ id: d.id, online: d.online === true }))
          : null,
      }
    }
    return { reachable: true, httpStatus: status, note: 'unrecognized probe payload', raw: body.slice(0, 200) }
  } catch {
    return { reachable: status < 500, httpStatus: status, note: 'probe returned non-JSON', raw: body.slice(0, 200) }
  }
}

/** Map `sc.exe query` output to a normalized state. Locale-proof: uses the
 *  numeric STATE code, not localized words. 4=running 1=stopped 2/3=pending. */
function parseWindowsServiceState(output) {
  const m = /STATE\s*:\s*(\d+)/i.exec(output)
  if (!m) return 'unknown'
  switch (Number(m[1])) {
    case 1: return 'stopped'
    case 2: return 'start_pending'
    case 3: return 'stop_pending'
    case 4: return 'running'
    case 5: return 'continue_pending'
    case 6: return 'pause_pending'
    case 7: return 'paused'
    default: return 'unknown'
  }
}

/** Resolve a service descriptor's state on the current platform. */
async function serviceState(svc) {
  const kind = svc.kind || (IS_WINDOWS ? 'windows-service' : 'systemd-user')
  try {
    if (kind === 'windows-service') {
      const out = await run('sc.exe', ['query', svc.name], CTL_TIMEOUT_MS)
      return parseWindowsServiceState(out)
    }
    const args = kind === 'systemd-system'
      ? ['show', '-p', 'ActiveState', '--value', svc.name]
      : ['--user', 'show', '-p', 'ActiveState', '--value', svc.name]
    const out = (await run('systemctl', args, CTL_TIMEOUT_MS)).trim()
    return out || 'unknown'
  } catch {
    return 'unknown'
  }
}

/** Restart a service. Windows uses sc.exe stop/start with bounded polling. */
async function serviceRestart(svc) {
  const kind = svc.kind || (IS_WINDOWS ? 'windows-service' : 'systemd-user')
  if (kind === 'windows-service') {
    await run('sc.exe', ['stop', svc.name], CTL_TIMEOUT_MS).catch(() => {})
    for (let i = 0; i < 20; i++) {
      if (await serviceState(svc) === 'stopped') break
      await new Promise((r) => setTimeout(r, 500))
    }
    await run('sc.exe', ['start', svc.name], CTL_TIMEOUT_MS)
    for (let i = 0; i < 20; i++) {
      const state = await serviceState(svc)
      if (state === 'running') return state
      await new Promise((r) => setTimeout(r, 500))
    }
    return await serviceState(svc)
  }
  const args = kind === 'systemd-system'
    ? ['restart', svc.name]
    : ['--user', 'restart', svc.name]
  await run('systemctl', args, CTL_TIMEOUT_MS)
  return await serviceState(svc)
}

/** Default service set when the config names none. */
function defaultServices(platform = PLATFORM) {
  return platform === 'win32'
    ? [
        { id: 'tunnel', label: 'dsh-tunnel', kind: 'windows-service', name: 'dsh-tunnel' },
        { id: 'forwarder', label: 'dsh-forwarder-client', kind: 'windows-service', name: 'dsh-forwarder-client' },
      ]
    : [
        { id: 'tunnel', label: 'dsh-tunnel.service', kind: 'systemd-user', name: 'dsh-tunnel.service' },
        { id: 'frpc', label: 'frpc.service', kind: 'systemd-user', name: 'frpc.service' },
        { id: 'frpc-gateway', label: 'frpc-gateway-8326.service', kind: 'systemd-user', name: 'frpc-gateway-8326.service' },
      ]
}

/** Resolve the service descriptors for one endpoint (explicit config wins,
 *  then legacy per-endpoint unit names, then platform defaults). */
function resolveServices(config, endpoint, platform = PLATFORM) {
  if (Array.isArray(config.services) && config.services.length > 0) return config.services
  if (platform !== 'win32') {
    const derived = []
    if (endpoint?.tunnelUnit) derived.push({ id: 'tunnel', label: endpoint.tunnelUnit, kind: 'systemd-user', name: endpoint.tunnelUnit })
    if (endpoint?.frpcUnit) derived.push({ id: 'frpc', label: endpoint.frpcUnit, kind: 'systemd-user', name: endpoint.frpcUnit })
    if (endpoint?.frpcGatewayUnit) derived.push({ id: 'frpc-gateway', label: endpoint.frpcGatewayUnit, kind: 'systemd-user', name: endpoint.frpcGatewayUnit })
    if (derived.length > 0) return derived
  }
  return defaultServices(platform)
}

/** Last N lines of a log file, tail-trimmed. */
async function tailFile(path, lines) {
  let text
  try {
    text = await readFile(path, 'utf8')
  } catch (error) {
    return { path, error: `unreadable: ${error.message}` }
  }
  const all = text.split('\n').filter((line) => line.length > 0)
  return { path, totalLines: all.length, tail: all.slice(-lines) }
}

/** Tunnel lifecycle tail + recent disconnect reasons. */
async function tunnelLogSummary(logDir, tailLines) {
  const summary = { lifecycle: null, recentDisconnects: [] }
  if (!logDir) { summary.error = 'logDir not configured'; return summary }
  let names
  try {
    names = await readdir(logDir)
  } catch (error) {
    summary.error = `log dir unreadable: ${error.message}`
    return summary
  }
  const lifecycleFile = names.filter((n) => n.startsWith('lifecycle')).sort().at(-1)
  if (lifecycleFile === undefined) {
    summary.error = `no lifecycle* file in ${logDir}`
    return summary
  }
  const tail = await tailFile(join(logDir, lifecycleFile), tailLines)
  summary.lifecycle = tail
  const eventLines = tail.tail === undefined ? [] : tail.tail.filter((l) => /disconnect|reconnect|closed|dial|fail/i.test(l))
  summary.recentDisconnects = eventLines.slice(-8)
  return summary
}

/** frpc proxy table (Linux only; returns an explicit error elsewhere). */
async function frpcProxies(configPath) {
  if (!configPath) return { config: null, error: 'not configured' }
  let text
  try {
    text = await readFile(configPath, 'utf8')
  } catch (error) {
    return { config: configPath, error: `unreadable: ${error.message}` }
  }
  const serverAddr = /serverAddr\s*=\s*"([^"]+)"/.exec(text)?.[1] ?? null
  const serverPort = /serverPort\s*=\s*(\d+)/.exec(text)?.[1] ?? null
  const proxies = []
  for (const block of text.split('[[').slice(1)) {
    const name = /name\s*=\s*"([^"]+)"/.exec(block)?.[1] ?? null
    const type = /type\s*=\s*"([^"]+)"/.exec(block)?.[1] ?? null
    const localPort = /localPort\s*=\s*(\d+)/.exec(block)?.[1] ?? null
    const remotePort = /remotePort\s*=\s*(\d+)/.exec(block)?.[1] ?? null
    if (name !== null) proxies.push({ name, type, localPort, remotePort })
  }
  return { config: configPath, server: serverAddr === null ? null : `${serverAddr}:${serverPort ?? '?'}`, proxies }
}

/** Read an access token from a file (trimmed), or ''. */
async function readTokenFile(path) {
  if (!path) return ''
  try {
    return (await readFile(path, 'utf8')).trim()
  } catch {
    return ''
  }
}

/** Probe one relay endpoint. An access token, when available, also returns the
 *  per-device online list (the relay only exposes it to authorized callers). */
async function probeRelay(endpoint, token) {
  const url = new URL(endpoint.probeUrl)
  // The relay authorizes the per-device list via header/cookie only; putting a
  // token in the query would leak it into reverse-proxy access logs.
  const headers = token ? { 'X-DSH-Relay-Token': token } : undefined
  try {
    const res = await requestUrl(url.toString(), PROBE_TIMEOUT_MS, endpoint.caFile || undefined, headers)
    return { probeUrl: endpoint.probeUrl, ...parseProbe(res.status, res.body) }
  } catch (error) {
    return { probeUrl: endpoint.probeUrl, reachable: false, error: error.message }
  }
}

/** Node view: relay URL + device id + canonical control URL. */
function nodeURL(node) {
  const base = String(node.relayUrl || '').replace(/\/+$/, '')
  return `${base}/d/${encodeURIComponent(node.deviceId)}/`
}

function nodeView(node, active) {
  return {
    id: node.id,
    label: node.label,
    deviceId: node.deviceId,
    relayUrl: node.relayUrl,
    url: nodeURL(node),
    active,
    accessTokenFile: node.accessTokenFile || null,
  }
}

/** Status for one node: online/offline/unknown, derived only from observation. */
async function nodeStatus(node, ambiguous = false) {
  const token = await readTokenFile(node.accessTokenFile)
  const probe = await probeRelay({
    probeUrl: `${String(node.relayUrl).replace(/\/+$/, '')}/__dsh_tunnel__/probe`,
    caFile: node.caFile,
  }, token)
  const base = { id: node.id, label: node.label, deviceId: node.deviceId, url: nodeURL(node), probe }
  if (!probe.reachable) return { ...base, online: 'unknown', source: 'unreachable' }
  if (Array.isArray(probe.devices)) {
    const hit = probe.devices.find((d) => d.id === node.deviceId)
    return { ...base, online: hit ? hit.online : 'unknown', source: 'probe-devices' }
  }
  // Older relay: only aggregate counts. A single configured device is
  // unambiguous; with several configured nodes the aggregate cannot say which
  // one is online, so report unknown instead of guessing.
  if (!ambiguous && probe.deviceCount === 1) return { ...base, online: probe.deviceOnline ? true : false, source: 'probe-aggregate' }
  return { ...base, online: 'unknown', source: 'probe-aggregate-ambiguous' }
}

/** One status snapshot for one endpoint. */
async function collectStatus(endpoint, services, tailLines) {
  const token = await readTokenFile(endpoint.accessTokenFile)
  const [relay, serviceStates, logs] = await Promise.all([
    probeRelay(endpoint, token),
    Promise.all(services.map(async (svc) => ({
      id: svc.id, label: svc.label, kind: svc.kind, name: svc.name, state: await serviceState(svc),
    }))),
    tunnelLogSummary(endpoint.logDir, tailLines),
  ])
  return { relay, services: serviceStates, logs, collectedAt: new Date().toISOString() }
}

/** Verdict from observed facts only. */
function verdict(snapshot) {
  const relayOk = snapshot.relay.reachable === true && snapshot.relay.deviceOnline === true
  const tunnel = snapshot.services.find((s) => s.id === 'tunnel')
  const tunnelOk = tunnel === undefined || tunnel.state === 'active' || tunnel.state === 'running'
  if (relayOk && tunnelOk) return 'ok: relay reachable and device linked, tunnel service active'
  if (snapshot.relay.reachable === false) return 'degraded: relay probe unreachable from this machine'
  if (snapshot.relay.deviceOnline === false) return 'degraded: relay reachable but no device linked (tunnel side down or pairing lost)'
  if (!tunnelOk) return `degraded: tunnel service is ${tunnel.state}`
  return 'unclear: see raw fields'
}

/** Config schema. All platform-specific values are optional: the same config
 *  file can describe both a Linux host (systemd units) and a Windows host
 *  (Windows services), with `services` overriding the derived defaults. */
export const Config = Schema.object({
  endpoints: Schema.array(Schema.object({
    id: Schema.string().required(),
    label: Schema.string().required(),
    probeUrl: Schema.string().required(),
    caFile: Schema.string().default(''),
    accessTokenFile: Schema.string().default(''),
    tunnelUnit: Schema.string().default(IS_WINDOWS ? 'dsh-tunnel' : 'dsh-tunnel.service'),
    frpcUnit: Schema.string().default(IS_WINDOWS ? '' : 'frpc.service'),
    frpcGatewayUnit: Schema.string().default(IS_WINDOWS ? '' : 'frpc-gateway-8326.service'),
    // 本机部署的默认日志目录；随 cordis.patch.yml 的 config 显式覆盖（机器特定路径不入库）
    logDir: Schema.string().default(''),
    frpcConfig: Schema.string().default(''),
    frpcGatewayConfig: Schema.string().default(''),
  })).default([]),
  nodes: Schema.array(Schema.object({
    id: Schema.string().required(),
    label: Schema.string().required(),
    deviceId: Schema.string().required(),
    relayUrl: Schema.string().required(),
    caFile: Schema.string().default(''),
    accessTokenFile: Schema.string().default(''),
  })).default([]),
  // Explicit service descriptors override per-endpoint legacy unit fields.
  services: Schema.array(Schema.object({
    id: Schema.string().required(),
    label: Schema.string().default(''),
    kind: Schema.string().default(IS_WINDOWS ? 'windows-service' : 'systemd-user'),
    name: Schema.string().required(),
  })).default([]),
  activeEndpoint: Schema.string().default(''),
  activeNode: Schema.string().default(''),
  logTailLines: Schema.number().default(40).min(5).max(200),
  allowRestart: Schema.boolean().default(false),
  // ── dsh-tunnel data-plane installer（托管安装，不托管运行）──
  // relayBase：从哪个 relay 的 update 通道下载二进制（默认取第一个 endpoint 的
  // probeUrl 去掉 /__dsh_tunnel__/probe）。allowInstall 是总闸：默认关，
  // 开启后设置页/Agent 才能真的写文件、写 unit、启服务。
  tunnelInstall: Schema.object({
    allowInstall: Schema.boolean().default(false),
    relayBase: Schema.string().default(''),
    deviceId: Schema.string().default(''),
    localAddr: Schema.string().default('127.0.0.1:3082'),
    pairingTokenFile: Schema.string().default(''),
    caFile: Schema.string().default(''),
    accessTokenFile: Schema.string().default(''),
    extraArgs: Schema.string().default(''),
  }).default({}),
})

export const inject = ['tools', 'commands', 'skills']

export function apply(ctx, config) {
  const endpoints = config.endpoints
  const nodes = config.nodes
  const byEndpoint = new Map(endpoints.map((ep) => [ep.id, ep]))
  const byNode = new Map(nodes.map((n) => [n.id, n]))

  const loadState = async () => {
    try {
      const data = JSON.parse(await readFile(stateFilePath(), 'utf8'))
      return {
        activeEndpoint: typeof data?.activeEndpoint === 'string' && byEndpoint.has(data.activeEndpoint) ? data.activeEndpoint : '',
        activeNode: typeof data?.activeNode === 'string' && byNode.has(data.activeNode) ? data.activeNode : '',
      }
    } catch {
      return { activeEndpoint: '', activeNode: '' }
    }
  }

  const saveState = async (patch) => {
    const current = await loadState()
    const next = { ...current, ...patch }
    if (next.activeEndpoint && !byEndpoint.has(next.activeEndpoint)) throw new Error(`unknown endpoint "${next.activeEndpoint}"`)
    if (next.activeNode && !byNode.has(next.activeNode)) throw new Error(`unknown node "${next.activeNode}"`)
    const path = stateFilePath()
    await mkdir(dirname(path), { recursive: true })
    await writeFile(path, `${JSON.stringify(next, null, 2)}\n`, 'utf8')
    return next
  }

  /** 服务端唯一切换写入：持久化状态 + POST relay default-node。
   *  工具 remote_access_switch 与 POST /api/remote-access/config 都只调这里，
   *  不再各自拼一遍 saveState+setRelayDefaultNode（2026-11-10「只留一套」）。 */
  const applySwitch = async (patch) => {
    const next = await saveState(patch)
    let relaySync = null
    if (next.activeNode) {
      const node = byNode.get(next.activeNode)
      if (node) {
        const token = await readTokenFile(node.accessTokenFile)
        relaySync = await setRelayDefaultNode(node, node.deviceId, token)
      } else {
        console.warn(`[remote-access] switch node=${next.activeNode} NOT IN byNode (nodes=${[...byNode.keys()].join(',')})`)
      }
    }
    return { ...next, relaySync }
  }

  const resolveEndpoint = async (explicitId) => {
    const state = await loadState()
    const id = explicitId || state.activeEndpoint || config.activeEndpoint || endpoints[0]?.id
    if (!id) throw new Error('no endpoints configured (set endpoints in the plugin config)')
    const ep = byEndpoint.get(id)
    if (!ep) throw new Error(`unknown endpoint "${id}"; configured: ${[...byEndpoint.keys()].join(', ')}`)
    return { id, ep }
  }

  const resolveNode = async (explicitId) => {
    const state = await loadState()
    const id = explicitId || await effectiveActiveNode() || state.activeNode || config.activeNode || nodes[0]?.id
    if (!id) return null
    const node = byNode.get(id)
    if (!node) throw new Error(`unknown node "${id}"; configured: ${[...byNode.keys()].join(', ')}`)
    return { id, node }
  }

  /** The node the relay currently routes root-path requests to: the single
   *  source of truth for "active". Falls back to the local state file only
   *  when the relay cannot be asked (unreachable / old relay). */
  const effectiveActiveNode = async () => {
    const state = await loadState()
    const candidates = [state.activeNode, config.activeNode, nodes[0]?.id].filter(Boolean)
    for (const cand of candidates) {
      const node = byNode.get(cand)
      if (!node) continue
      const token = await readTokenFile(node.accessTokenFile)
      const relayId = await relayDefaultNode(node, token)
      if (relayId && byNode.has(relayId)) return relayId
      if (relayId === '' && node.relayUrl) {
        // relay reachable but no default configured -> keep local order fallback
        return state.activeNode || config.activeNode || nodes[0]?.id || ''
      }
    }
    return state.activeNode || config.activeNode || nodes[0]?.id || ''
  }

  /** Full configuration view for the Settings section and the agent. */
  const configOp = async () => {
    const state = await loadState()
    const activeEndpoint = state.activeEndpoint || config.activeEndpoint || endpoints[0]?.id || ''
    const activeNode = await effectiveActiveNode()
    return {
      platform: PLATFORM,
      endpoints: endpoints.map((ep) => ({
        id: ep.id, label: ep.label, probeUrl: ep.probeUrl, caFile: ep.caFile || null,
        accessTokenFile: ep.accessTokenFile || null, active: ep.id === activeEndpoint,
      })),
      nodes: nodes.map((n) => nodeView(n, n.id === activeNode)),
      activeEndpoint,
      activeNode,
      services: resolveServices(config, byEndpoint.get(activeEndpoint)),
      stateFile: stateFilePath(),
      allowRestart: config.allowRestart,
      logTailLines: config.logTailLines,
    }
  }

  /** Shared status operation: settings page, agent tool, command and routes. */
  const statusOp = async (explicitEndpointId, explicitNodeId) => {
    const { id, ep } = await resolveEndpoint(explicitEndpointId)
    const services = resolveServices(config, ep)
    const snapshot = await collectStatus(ep, services, config.logTailLines)
    const frpc = await frpcProxies(ep.frpcConfig)
    const frpcGateway = await frpcProxies(ep.frpcGatewayConfig)
    const node = await resolveNode(explicitNodeId)
    const nodeInfo = node === null ? null : await nodeStatus(node.node, nodes.length > 1)
    return {
      endpointId: id,
      endpointLabel: ep.label,
      nodeId: node?.id ?? null,
      nodeLabel: node?.node.label ?? null,
      node: nodeInfo,
      verdict: verdict(snapshot),
      snapshot,
      frpc,
      frpcGateway,
      platform: PLATFORM,
    }
  }

  const restartOp = async (serviceId, explicitEndpointId) => {
    if (config.allowRestart !== true) {
      return { ok: false, reason: 'restart disabled: set allowRestart=true in the plugin config first' }
    }
    const { id, ep } = await resolveEndpoint(explicitEndpointId)
    const services = resolveServices(config, ep)
    const svc = services.find((s) => s.id === serviceId || s.name === serviceId)
    if (!svc) {
      return { ok: false, reason: `service "${serviceId}" is not managed for endpoint "${id}"; known: ${services.map((s) => s.id).join(', ')}` }
    }
    const stateAfter = await serviceRestart(svc)
    return { ok: true, service: svc.id, name: svc.name, endpointId: id, stateAfter }
  }

  // ── dsh-tunnel data-plane installer ops（管理面：只下载/安装/卸载，不自己跑隧道）──
  const ti = config.tunnelInstall ?? {}
  const installAllowed = () => ti.allowInstall === true

  /** Default relay base = active endpoint's probeUrl minus the probe path. */
  const resolveRelayBase = async () => {
    if (ti.relayBase) return String(ti.relayBase).replace(/\/+$/, '')
    const { ep } = await resolveEndpoint()
    return String(ep.probeUrl).replace(/\/__dsh_tunnel__\/probe.*$/, '').replace(/\/+$/, '')
  }

  /** Installed-state probe: binary present? version? unit state? Never guesses. */
  const tunnelInstallStatusOp = async () => {
    const binPath = join(installRoot(), binaryName())
    const info = { platform: PLATFORM, channel: `${channelPlatform()}/${process.arch}`, binPath, installed: false }
    try {
      const stat = await fsStat(binPath)
      info.installed = true
      info.size = stat.size
      info.mode = stat.mode
    } catch { /* absent is a valid answer */ }
    if (IS_WINDOWS) {
      info.kind = 'windows-service'
      try {
        const out = await run('sc.exe', ['query', 'dsh-tunnel'], CTL_TIMEOUT_MS)
        info.serviceState = parseWindowsServiceState(out)
      } catch { info.serviceState = 'absent' }
    } else {
      info.kind = 'systemd-user'
      try {
        const state = (await run('systemctl', ['--user', 'show', '-p', 'ActiveState', '--value', UNIT_NAME], CTL_TIMEOUT_MS)).trim()
        info.serviceState = state || 'absent'
        const unitText = await readFile(join(process.env.HOME || '', '.config/systemd/user', UNIT_NAME), 'utf8').catch(() => null)
        info.unitManaged = unitText !== null && unitText.includes('installed by @local/dsh-remote-access')
      } catch { info.serviceState = 'absent' }
    }
    return info
  }

  /** Download + verify + install the binary. No service changes here. */
  const tunnelInstallBinaryOp = async () => {
    if (!installAllowed()) return { ok: false, reason: 'install disabled: set tunnelInstall.allowInstall=true in the plugin config first' }
    const relayBase = await resolveRelayBase()
    const entry = await fetchChannelEntry(relayBase, { caFile: ti.caFile || undefined, accessTokenFile: ti.accessTokenFile || undefined })
    const result = await installBinary(entry, { caFile: ti.caFile || undefined })
    return { ok: true, relayBase, ...result, channel: `${channelPlatform()}/${process.arch}` }
  }

  /** Full install: binary + service registration (systemd / sc.exe script). */
  const tunnelInstallServiceOp = async (overrides = {}) => {
    if (!installAllowed()) return { ok: false, reason: 'install disabled: set tunnelInstall.allowInstall=true in the plugin config first' }
    const relayBase = await resolveRelayBase()
    const deviceId = overrides.deviceId || ti.deviceId || ''
    if (!deviceId) throw new Error('deviceId is required (tunnelInstall.deviceId or argument)')
    const relayUrl = overrides.relayUrl || `${relayBase}/__dsh_tunnel__/device`
    const localAddr = ti.localAddr || '127.0.0.1:3082'
    const binPath = join(installRoot(), binaryName())
    try { await fsStat(binPath) } catch {
      const dl = await tunnelInstallBinaryOp()
      if (!dl.ok) return dl
    }
    const extraArgs = String(ti.extraArgs || '').split(/\s+/).filter(Boolean)
    if (IS_WINDOWS) {
      const scriptPath = join(installRoot(), 'install-dsh-tunnel-service.bat')
      await mkdir(dirname(scriptPath), { recursive: true })
      await writeFile(scriptPath, renderWindowsInstallScript({ relayUrl, deviceId, binPath }), 'utf8')
      return { ok: true, kind: 'windows-service', scriptPath, note: 'run the generated script as Administrator (sc.exe needs elevation; the plugin never elevates)', relayUrl, deviceId }
    }
    const unit = renderUnit({ relayUrl, deviceId, pairingTokenFile: ti.pairingTokenFile || undefined, localAddr, extraArgs })
    const result = await installUnitLinux(unit, { relayUrl, deviceId })
    return { ok: true, kind: 'systemd-user', ...result }
  }

  const tunnelUninstallOp = async () => {
    if (!installAllowed()) return { ok: false, reason: 'install disabled: set tunnelInstall.allowInstall=true in the plugin config first' }
    if (IS_WINDOWS) return { ok: false, reason: 'on Windows run: sc stop dsh-tunnel && sc delete dsh-tunnel (Administrator); then remove ' + join(installRoot(), binaryName()) }
    const result = await uninstallUnitLinux()
    return { ok: true, ...result, binaryKept: join(installRoot(), binaryName()) }
  }

  const jsonResponse = (value, status = 200) => new Response(JSON.stringify(value), {
    status,
    headers: { 'content-type': 'application/json; charset=utf-8', 'cache-control': 'no-store' },
  })

  // 注：dsh-mobile-ui-adaptation skill 曾以 bundled provider 随插件分发，
  // 2026-11-10 起改为由本机技能库 ~/.agents/skills/ 提供（用户/项目级副本），
  // 插件不再内嵌 SKILL.md，也不再注册 skill provider。

  ctx.tools.register({
    name: 'remote_access_status',
    description: 'Observe the DSH remote-access data plane for one configured endpoint and controlled node: relay probe (reachability, auth, device online), local service states (systemd on Linux, Windows services on Windows), tunnel lifecycle log tail, and frpc proxy tables. Read-only. Omit endpointId/nodeId to use the ones selected in Settings.',
    parameters: {
      type: 'object',
      properties: {
        endpointId: { type: 'string', description: 'Optional relay endpoint id; defaults to the selected endpoint.' },
        nodeId: { type: 'string', description: 'Optional controlled node id; defaults to the selected node.' },
      },
      additionalProperties: false,
    },
    output: { schema: { type: 'string' }, render: (_args, value) => [{ type: 'text', text: value }] },
    async execute(args, exec) {
      exec.signal.throwIfAborted()
      const endpointId = typeof args.endpointId === 'string' && args.endpointId.length > 0 ? args.endpointId : undefined
      const nodeId = typeof args.nodeId === 'string' && args.nodeId.length > 0 ? args.nodeId : undefined
      return JSON.stringify(await statusOp(endpointId, nodeId))
    },
  })

  ctx.tools.register({
    name: 'remote_access_nodes',
    description: 'List every configured controlled node (machine) with its relay URL, device id, canonical control URL and observed online state. Read-only. Use before switching nodes with remote_access_switch.',
    parameters: { type: 'object', properties: {}, additionalProperties: false },
    output: { schema: { type: 'string' }, render: (_args, value) => [{ type: 'text', text: value }] },
    async execute(_args, exec) {
      exec.signal.throwIfAborted()
      const active = await effectiveActiveNode()
      const list = []
      for (const n of nodes) {
        const st = await nodeStatus(n, nodes.length > 1)
        list.push({ ...nodeView(n, n.id === active), online: st.online, source: st.source })
      }
      return JSON.stringify({ activeNode: active, nodes: list })
    },
  })

  ctx.tools.register({
    name: 'remote_access_switch',
    description: 'Select the active relay endpoint and/or controlled node. The selection persists across restarts in a state file; the lists themselves are authored in the plugin config. Switching changes what status/restart and the Settings section observe; it does not restart any service. A node URL is <relayUrl>/d/<deviceId>/.',
    parameters: {
      type: 'object',
      properties: {
        endpointId: { type: 'string', description: 'Optional endpoint id to make active.' },
        nodeId: { type: 'string', description: 'Optional controlled node id to make active.' },
      },
      additionalProperties: false,
    },
    output: { schema: { type: 'string' }, render: (_args, value) => [{ type: 'text', text: value }] },
    async execute(args, exec) {
      exec.signal.throwIfAborted()
      const patch = {}
      if (typeof args.endpointId === 'string' && args.endpointId.length > 0) patch.activeEndpoint = args.endpointId
      if (typeof args.nodeId === 'string' && args.nodeId.length > 0) patch.activeNode = args.nodeId
      if (Object.keys(patch).length === 0) throw new Error('endpointId or nodeId is required')
      const next = await applySwitch(patch)
      const node = next.activeNode ? byNode.get(next.activeNode) : null
      return JSON.stringify({ ok: true, ...next, ...(node ? { url: nodeURL(node) } : {}), stateFile: stateFilePath() })
    },
  })

  ctx.tools.register({
    name: 'remote_access_restart',
    description: 'Restart one managed local service for the selected endpoint: systemd unit on Linux, Windows service on Windows. Disabled unless the plugin config sets allowRestart=true. Run remote_access_status first and only restart on an explicit user request.',
    parameters: {
      type: 'object',
      properties: {
        serviceId: { type: 'string', description: 'Service descriptor id or exact name, as reported by remote_access_status.' },
        endpointId: { type: 'string', description: 'Optional endpoint id; defaults to the selected endpoint.' },
      },
      required: ['serviceId'],
      additionalProperties: false,
    },
    output: { schema: { type: 'string' }, render: (_args, value) => [{ type: 'text', text: value }] },
    async execute(args, exec) {
      exec.signal.throwIfAborted()
      if (typeof args.serviceId !== 'string' || args.serviceId.length === 0) throw new Error('serviceId is required')
      const endpointId = typeof args.endpointId === 'string' && args.endpointId.length > 0 ? args.endpointId : undefined
      return JSON.stringify(await restartOp(args.serviceId, endpointId))
    },
  })

  ctx.tools.register({
    name: 'remote_access_tunnel_install_status',
    description: 'Report the local dsh-tunnel data-plane installation state: binary present (path/size), service registration state (systemd user unit or Windows service), and whether the unit was written by this installer. Read-only; no network access.',
    parameters: { type: 'object', properties: {}, additionalProperties: false },
    output: { schema: { type: 'string' }, render: (_args, value) => [{ type: 'text', text: value }] },
    async execute(_args, exec) {
      exec.signal.throwIfAborted()
      return JSON.stringify(await tunnelInstallStatusOp())
    },
  })

  ctx.tools.register({
    name: 'remote_access_tunnel_install',
    description: 'Install or upgrade the dsh-tunnel data plane on this machine: download the platform-matched binary from the relay update channel (SHA-256 verified), write it under <DSH_HOME>/bin/, and register the service (systemd user unit on Linux; a generated sc.exe script on Windows). Requires tunnelInstall.allowInstall=true in the plugin config. Does NOT run tunnel logic in-process.',
    parameters: {
      type: 'object',
      properties: {
        deviceId: { type: 'string', description: 'Device id for this machine (required on first install unless tunnelInstall.deviceId is set).' },
        relayUrl: { type: 'string', description: 'Optional relay device endpoint override, e.g. wss://relay/__dsh_tunnel__/device; defaults to <relayBase>/__dsh_tunnel__/device.' },
      },
      additionalProperties: false,
    },
    output: { schema: { type: 'string' }, render: (_args, value) => [{ type: 'text', text: value }] },
    async execute(args, exec) {
      exec.signal.throwIfAborted()
      return JSON.stringify(await tunnelInstallServiceOp({
        deviceId: typeof args.deviceId === 'string' ? args.deviceId : undefined,
        relayUrl: typeof args.relayUrl === 'string' ? args.relayUrl : undefined,
      }))
    },
  })

  ctx.tools.register({
    name: 'remote_access_tunnel_uninstall',
    description: 'Stop and remove the installer-managed dsh-tunnel systemd user unit (Linux). Refuses to touch units this installer did not write. The binary is kept; delete it manually if wanted.',
    parameters: { type: 'object', properties: {}, additionalProperties: false },
    output: { schema: { type: 'string' }, render: (_args, value) => [{ type: 'text', text: value }] },
    async execute(_args, exec) {
      exec.signal.throwIfAborted()
      return JSON.stringify(await tunnelUninstallOp())
    },
  })

  // /remote-status: human-readable text, same operation core as the tool.
  ctx.commands.register({
    definitionId: CommandDefinitionId('@local/dsh-remote-access'),
    name: 'remote-status',
    description: 'Show remote-access status for the selected endpoint and node',
    handler: async (invocation) => {
      invocation.signal?.throwIfAborted?.()
      try {
        const result = await statusOp()
        const s = result.snapshot
        const lines = [
          `Remote access [${result.endpointLabel}]: ${result.verdict}`,
          `Platform: ${result.platform}`,
          result.node ? `Node: ${result.node.label} (${result.node.deviceId}) online=${result.node.online} url=${result.node.url}` : 'Node: (none configured)',
          '',
          `Relay probe (${s.relay.probeUrl}):`,
          s.relay.reachable
            ? `  reachable, HTTP ${s.relay.httpStatus}, authRequired=${s.relay.authRequired}, deviceOnline=${s.relay.deviceOnline}, deviceCount=${s.relay.deviceCount}`
            : `  UNREACHABLE: ${s.relay.error}`,
          '',
          'Services:',
          ...s.services.map((svc) => `  ${svc.id} (${svc.name}): ${svc.state}`),
        ]
        if (s.logs.lifecycle?.tail?.length) {
          lines.push('', 'Tunnel lifecycle tail:')
          for (const line of s.logs.lifecycle.tail.slice(-10)) lines.push(`  ${line}`)
        }
        return { kind: 'success', text: lines.join('\n') }
      } catch (error) {
        return { kind: 'error', text: `remote-status failed: ${error.message}` }
      }
    },
  })

  // ── QR pairing（设备凭据面）──
  // 出码/列表/撤销都打到「当前 endpoint」的 relay 管理路由；管理 token 只从
  // accessTokenFile 读、只走 header，绝不进 QR、绝不进 client 响应。
  // relay 未开 PhoneDevicesFile 时应答 501，这里原样透传让 UI 显示原因。
  const pairOp = async (action, endpointId, body = {}) => {
    const { id, ep } = await resolveEndpoint(endpointId)
    const token = await readTokenFile(ep.accessTokenFile)
    if (!token) return { error: `endpoint "${id}" has no accessTokenFile configured; pairing needs the management token` }
    // probeUrl 形如 https://host:port/__dsh_tunnel__/probe → 取基座。
    const base = String(ep.probeUrl || '').replace(/\/__dsh_tunnel__\/probe.*$/, '').replace(/\/+$/, '')
    if (!base) return { error: `endpoint "${id}" has no usable probeUrl` }
    const headers = { 'X-DSH-Relay-Token': token }
    const ca = ep.caFile || undefined
    const call = async (path, method, payload) => {
      const res = method === 'POST'
        ? await postJson(`${base}${path}`, CTL_TIMEOUT_MS, ca, headers, payload || {})
        : await requestUrl(`${base}${path}`, CTL_TIMEOUT_MS, ca, headers)
      let data = null
      try { data = JSON.parse(res.body) } catch { /* non-JSON error body */ }
      return { status: res.status, data, raw: res.body }
    }
    switch (action) {
      case 'mint': {
        const res = await call('/__dsh_tunnel__/pair/mint', 'POST')
        if (res.status === 501) return { supported: false, error: 'relay 未启用设备配对（PhoneDevicesFile 未配置）' }
        if (res.status !== 200 || !res.data?.code) return { error: `relay mint HTTP ${res.status}: ${String(res.raw).slice(0, 120)}` }
        const payload = pairingPayload({ relay: base, ca: caKindOf(base), code: res.data.code })
        return {
          supported: true,
          endpointId: id,
          svg: pairingSvg(payload),
          // payload 给「复制链接」用：一次性码本身不是长效凭据，可交给用户。
          payload: JSON.stringify(payload),
          expiresAt: res.data.expiresAt,
        }
      }
      case 'status': {
        const res = await call('/__dsh_tunnel__/pair/status', 'GET')
        if (res.status === 501) return { supported: false }
        if (res.status !== 200) return { error: `relay status HTTP ${res.status}` }
        return { supported: true, active: res.data?.active === true, expiresAt: res.data?.expiresAt ?? null }
      }
      case 'devices': {
        const res = await call('/__dsh_tunnel__/pair/devices', 'GET')
        if (res.status === 501) return { supported: false }
        if (res.status !== 200) return { error: `relay devices HTTP ${res.status}` }
        return { supported: true, devices: Array.isArray(res.data?.devices) ? res.data.devices : [] }
      }
      case 'revoke': {
        const deviceId = typeof body?.deviceId === 'string' ? body.deviceId.trim() : ''
        if (!deviceId) return { error: 'deviceId is required' }
        const res = await call('/__dsh_tunnel__/pair/revoke', 'POST', { deviceId })
        if (res.status !== 200) return { error: `relay revoke HTTP ${res.status}: ${String(res.raw).slice(0, 120)}` }
        return { revoked: true, warning: res.data?.warning ?? null }
      }
      default:
        return { error: `unknown pair action "${action}"` }
    }
  }

  // Same-origin Fetch routes for the Settings section. Registered only when
  // the Connection service exists; the carrier enforces Host/Origin trust and
  // browser authentication before these handlers run.
  ctx.inject(['connection'], (scope) => {
    scope.effect(() => scope.connection.fetch.register({
      path: '/api/remote-access/status',
      methods: ['GET'],
      requestBody: 'buffered',
      fetch: async (request) => {
        try {
          const url = new URL(request.url)
          const endpointId = url.searchParams.get('endpointId') || undefined
          const nodeId = url.searchParams.get('nodeId') || undefined
          return jsonResponse(await statusOp(endpointId, nodeId))
        } catch (error) {
          return jsonResponse({ error: error.message }, 502)
        }
      },
    }), 'remote-access: /api/remote-access/status')

    scope.effect(() => scope.connection.fetch.register({
      path: '/api/remote-access/tunnel-install',
      methods: ['GET', 'POST'],
      requestBody: 'buffered',
      fetch: async (request) => {
        try {
          if (request.method === 'GET') return jsonResponse(await tunnelInstallStatusOp())
          const body = await request.json().catch(() => ({}))
          if (body?.action === 'uninstall') return jsonResponse(await tunnelUninstallOp())
          return jsonResponse(await tunnelInstallServiceOp({
            deviceId: typeof body?.deviceId === 'string' ? body.deviceId : undefined,
            relayUrl: typeof body?.relayUrl === 'string' ? body.relayUrl : undefined,
          }))
        } catch (error) {
          return jsonResponse({ error: error.message }, 400)
        }
      },
    }), 'remote-access: /api/remote-access/tunnel-install')


    scope.effect(() => scope.connection.fetch.register({
      path: '/api/remote-access/pair',
      methods: ['GET', 'POST'],
      requestBody: 'buffered',
      fetch: async (request) => {
        try {
          const url = new URL(request.url)
          const body = request.method === 'POST' ? await request.json().catch(() => ({})) : {}
          const endpointId = url.searchParams.get('endpointId') || body?.endpointId || undefined
          const action = request.method === 'GET' ? (url.searchParams.get('action') || 'status') : (body?.action || '')
          return jsonResponse(await pairOp(action, endpointId, body))
        } catch (error) {
          return jsonResponse({ error: error.message }, 502)
        }
      },
    }), 'remote-access: /api/remote-access/pair')

    scope.effect(() => scope.connection.fetch.register({
      path: '/api/remote-access/config',
      methods: ['GET', 'POST'],
      requestBody: 'buffered',
      fetch: async (request) => {
        try {
          if (request.method === 'GET') return jsonResponse(await configOp())
          const body = await request.json()
          const patch = {}
          if (typeof body?.endpointId === 'string' && body.endpointId.length > 0) patch.activeEndpoint = body.endpointId
          if (typeof body?.nodeId === 'string' && body.nodeId.length > 0) patch.activeNode = body.nodeId
          if (Object.keys(patch).length === 0) return jsonResponse({ error: 'endpointId or nodeId is required' }, 400)
          const next = await applySwitch(patch)
          return jsonResponse({ ok: true, ...next, ...await configOp() })
        } catch (error) {
          return jsonResponse({ error: error.message }, 400)
        }
      },
    }), 'remote-access: /api/remote-access/config')
  })

  return () => {}
}

/** Test-only helpers; harmless for the runtime. */
export const internals = {
  parseWindowsServiceState, nodeURL, parseProbe, resolveServices, stateFilePath,
  setRelayDefaultNode,
  installRoot, channelPlatform, binaryName, fetchChannelEntry, installBinary,
  renderUnit, renderWindowsInstallScript, installUnitLinux, uninstallUnitLinux,
}
