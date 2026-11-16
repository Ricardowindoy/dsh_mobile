// .verify-ui/probe-row.mjs —— 量清 .field-row 的高度到底被谁撑起来（不猜）。
import { createServer } from 'node:http'
import { readFileSync, existsSync, statSync } from 'node:fs'
import { extname, join, resolve, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = dirname(fileURLToPath(import.meta.url))
const DIST = resolve(HERE, '..', '..', 'mobile', 'dist')
const MIME = { '.html': 'text/html; charset=utf-8', '.css': 'text/css; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.svg': 'image/svg+xml' }
const server = createServer((req, res) => {
  let p = decodeURIComponent(req.url.split('?')[0]); if (p === '/') p = '/index.html'
  const file = join(DIST, p)
  if (!existsSync(file) || !statSync(file).isFile()) { res.writeHead(404); res.end('nope'); return }
  res.writeHead(200, { 'Content-Type': MIME[extname(file)] || 'application/octet-stream' }); res.end(readFileSync(file))
})
await new Promise((r) => server.listen(0, '127.0.0.1', r))
const BASE = `http://127.0.0.1:${server.address().port}/`

const ver = await (await fetch('http://127.0.0.1:9222/json/version')).json()
const ws = new WebSocket(ver.webSocketDebuggerUrl)
await new Promise((r, j) => { ws.onopen = r; ws.onerror = j })
let seq = 0; const pending = new Map()
ws.onmessage = (e) => { const m = JSON.parse(e.data); if (m.id && pending.has(m.id)) { const { res, rej } = pending.get(m.id); pending.delete(m.id); m.error ? rej(new Error(JSON.stringify(m.error))) : res(m.result) } }
const send = (method, params = {}, sessionId) => new Promise((res, rej) => { const id = ++seq; pending.set(id, { res, rej }); ws.send(JSON.stringify({ id, method, params, sessionId })) })
const { targetId } = await send('Target.createTarget', { url: 'about:blank' })
const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true })
await send('Page.enable', {}, sessionId); await send('Runtime.enable', {}, sessionId)

for (const w of [420, 900]) {
  await send('Emulation.setDeviceMetricsOverride', { width: w, height: 760, deviceScaleFactor: 1, mobile: false }, sessionId)
  await send('Page.navigate', { url: BASE }, sessionId)
  await new Promise((r) => setTimeout(r, 800))
  const { result } = await send('Runtime.evaluate', { sessionId, returnByValue: true, expression: `(() => {
    const row = document.querySelector('.field-row')
    const cs = getComputedStyle(row)
    const kids = [...row.querySelectorAll('*')].map(el => ({
      tag: el.tagName.toLowerCase(), cls: el.className?.toString?.().slice(0,30),
      h: Math.round(el.getBoundingClientRect().height),
      w: Math.round(el.getBoundingClientRect().width),
      lh: getComputedStyle(el).lineHeight, pad: getComputedStyle(el).padding, fs: getComputedStyle(el).fontSize,
    }))
    return {
      rowH: Math.round(row.getBoundingClientRect().height),
      rowMinH: cs.minHeight, rowPad: cs.padding, rowAlign: cs.alignItems,
      input: kids.find(k => k.tag === 'input'),
      action: kids.find(k => k.cls?.includes('field-action')),
      kids,
    }
  })()` }, sessionId)
  console.log(`\n=== viewport ${w}px ===`)
  console.log(JSON.stringify(result.value, null, 2))
}
server.close(); process.exit(0)
