import test from 'node:test'
import assert from 'node:assert/strict'
import http from 'node:http'
import os from 'node:os'
import path from 'node:path'
import fs from 'node:fs/promises'

import { internals, Config, apply } from '../index.js'

const { parseWindowsServiceState, nodeURL, parseProbe, resolveServices, stateFilePath } = internals

test('nodeURL builds relay-scoped control URL and encodes the device id', () => {
  assert.equal(nodeURL({ relayUrl: 'https://relay.example.com/', deviceId: 'home-dsh' }), 'https://relay.example.com/d/home-dsh/')
  assert.equal(nodeURL({ relayUrl: 'https://relay.example.com', deviceId: 'a b&c' }), 'https://relay.example.com/d/a%20b%26c/')
})

test('parseWindowsServiceState uses numeric STATE codes (locale-proof)', () => {
  assert.equal(parseWindowsServiceState('STATE              : 4  RUNNING'), 'running')
  assert.equal(parseWindowsServiceState('STATE              : 1  STOPPED'), 'stopped')
  assert.equal(parseWindowsServiceState('STATE              : 2  START_PENDING'), 'start_pending')
  assert.equal(parseWindowsServiceState('STATE              : 3  STOP_PENDING'), 'stop_pending')
  assert.equal(parseWindowsServiceState('garbage'), 'unknown')
})

test('parseProbe exposes devices only when the relay returns them', () => {
  const withDevices = parseProbe(200, JSON.stringify({
    ok: true, authRequired: true, deviceOnline: true, deviceCount: 2,
    devices: [{ id: 'a', online: true }, { id: 'b', online: false }, { bad: 1 }],
  }))
  assert.deepEqual(withDevices.devices, [{ id: 'a', online: true }, { id: 'b', online: false }])
  const aggregateOnly = parseProbe(200, JSON.stringify({ ok: true, deviceOnline: true, deviceCount: 1 }))
  assert.equal(aggregateOnly.devices, null)
})

test('resolveServices: explicit config wins, Linux derives units, Windows uses services', () => {
  const explicit = [{ id: 'x', label: 'x', kind: 'windows-service', name: 'X' }]
  assert.deepEqual(resolveServices({ services: explicit }, {}, 'linux'), explicit)

  const linux = resolveServices({ services: [] }, { tunnelUnit: 'dsh-tunnel.service', frpcUnit: 'frpc.service', frpcGatewayUnit: 'gw.service' }, 'linux')
  assert.deepEqual(linux.map((s) => [s.id, s.kind, s.name]), [
    ['tunnel', 'systemd-user', 'dsh-tunnel.service'],
    ['frpc', 'systemd-user', 'frpc.service'],
    ['frpc-gateway', 'systemd-user', 'gw.service'],
  ])

  const win = resolveServices({ services: [] }, { tunnelUnit: 'dsh-tunnel.service' }, 'win32')
  assert.ok(win.every((s) => s.kind === 'windows-service'))
  assert.deepEqual(win.map((s) => s.name), ['dsh-tunnel', 'dsh-forwarder-client'])
})

test('stateFilePath honours DSH_HOME', () => {
  const prev = process.env.DSH_HOME
  process.env.DSH_HOME = '/tmp/dsh-test-home'
  try {
    assert.equal(stateFilePath(), '/tmp/dsh-test-home/remote-access-state.json')
  } finally {
    if (prev === undefined) delete process.env.DSH_HOME
    else process.env.DSH_HOME = prev
  }
})

/** Build a mock Cordis ctx, returning the registered tools/routes. */
function mockCtx() {
  const tools = []
  const commands = []
  const routes = []
  const scope = {
    connection: { fetch: { register: (route) => { routes.push(route); return () => {} } } },
    effect: (fn) => { fn(); return () => {} },
  }
  const ctx = {
    tools: { register: (t) => tools.push(t) },
    commands: { register: (c) => commands.push(c) },
    inject: (deps, cb) => { if (deps.includes('connection')) cb(scope) },
  }
  return { ctx, tools, commands, routes }
}

