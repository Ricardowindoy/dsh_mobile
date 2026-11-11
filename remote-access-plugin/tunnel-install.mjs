/**
 * dsh-tunnel data-plane installer for the remote-access management plugin.
 *
 * Downloads the matching dsh-tunnel binary from the relay's update channel
 * (platform `tunnel-<os>`), verifies SHA-256, atomically installs it under
 * <DSH_HOME>/bin/, and (Linux) generates + enables a systemd user unit or
 * (Windows) writes an install script for sc.exe. The tunnel remains an
 * independent process — this module never runs tunnel logic in-process.
 */
import { execFile } from 'node:child_process'
import { createHash } from 'node:crypto'
import { createWriteStream, readFileSync } from 'node:fs'
import fs from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { pipeline } from 'node:stream/promises'
import { get as httpsGet } from 'node:https'
import { get as httpGet } from 'node:http'

const DOWNLOAD_TIMEOUT_MS = 120000
const CTL_TIMEOUT_MS = 15000

/** Non-streaming GET returning text; honours a pinned private CA (Node fetch
 *  does not read SSL_CERT_FILE, so https requests here pass ca explicitly). */
function requestUrl(url, timeoutMs, caFile, headers) {
  return new Promise((resolve, reject) => {
    const isHttps = url.startsWith('https:')
    const getter = isHttps ? httpsGet : httpGet
    const options = { timeout: timeoutMs, headers: headers || {} }
    if (isHttps && caFile) options.ca = readFileSync(caFile)
    const req = getter(url, options, (res) => {
      let body = ''
      res.setEncoding('utf8')
      res.on('data', (c) => { body += c })
      res.on('end', () => resolve({ status: res.statusCode ?? 0, body }))
    })
    req.on('timeout', () => req.destroy(new Error(`timeout after ${timeoutMs}ms`)))
    req.on('error', (e) => reject(e instanceof Error ? e : new Error(String(e))))
  })
}

export const TUNNEL_PLATFORM_PREFIX = 'tunnel-'
export const BIN_DIRNAME = 'bin'
export const UNIT_NAME = 'dsh-tunnel.service'

/** Install root: DSH_HOME wins; Windows falls back to %APPDATA%\dsh. */
export function installRoot(env = process.env, platform = process.platform) {
  const base = env.DSH_HOME
    || (platform === 'win32' ? join(env.APPDATA || '', 'dsh') : join(env.HOME || '', '.dsh'))
  return join(base, BIN_DIRNAME)
}

/** Update-channel platform key for this machine, e.g. tunnel-linux. */
export function channelPlatform(platform = process.platform) {
  switch (platform) {
    case 'linux': return `${TUNNEL_PLATFORM_PREFIX}linux`
    case 'darwin': return `${TUNNEL_PLATFORM_PREFIX}darwin`
    case 'win32': return `${TUNNEL_PLATFORM_PREFIX}windows`
    default: throw new Error(`unsupported platform for tunnel install: ${platform}`)
  }
}

/** Binary file name on disk for this platform. */
export function binaryName(platform = process.platform) {
  return platform === 'win32' ? 'dsh-tunnel.exe' : 'dsh-tunnel'
}

function run(file, args, timeoutMs = CTL_TIMEOUT_MS) {
  return new Promise((resolve, reject) => {
    execFile(file, args, { timeout: timeoutMs, encoding: 'utf8', windowsHide: true }, (error, stdout, stderr) => {
      if (error) reject(new Error(`${file} ${args.join(' ')} failed: ${(stderr || error.message || '').trim().slice(0, 300)}`))
      else resolve(stdout)
    })
  })
}

/** Stream a download to a file while hashing; size is advisory. */
async function downloadTo(url, dest, { caFile, headers, expectedSize }) {
  const { requestUrlStream } = await import('./http-stream.mjs')
  const res = await requestUrlStream(url, DOWNLOAD_TIMEOUT_MS, caFile, headers)
  if (res.status !== 200) throw new Error(`download failed: HTTP ${res.status}`)
  const hash = createHash('sha256')
  let size = 0
  const target = createWriteStream(dest, { mode: 0o755 })
  // res.body is an IncomingMessage (Node) — iterate it directly and hash each
  // chunk; Readable.fromWeb is only for Web ReadableStreams, not Node streams.
  await pipeline(async function* () {
    for await (const chunk of res.body) {
      hash.update(chunk)
      size += chunk.length
      yield chunk
    }
  }(), target)
  if (Number.isFinite(expectedSize) && expectedSize > 0 && size !== expectedSize) {
    throw new Error(`size mismatch: expected ${expectedSize}, got ${size}`)
  }
  return { size, sha256: hash.digest('hex') }
}

/** Resolve the update-channel entry for this platform from a relay base URL.
 *  Uses node:https with an optional pinned CA — Node's global fetch does not
 *  honour SSL_CERT_FILE, so a private-CA relay fails with
 *  UNABLE_TO_VERIFY_LEAF_SIGNATURE there (same fix as probeRelay). */
