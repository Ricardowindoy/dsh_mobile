import test from 'node:test'
import assert from 'node:assert/strict'
import http from 'node:http'
import os from 'node:os'
import path from 'node:path'
import fs from 'node:fs/promises'

import { internals, Config, apply } from '../index.js'
import { pairingPayload, pairingSvg, caKindOf } from '../pairing.mjs'

// ── pairing.mjs 纯函数 ──────────────────────────────────────────────

test('pairingPayload validates shape and never accepts a short code', () => {
  const p = pairingPayload({ relay: 'https://relay.example.com:24000/', ca: 'ip', code: 'c'.repeat(32) })
  assert.equal(p.v, 1)
  assert.equal(p.relay, 'https://relay.example.com:24000') // trailing slash stripped
  assert.equal(p.ca, 'ip')
  assert.throws(() => pairingPayload({ relay: 'not a url', code: 'c'.repeat(32) }), /valid URL/)
  assert.throws(() => pairingPayload({ relay: 'ftp://x', code: 'c'.repeat(32) }), /http/)
  assert.throws(() => pairingPayload({ relay: 'https://x', code: 'short' }), /too short/)
  // Unknown ca kinds normalize to none, never crash the QR.
  assert.equal(pairingPayload({ relay: 'https://x', ca: 'weird', code: 'c'.repeat(32) }).ca, 'none')
})

test('caKindOf distinguishes IP literal from hostname entrances', () => {
  assert.equal(caKindOf('https://192.0.2.10:24000'), 'ip') // RFC5737 doc IP
  assert.equal(caKindOf('https://dsh.relay.example.com'), 'domain')
  assert.equal(caKindOf('garbage'), 'none')
})

test('pairingSvg renders a standalone SVG carrying the payload', () => {
  const payload = pairingPayload({ relay: 'https://relay.example.com', ca: 'domain', code: 'x'.repeat(32) })
  const svg = pairingSvg(payload)
  assert.match(svg, /^<svg/)
  assert.match(svg, /xmlns="http:\/\/www.w3.org\/2000\/svg"/)
  assert.ok(svg.length > 1000, 'QR svg suspiciously small')
  // The QR must not embed anything beyond the JSON payload (no tokens by
  // construction: payload has only v/relay/ca/code).
  assert.deepEqual(Object.keys(JSON.parse(JSON.stringify(payload))).sort(), ['ca', 'code', 'relay', 'v'])
})

// ── pairOp via mock relay + mock ctx ────────────────────────────────

function mockCtx() {
  const tools = []
  const routes = []
  const scope = {
    connection: { fetch: { register: (route) => { routes.push(route); return () => {} } } },
    effect: (fn) => { fn(); return () => {} },
  }
  const ctx = {
    tools: { register: (t) => tools.push(t) },
    commands: { register: () => {} },
    inject: (deps, cb) => { if (deps.includes('connection')) cb(scope) },
  }
  return { ctx, tools, routes }
}

/** A fake relay implementing the pair management surface. Records the
 *  credential carrier of every request so tests can assert header-only. */
async function fakeRelay() {
  const seen = []
  let minted = null
  const devices = [{ deviceId: 'ph_1', name: 'Pixel', createdAt: 1, lastSeenAt: 2 }]
  const server = http.createServer((req, res) => {
    seen.push({ path: req.url, token: req.headers['x-dsh-relay-token'] || null, query: req.url.includes('token=') })
    res.setHeader('content-type', 'application/json')
    if (req.headers['x-dsh-relay-token'] !== 'admin-sekrit') { res.statusCode = 401; res.end('{}'); return }
    if (req.url === '/__dsh_tunnel__/pair/mint') {
      minted = { code: 'c'.repeat(32), expiresAt: Date.now() + 120000 }
      res.end(JSON.stringify(minted)); return
    }
    if (req.url === '/__dsh_tunnel__/pair/status') { res.end(JSON.stringify({ active: minted !== null, expiresAt: minted?.expiresAt ?? null })); return }
    if (req.url === '/__dsh_tunnel__/pair/devices') { res.end(JSON.stringify({ devices })); return }
    if (req.url === '/__dsh_tunnel__/pair/revoke') {
      let body = ''
      req.on('data', (c) => { body += c })
      req.on('end', () => {
        const id = JSON.parse(body).deviceId
        const i = devices.findIndex((d) => d.deviceId === id)
        if (i < 0) { res.statusCode = 404; res.end('{}'); return }
        devices.splice(i, 1)
        res.end(JSON.stringify({ revoked: true }))
      })
      return
    }
    res.statusCode = 404
    res.end('{}')
  })
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  return { server, port: server.address().port, seen, devices }
}