test('switch tool persists endpoint and node selection; invalid ids are rejected', async () => {
  const home = await fs.mkdtemp(path.join(os.tmpdir(), 'dsh-ra-'))
  process.env.DSH_HOME = home
  try {
    const cfg = Config({
      endpoints: [
        { id: 'ip', label: 'IP', probeUrl: 'http://127.0.0.1:1/probe' },
        { id: 'domain', label: 'Domain', probeUrl: 'http://127.0.0.1:1/probe' },
      ],
      nodes: [
        { id: 'a', label: 'Node A', deviceId: 'dev-a', relayUrl: 'https://relay.example.com' },
        { id: 'b', label: 'Node B', deviceId: 'dev-b', relayUrl: 'https://relay.example.com' },
      ],
    })
    const { ctx, tools, routes } = mockCtx()
    apply(ctx, cfg)
    const switchTool = tools.find((t) => t.name === 'remote_access_switch')
    const nodesTool = tools.find((t) => t.name === 'remote_access_nodes')

    const out = JSON.parse(await switchTool.execute({ endpointId: 'domain', nodeId: 'b' }, { signal: new AbortController().signal }))
    assert.equal(out.ok, true)
    assert.equal(out.activeNode, 'b')
    assert.equal(out.url, 'https://relay.example.com/d/dev-b/')

    const saved = JSON.parse(await fs.readFile(path.join(home, 'remote-access-state.json'), 'utf8'))
    assert.deepEqual(saved, { activeEndpoint: 'domain', activeNode: 'b' })

    // Invalid ids must fail loudly, not silently switch.
    await assert.rejects(() => switchTool.execute({ nodeId: 'ghost' }, { signal: new AbortController().signal }), /unknown node/)
    await assert.rejects(() => switchTool.execute({ endpointId: 'ghost' }, { signal: new AbortController().signal }), /unknown endpoint/)
    await assert.rejects(() => switchTool.execute({}, { signal: new AbortController().signal }), /endpointId or nodeId is required/)

    // Config route reflects the persisted selection.
    const cfgRoute = routes.find((r) => r.path === '/api/remote-access/config')
    const cfgBody = await (await cfgRoute.fetch(new Request('http://x/api/remote-access/config'))).json()
    assert.equal(cfgBody.activeNode, 'b')
    assert.equal(cfgBody.nodes.find((n) => n.id === 'b').active, true)
    assert.equal(cfgBody.nodes.find((n) => n.id === 'a').active, false)

    // nodes tool lists both nodes with their canonical URLs.
    const nodesOut = JSON.parse(await nodesTool.execute({}, { signal: new AbortController().signal }))
    assert.equal(nodesOut.activeNode, 'b')
    assert.equal(nodesOut.nodes.length, 2)
    assert.equal(nodesOut.nodes[1].url, 'https://relay.example.com/d/dev-b/')
  } finally {
    await fs.rm(home, { recursive: true, force: true })
    delete process.env.DSH_HOME
  }
})

test('node status is derived from the relay device list (online/offline/unknown)', async () => {
  const server = http.createServer((req, res) => {
    res.setHeader('content-type', 'application/json')
    res.end(JSON.stringify({
      ok: true, authRequired: true, deviceOnline: true, deviceCount: 2,
      devices: [{ id: 'dev-a', online: true }, { id: 'dev-b', online: false }],
    }))
  })
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  const port = server.address().port
  const home = await fs.mkdtemp(path.join(os.tmpdir(), 'dsh-ra-'))
  process.env.DSH_HOME = home
  try {
    const cfg = Config({
      endpoints: [{ id: 'local', label: 'Local', probeUrl: `http://127.0.0.1:${port}/__dsh_tunnel__/probe`, logDir: '' }],
      nodes: [
        { id: 'a', label: 'A', deviceId: 'dev-a', relayUrl: `http://127.0.0.1:${port}` },
        { id: 'b', label: 'B', deviceId: 'dev-b', relayUrl: `http://127.0.0.1:${port}` },
        { id: 'c', label: 'C', deviceId: 'dev-c', relayUrl: `http://127.0.0.1:${port}` },
      ],
    })
    const { ctx, tools } = mockCtx()
    apply(ctx, cfg)
    const statusTool = tools.find((t) => t.name === 'remote_access_status')
    const sig = { signal: new AbortController().signal }

    const a = JSON.parse(await statusTool.execute({ nodeId: 'a' }, sig))
    assert.equal(a.node.online, true)
    assert.equal(a.node.source, 'probe-devices')
    const b = JSON.parse(await statusTool.execute({ nodeId: 'b' }, sig))
    assert.equal(b.node.online, false)
    const c = JSON.parse(await statusTool.execute({ nodeId: 'c' }, sig))
    assert.equal(c.node.online, 'unknown') // device not present in the relay list
  } finally {
    await new Promise((resolve) => server.close(resolve))
    await fs.rm(home, { recursive: true, force: true })
    delete process.env.DSH_HOME
  }
})