export async function fetchChannelEntry(relayBase, { caFile, accessTokenFile } = {}) {
  const base = String(relayBase).replace(/\/+$/, '')
  const checkUrl = `${base}/__dsh_tunnel__/update/check?platform=${channelPlatform()}&arch=${process.arch}&appVersion=0.0.0`
  const headers = {}
  if (accessTokenFile) {
    try { headers['X-DSH-Relay-Token'] = (await fs.readFile(accessTokenFile, 'utf8')).trim() } catch { /* probe continues without it */ }
  }
  const res = await requestUrl(checkUrl, DOWNLOAD_TIMEOUT_MS, caFile || undefined, headers)
  if (res.status !== 200) throw new Error(`update check failed: HTTP ${res.status}`)
  let body
  try { body = JSON.parse(res.body) } catch { throw new Error('update check returned non-JSON') }
  if (body?.code === 204 || body?.msg === 'already up to date') throw new Error(`no tunnel release for ${channelPlatform()}/${process.arch} at this relay (channel manifest has no matching entry)`)
  const item = body?.code === 200 ? body : body?.item ?? body
  if (!item || !item.sha256 || !item.size) throw new Error(`no tunnel release for ${channelPlatform()}/${process.arch} at this relay`)
  return {
    version: String(item.version ?? ''),
    filename: String(item.filename ?? ''),
    url: `${base}/__dsh_tunnel__/update/download?platform=${channelPlatform()}&arch=${process.arch}`,
    size: Number(item.size ?? 0),
    sha256: String(item.sha256),
    releaseNote: String(item.releaseNote ?? ''),
  }
}

/** Install one version atomically: download to tmp, verify, rename into place. */
export async function installBinary(entry, { caFile, headers } = {}) {
  const root = installRoot()
  await fs.mkdir(root, { recursive: true })
  const staged = join(root, `.staging-${Date.now()}-${binaryName()}`)
  try {
    const got = await downloadTo(entry.url, staged, { caFile, headers, expectedSize: entry.size })
    if (got.sha256 !== String(entry.sha256).toLowerCase()) {
      throw new Error(`sha256 mismatch: expected ${entry.sha256}, got ${got.sha256}`)
    }
    const dest = join(root, binaryName())
    await fs.rm(dest, { force: true })
    await fs.rename(staged, dest)
    if (process.platform !== 'win32') await fs.chmod(dest, 0o755)
    return { path: dest, size: got.size, sha256: got.sha256, version: entry.version }
  } finally {
    await fs.rm(staged, { force: true }).catch(() => {})
  }
}

/** Render the systemd user unit for an installed tunnel. */
export function renderUnit({ relayUrl, deviceId, pairingTokenFile, localAddr, logDir, extraArgs = [] }) {
  if (!relayUrl) throw new Error('relayUrl is required')
  if (!deviceId) throw new Error('deviceId is required')
  const lines = [
    '[Unit]',
    'Description=dsh tunnel to public relay (installed by @local/dsh-remote-access)',
    'After=network-online.target',
    '',
    '[Service]',
    'Type=simple',
  ]
  if (pairingTokenFile) lines.push(`Environment=DSH_TUNNEL_PAIRING_TOKEN_FILE=${pairingTokenFile}`)
  lines.push(
    `ExecStart=${join(installRoot(), binaryName())} --relay ${relayUrl} --device-id ${deviceId} --local ${localAddr} ${extraArgs.join(' ')}`.trim(),
    'Restart=always',
    'RestartSec=3',
    '',
    '[Install]',
    'WantedBy=default.target',
    '',
  )
  return lines.join('\n')
}

/** systemd user unit dir, resolved per call so tests can retarget HOME. */
const unitDir = () => join(process.env.HOME || '', '.config/systemd/user')

/** Linux: write the unit, daemon-reload, enable --now. Refuses when a unit
 *  already exists that this installer did not write (never clobber manual work). */
export async function installUnitLinux(unit, { relayUrl, deviceId } = {}) {
  const dir = unitDir()
  const unitPath = join(dir, UNIT_NAME)
  let existing = null
  try { existing = await fs.readFile(unitPath, 'utf8') } catch { /* absent */ }
  if (existing !== null && !existing.includes('installed by @local/dsh-remote-access')) {
    throw new Error(`${unitPath} already exists and was not installed by this plugin; refusing to overwrite (manage it manually or remove it first)`)
  }
  await fs.mkdir(dir, { recursive: true })
  await fs.writeFile(unitPath, unit, 'utf8')
  await run('systemctl', ['--user', 'daemon-reload'])
  await run('systemctl', ['--user', 'enable', '--now', UNIT_NAME])
  const state = (await run('systemctl', ['--user', 'show', '-p', 'ActiveState', '--value', UNIT_NAME])).trim()
  return { unitPath, enabled: true, state, relayUrl, deviceId }
}

/** Windows: write a bat that installs the service via sc.exe (admin consent
 *  stays with the user; the plugin never elevates). */
export function renderWindowsInstallScript({ relayUrl, deviceId, binPath }) {
  const exe = binPath ?? join(installRoot(), binaryName())
  return [
    '@echo off',
    'rem dsh-tunnel service installer (generated by @local/dsh-remote-access)',
    'rem Run this script as Administrator once; sc.exe needs elevation.',
    `sc create dsh-tunnel binPath= "${exe} --relay ${relayUrl} --device-id ${deviceId}" start= auto DisplayName= "dsh tunnel"`,
    'sc description dsh-tunnel "dsh remote-access data plane (installed by @local/dsh-remote-access)"',
    'sc start dsh-tunnel',
    'sc query dsh-tunnel',
    'pause',
    '',
  ].join('\r\n')
}

export async function uninstallUnitLinux() {
  await run('systemctl', ['--user', 'disable', '--now', UNIT_NAME]).catch(() => {})
  await fs.rm(join(unitDir(), UNIT_NAME), { force: true })
  await run('systemctl', ['--user', 'daemon-reload']).catch(() => {})
  return { removed: UNIT_NAME }
}