test('pair route: mint returns SVG + payload; management token stays header-only and out of the client body', async () => {
  const home = await fs.mkdtemp(path.join(os.tmpdir(), 'dsh-pair-'))
  process.env.DSH_HOME = home
  const relay = await fakeRelay()
  const tokenFile = path.join(home, 'relay-token')
  await fs.writeFile(tokenFile, 'admin-sekrit')
  try {
    const cfg = Config({
      endpoints: [{ id: 'ip', label: 'IP', probeUrl: `http://127.0.0.1:${relay.port}/__dsh_tunnel__/probe`, accessTokenFile: tokenFile }],
      nodes: [],
    })
    const { ctx, routes } = mockCtx()
    apply(ctx, cfg)
    const route = routes.find((r) => r.path === '/api/remote-access/pair')
    assert.ok(route, 'pair route registered')

    const call = (body) => route.fetch(new Request('http://x/api/remote-access/pair', {
      method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body),
    })).then((r) => r.json())

    const mint = await call({ action: 'mint' })
    assert.equal(mint.supported, true)
    assert.match(mint.svg, /^<svg/)
    const payload = JSON.parse(mint.payload)
    assert.equal(payload.v, 1)
    assert.equal(payload.code.length, 32)
    assert.equal(payload.ca, 'ip') // 127.0.0.1 literal
    // The management token must not appear anywhere in the client response.
    assert.ok(!JSON.stringify(mint).includes('admin-sekrit'), 'management token leaked to client')
    // And every relay call carried it as a HEADER, never a query param.
    assert.ok(relay.seen.length > 0)
    for (const s of relay.seen) {
      assert.equal(s.token, 'admin-sekrit', `header token missing on ${s.path}`)
      assert.equal(s.query, false, `query token used on ${s.path}`)
    }

    const devices = await call({ action: 'devices' })
    assert.equal(devices.devices.length, 1)
    assert.ok(!JSON.stringify(devices).includes('tokenHash'), 'hash leaked')

    const revoked = await call({ action: 'revoke', deviceId: 'ph_1' })
    assert.equal(revoked.revoked, true)
    const after = await call({ action: 'devices' })
    assert.equal(after.devices.length, 0)

    const bad = await call({ action: 'revoke' })
    assert.match(bad.error, /deviceId/)
    const unknown = await call({ action: 'nope' })
    assert.match(unknown.error, /unknown pair action/)
  } finally {
    await new Promise((resolve) => relay.server.close(resolve))
    await fs.rm(home, { recursive: true, force: true })
    delete process.env.DSH_HOME
  }
})

test('pair route: relay without the device surface reports supported=false, not a crash', async () => {
  const home = await fs.mkdtemp(path.join(os.tmpdir(), 'dsh-pair-'))
  process.env.DSH_HOME = home
  const server = http.createServer((req, res) => { res.statusCode = 501; res.end('pairing not enabled') })
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  const tokenFile = path.join(home, 'relay-token')
  await fs.writeFile(tokenFile, 'admin-sekrit')
  try {
    const cfg = Config({
      endpoints: [{ id: 'ip', label: 'IP', probeUrl: `http://127.0.0.1:${server.address().port}/__dsh_tunnel__/probe`, accessTokenFile: tokenFile }],
      nodes: [],
    })
    const { ctx, routes } = mockCtx()
    apply(ctx, cfg)
    const route = routes.find((r) => r.path === '/api/remote-access/pair')
    const body = await route.fetch(new Request('http://x/api/remote-access/pair', {
      method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ action: 'mint' }),
    })).then((r) => r.json())
    assert.equal(body.supported, false)
    assert.ok(body.error)
  } finally {
    await new Promise((resolve) => server.close(resolve))
    await fs.rm(home, { recursive: true, force: true })
    delete process.env.DSH_HOME
  }
})

test('pair route: endpoint without accessTokenFile refuses to mint (no anonymous management)', async () => {
  const home = await fs.mkdtemp(path.join(os.tmpdir(), 'dsh-pair-'))
  process.env.DSH_HOME = home
  try {
    const cfg = Config({
      endpoints: [{ id: 'ip', label: 'IP', probeUrl: 'http://127.0.0.1:1/__dsh_tunnel__/probe' }],
      nodes: [],
    })
    const { ctx, routes } = mockCtx()
    apply(ctx, cfg)
    const route = routes.find((r) => r.path === '/api/remote-access/pair')
    const body = await route.fetch(new Request('http://x/api/remote-access/pair', {
      method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ action: 'mint' }),
    })).then((r) => r.json())
    assert.match(body.error, /accessTokenFile/)
  } finally {
    await fs.rm(home, { recursive: true, force: true })
    delete process.env.DSH_HOME
  }
})