test('restart stays gated and only addresses configured services', async () => {
  const home = await fs.mkdtemp(path.join(os.tmpdir(), 'dsh-ra-'))
  process.env.DSH_HOME = home
  try {
    const cfg = Config({
      allowRestart: false,
      endpoints: [{ id: 'local', label: 'Local', probeUrl: 'http://127.0.0.1:1/probe' }],
      nodes: [],
    })
    const { ctx, tools } = mockCtx()
    apply(ctx, cfg)
    const restart = tools.find((t) => t.name === 'remote_access_restart')
    const out = JSON.parse(await restart.execute({ serviceId: 'tunnel' }, { signal: new AbortController().signal }))
    assert.equal(out.ok, false)
    assert.match(out.reason, /allowRestart/)
  } finally {
    await fs.rm(home, { recursive: true, force: true })
    delete process.env.DSH_HOME
  }
})

test('aggregate-only relay: one node is unambiguous, several nodes are unknown', async () => {
  const server = http.createServer((req, res) => {
    res.setHeader('content-type', 'application/json')
    res.end(JSON.stringify({ ok: true, authRequired: true, deviceOnline: true, deviceCount: 1 }))
  })
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  const port = server.address().port
  const home = await fs.mkdtemp(path.join(os.tmpdir(), 'dsh-ra-'))
  process.env.DSH_HOME = home
  try {
    const mk = (nodeCount) => Config({
      endpoints: [{ id: 'local', label: 'Local', probeUrl: `http://127.0.0.1:${port}/__dsh_tunnel__/probe`, logDir: '' }],
      nodes: Array.from({ length: nodeCount }, (_, i) => ({
        id: `n${i}`, label: `N${i}`, deviceId: `dev-${i}`, relayUrl: `http://127.0.0.1:${port}`,
      })),
    })
    const sig = { signal: new AbortController().signal }

    const one = mockCtx()
    apply(one.ctx, mk(1))
    const oneStatus = one.tools.find((t) => t.name === 'remote_access_status')
    const a = JSON.parse(await oneStatus.execute({}, sig))
    assert.equal(a.node.online, true)
    assert.equal(a.node.source, 'probe-aggregate')

    const two = mockCtx()
    apply(two.ctx, mk(2))
    const twoStatus = two.tools.find((t) => t.name === 'remote_access_status')
    const b = JSON.parse(await twoStatus.execute({}, sig))
    assert.equal(b.node.online, 'unknown')
    assert.equal(b.node.source, 'probe-aggregate-ambiguous')
  } finally {
    await new Promise((resolve) => server.close(resolve))
    await fs.rm(home, { recursive: true, force: true })
    delete process.env.DSH_HOME
  }
})

// 2026-11-10：bundled skill provider 已移除（skill 改由 ~/.agents/skills 提供），
// 相应测试改为钉住「插件不依赖 skills/ 目录、不注册 skill provider」这一新事实。

test('no bundled skill provider: management plane registers without skills/ directory', () => {
  const calls = { tools: 0, commands: 0, providers: 0 }
  const ctx = {
    tools: { register: () => { calls.tools++ } },
    commands: { register: () => { calls.commands++ } },
    // 若有人重新引入 registerProvider 调用，这里会立刻把它记下来（回归门禁）
    skills: {
      registerProvider: () => {
        calls.providers++
        return () => {}
      },
    },
    inject: () => {},
  }
  assert.doesNotThrow(() => apply(ctx, Config({
    endpoints: [{ id: 'x', label: 'X', probeUrl: 'http://127.0.0.1:1/p' }],
    nodes: [],
  })))
  assert.equal(calls.providers, 0, 'plugin must not register a bundled skill provider')
  assert.ok(calls.tools >= 4, 'tools registered without bundled skill')
  assert.ok(calls.commands >= 1, 'commands registered without bundled skill')
})

// ── tunnel installer（数据面托管安装）──────────────────────────────────────
import { installBinary, fetchChannelEntry, renderUnit, installUnitLinux, channelPlatform, binaryName, installRoot } from '../tunnel-install.mjs'
import { createHash } from 'node:crypto'

const sha256 = (buf) => createHash('sha256').update(buf).digest('hex')

test('channelPlatform/binaryName map per OS', () => {
  assert.equal(channelPlatform('linux'), 'tunnel-linux')
  assert.equal(channelPlatform('darwin'), 'tunnel-darwin')
  assert.equal(channelPlatform('win32'), 'tunnel-windows')
  assert.throws(() => channelPlatform('sunos'))
  assert.equal(binaryName('win32'), 'dsh-tunnel.exe')
  assert.equal(binaryName('linux'), 'dsh-tunnel')
  assert.ok(String(installRoot({ DSH_HOME: '/tmp/x' }, 'linux')).endsWith('/bin/dsh-tunnel') === false) // root only
  assert.equal(String(installRoot({ DSH_HOME: '/tmp/x' }, 'linux')), '/tmp/x/bin')
})

test('installBinary: happy path verifies sha256 and installs atomically', async () => {
  const home = await fs.mkdtemp(path.join(os.tmpdir(), 'dsh-ra-bin-'))
  process.env.DSH_HOME = home
  const payload = Buffer.from('#!/bin/sh\necho fake-tunnel\n')
  const server = http.createServer((req, res) => { res.writeHead(200); res.end(payload) })
  await new Promise((r) => server.listen(0, '127.0.0.1', r))
  const port = server.address().port
  try {
    const out = await installBinary({
      url: `http://127.0.0.1:${port}/dl`, size: payload.length, sha256: sha256(payload), version: '1.0.0',
    })
    assert.equal(out.sha256, sha256(payload))
    const onDisk = await fs.readFile(out.path)
    assert.ok(onDisk.equals(payload))
    assert.equal((await fs.stat(out.path)).mode & 0o777, 0o755)
  } finally {
    server.close()
    delete process.env.DSH_HOME
  }
})

test('installBinary: sha256 mismatch must fail loudly (mutation check)', async () => {
  const home = await fs.mkdtemp(path.join(os.tmpdir(), 'dsh-ra-bin-'))
  process.env.DSH_HOME = home
  const payload = Buffer.from('real-bytes')
  const server = http.createServer((req, res) => { res.writeHead(200); res.end(payload) })
  await new Promise((r) => server.listen(0, '127.0.0.1', r))
  const port = server.address().port
  try {
    await assert.rejects(
      () => installBinary({ url: `http://127.0.0.1:${port}/dl`, size: payload.length, sha256: '0'.repeat(64), version: '1.0.0' }),
      /sha256 mismatch/,
    )
    // Nothing installed, no staging residue.
    assert.equal(await fs.readdir(path.join(home, 'bin')).then((xs) => xs.length), 0)
  } finally {
    server.close()
    delete process.env.DSH_HOME
  }
})

test('installBinary: size mismatch must fail (truncated download)', async () => {
  const home = await fs.mkdtemp(path.join(os.tmpdir(), 'dsh-ra-bin-'))
  process.env.DSH_HOME = home
  const payload = Buffer.from('short')
  const server = http.createServer((req, res) => { res.writeHead(200); res.end(payload) })
  await new Promise((r) => server.listen(0, '127.0.0.1', r))
  const port = server.address().port
  try {
    await assert.rejects(
      () => installBinary({ url: `http://127.0.0.1:${port}/dl`, size: payload.length + 10, sha256: sha256(payload), version: '1.0.0' }),
      /size mismatch/,
    )
  } finally {
    server.close()
    delete process.env.DSH_HOME
  }
})

test('renderUnit: carries relay/device/token and installer marker; refuses bad input', () => {
  const unit = renderUnit({ relayUrl: 'wss://r/__dsh_tunnel__/device', deviceId: 'dev-a', pairingTokenFile: '/x/token', localAddr: '127.0.0.1:3082' })
  assert.match(unit, /ExecStart=.*dsh-tunnel --relay wss:\/\/r\/__dsh_tunnel__\/device --device-id dev-a --local 127\.0\.0\.1:3082/)
  assert.match(unit, /DSH_TUNNEL_PAIRING_TOKEN_FILE=\/x\/token/)
  assert.match(unit, /installed by @local\/dsh-remote-access/)
  assert.match(unit, /Restart=always/)
  assert.throws(() => renderUnit({ deviceId: 'x' }), /relayUrl is required/)
  assert.throws(() => renderUnit({ relayUrl: 'wss://r' }), /deviceId is required/)
})

test('installUnitLinux: overwrites only its own unit; manual unit is preserved (mutation check)', async () => {
  const home = await fs.mkdtemp(path.join(os.tmpdir(), 'dsh-ra-unit-'))
  const unitDir = path.join(home, '.config/systemd/user')
  await fs.mkdir(unitDir, { recursive: true })
  const unit = renderUnit({ relayUrl: 'wss://r/d', deviceId: 'd1' })
  const prevHome = process.env.HOME
  process.env.HOME = home
  const prevPath = process.env.PATH
  // Fake systemctl so the test never touches the real user manager.
  const fakeBin = path.join(home, 'bin')
  await fs.mkdir(fakeBin, { recursive: true })
  await fs.writeFile(path.join(fakeBin, 'systemctl'), '#!/bin/sh\nexit 0\n', { mode: 0o755 })
  process.env.PATH = `${fakeBin}:${prevPath}`
  try {
    // 1) no unit → installs
    const r1 = await installUnitLinux(unit, {})
    assert.equal(r1.enabled, true)
    // 2) own unit → replaced
    await installUnitLinux(unit, {})
    const txt = await fs.readFile(path.join(unitDir, 'dsh-tunnel.service'), 'utf8')
    assert.match(txt, /installed by @local\/dsh-remote-access/)
    // 3) manual unit → refused
    await fs.writeFile(path.join(unitDir, 'dsh-tunnel.service'), '# hand written\n[Service]\nExecStart=/bin/true\n')
    await assert.rejects(() => installUnitLinux(unit, {}), /was not installed by this plugin/)
    const preserved = await fs.readFile(path.join(unitDir, 'dsh-tunnel.service'), 'utf8')
    assert.match(preserved, /hand written/)
  } finally {
    process.env.HOME = prevHome
    process.env.PATH = prevPath
  }
})

// 回归钉：setRelayDefaultNode 必须发真 POST。曾经漏写 method → node 默认 GET →
// relay 的 GET 端点只回显不写状态还返回 200 → 插件假报 ok，切换永远不同步
// （2026-11-08 用户实锤：设置页切节点本地变、relay 不变）。
test('setRelayDefaultNode sends a real POST (not a GET echo)', async (t) => {
  const http = await import('node:http');
  const seen = { method: '', body: '' };
  const srv = http.createServer((req, res) => {
    seen.method = req.method;
    let buf = '';
    req.on('data', (c) => { buf += c; });
    req.on('end', () => {
      seen.body = buf;
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ deviceId: JSON.parse(buf || '{}').deviceId || '' }));
    });
  });
  await new Promise((r) => srv.listen(0, '127.0.0.1', r));
  t.after(() => srv.close());
  const port = srv.address().port;
  const node = { relayUrl: `http://127.0.0.1:${port}`, deviceId: 'aliyun-ecs' };
  const res = await internals.setRelayDefaultNode(node, 'aliyun-ecs', '');
  assert.equal(res.ok, true, 'sync should succeed');
  assert.equal(seen.method, 'POST', `relay must receive POST, got ${seen.method}`);
  assert.match(seen.body, /aliyun-ecs/);
});


// 回归钉（2026-11-10「切换逻辑只留一套」）：client.js 不得再含插件内
// node-watch / 整页 reload / 双写 default-node；唯一写入 = switchNode 直发
// relay default-node，唯一跟随 = 壳页 SSE（在 Go 侧 shell.go）。
test('single switch path: no plugin node-watch, no reload, one write', async () => {
  const { readFile } = await import('node:fs/promises');
  const src = await readFile(new URL('../client.js', import.meta.url), 'utf8');

  // 1. 旧 watcher / reload 机制整体消失
  assert.doesNotMatch(src, /startNodeWatch/, 'plugin node-watch must be removed');
  assert.doesNotMatch(src, /maybeReload/, 'plugin reload helper must be removed');
  assert.doesNotMatch(src, /globalThis\.location\.reload\(\)/, 'no whole-page reload in switch path');
  assert.doesNotMatch(src, /new EventSource\(/, 'plugin must not open its own SSE watcher');
  assert.doesNotMatch(src, /\}, 5000\);/, 'no plugin-side 5s node polling fallback');

  // 2. 唯一写入函数存在，且只 POST relay default-node（404 时 host 代发仍同一处）
  assert.match(src, /async function switchNode\(deviceId\)/, 'switchNode is the single write entry');
  assert.match(src, /const RELAY_DEFAULT_NODE = '\/__dsh_tunnel__\/default-node'/, 'single relay write target');
  assert.match(src, /globalThis\.__rarSwitchNode = switchNode/, 'shared by settings page');

  // 3. 下拉与设置页都汇到 switchNode，不再各自 POST config 触发第二次写入
  assert.match(src, /const out = await switchNode\(node\.deviceId\)/, 'dropdown uses switchNode');
  assert.match(src, /const out = await switchNode\(patch\.nodeId\)/, 'settings node switch uses switchNode');
});
