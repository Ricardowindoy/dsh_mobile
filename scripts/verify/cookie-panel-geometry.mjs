// scripts/verify/cookie-panel-geometry.mjs —— 登录站点清理面板的真实几何量测。
//
// 为什么需要：单元闸门只能证明「代码里有这些元素」，证明不了「用户看得见、
// 点得到、不溢出」。这里用 CDP 驱动真浏览器，stub 掉 Tauri 的 invoke 喂假数据，
// 点真实按钮，量真实几何。
//
// 判据：
//   G1 底栏三个图标按钮（推送日志 / 登录站点清理）都 ≥44px 命中区
//   G2 点入口后面板出现，行数 = 喂进去的可清站点数（保护站点被过滤掉）
//   G3 每行「清除」按钮 ≥44px
//   G4 420 与 900 两档都无横向溢出
//   G5 面板说明文字里出现「不影响 dsh」类表述（用户敢不敢点取决于此）
//
// 跑法：node scripts/verify/cookie-panel-geometry.mjs
// 前置：cd mobile && ./node_modules/.bin/vite build
import { createServer } from 'node:http'
import { readFileSync, existsSync, statSync } from 'node:fs'
import { extname, join, resolve, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = dirname(fileURLToPath(import.meta.url))
const ROOT = resolve(HERE, '..', '..')
const DIST = join(ROOT, 'mobile', 'dist')
const SRC = join(ROOT, 'mobile', 'src')

if (!existsSync(join(DIST, 'index.html'))) {
  console.error(`SKIP: ${DIST}/index.html 不存在 —— 先跑 (cd mobile && ./node_modules/.bin/vite build)`)
  process.exit(2)
}
// 产物新鲜度：量陈旧产物等于量上一版代码。
const newestSrc = Math.max(...['styles.css', 'index.html', 'main.js', 'cookie-sites.js']
  .map((f) => statSync(join(SRC, f)).mtimeMs))
if (statSync(join(DIST, 'index.html')).mtimeMs < newestSrc) {
  console.error('SKIP: dist 比源码旧 —— 先重建再量（量陈旧产物的几何不是当前代码）')
  process.exit(2)
}

const MIME = {
  '.html': 'text/html; charset=utf-8', '.css': 'text/css; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8', '.svg': 'image/svg+xml', '.png': 'image/png',
}
const server = createServer((req, res) => {
  let p = decodeURIComponent(req.url.split('?')[0])
  if (p === '/') p = '/index.html'
  const file = join(DIST, p)
  if (!existsSync(file) || !statSync(file).isFile()) { res.writeHead(404); res.end('nope'); return }
  res.writeHead(200, { 'Content-Type': MIME[extname(file)] || 'application/octet-stream' })
  res.end(readFileSync(file))
})
await new Promise((r) => server.listen(0, '127.0.0.1', r))
const BASE = `http://127.0.0.1:${server.address().port}/`

/* 喂给页面的假数据：两行可真清 + 一行全是保护项（必须被过滤掉） */
const MOCK = [
  { site: 'www.trae.cn', cookieCount: 3, names: ['sid', 'uid', 'sso_token'] },
  { site: 'www.workbuddy.cn', cookieCount: 2, names: ['token', 'lang'] },
  { site: 'relay.example.com', cookieCount: 2, names: ['dsh_relay_device', 'dsh-auth-abc'] },
]

const ver = await (await fetch('http://127.0.0.1:9222/json/version')).json()
const ws = new WebSocket(ver.webSocketDebuggerUrl)
await new Promise((r, j) => { ws.onopen = r; ws.onerror = j })
let seq = 0
const pending = new Map()
ws.onmessage = (e) => {
  const m = JSON.parse(e.data)
  if (m.id && pending.has(m.id)) { const { res, rej } = pending.get(m.id); pending.delete(m.id); m.error ? rej(new Error(JSON.stringify(m.error))) : res(m.result) }
}
const send = (method, params = {}, sessionId) => new Promise((res, rej) => {
  const id = ++seq; pending.set(id, { res, rej })
  ws.send(JSON.stringify({ id, method, params, sessionId }))
})

const { targetId } = await send('Target.createTarget', { url: 'about:blank' })
const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true })
await send('Page.enable', {}, sessionId)
await send('Runtime.enable', {}, sessionId)

// Tauri invoke 桩：必须在页面脚本执行前注入。
const stub = `
  window.__TAURI_INTERNALS__ = {
    invoke: async (cmd, args) => {
      if (cmd === 'android_cookie_sites' || cmd === 'desktop_cookie_sites') return ${JSON.stringify(MOCK)};
      if (cmd === 'android_clear_site_cookies' || cmd === 'desktop_clear_site_cookies') return { requested: 2, cleared: 2, deleted: 2 };
      if (cmd === 'load_config') return null;
      if (cmd === 'read_crash_log') return '';
      if (cmd === 'boot_elapsed_ms') return 1;
      return null;
    },
    transformCallback: (cb) => { const id = Math.floor(Math.random() * 1e9); window['_' + id] = cb; return id; },
  };
  window.confirm = () => true;
`
await send('Page.addScriptToEvaluateOnNewDocument', { source: stub }, sessionId)

const results = []
for (const vp of [{ w: 420, h: 760 }, { w: 900, h: 700 }]) {
  await send('Emulation.setDeviceMetricsOverride', { width: vp.w, height: vp.h, deviceScaleFactor: 1, mobile: false }, sessionId)
  await send('Page.navigate', { url: BASE }, sessionId)
  await new Promise((r) => setTimeout(r, 900))

  // 点真实的入口按钮（不是直接改 hidden）
  const clicked = await send('Runtime.evaluate', {
    sessionId, returnByValue: true, awaitPromise: false,
    expression: `(() => {
      const b = document.getElementById('cookie-clear')
      if (!b) return 'no-button'
      b.click()
      return 'clicked'
    })()`,
  }, sessionId)
  await new Promise((r) => setTimeout(r, 700))

  const { result } = await send('Runtime.evaluate', {
    sessionId, returnByValue: true,
    expression: `(() => {
      const r = (el) => el ? el.getBoundingClientRect() : null
      const panel = document.getElementById('cookie-panel')
      const rows = [...document.querySelectorAll('#cookie-list .cookie-row')]
      const clearBtns = [...document.querySelectorAll('#cookie-list .cookie-clear-one')]
      const bottomBtns = [...document.querySelectorAll('.bottom-bar .icon-btn')]
      const doc = document.documentElement
      return {
        clicked: ${JSON.stringify(clicked.result?.value ?? '')},
        panelVisible: panel ? !panel.hasAttribute('hidden') : false,
        rowSites: rows.map((row) => row.querySelector('.cookie-row-site')?.textContent || ''),
        rowCount: rows.length,
        bottomBtnSizes: bottomBtns.map((b) => { const box = r(b); return { id: b.id, w: Math.round(box.width), h: Math.round(box.height) } }),
        clearBtnSizes: clearBtns.map((b) => { const box = r(b); return { w: Math.round(box.width), h: Math.round(box.height) } }),
        lastRowVsBar: (() => {
          // 滚到底后，最后一行必须完整在底栏之上（可点）。底栏是 sticky 磨砂层，
          // 内容从它下面穿过 —— 不滚到底就截图会看到"行被盖住"，那不是 bug。
          // 真滚动容器是 document（main 是 flex 高度 100%，自己不滚）——
          // 第一版写成 #setup.scrollTop，量出来永远滚动量为 0，等于没滚。
          const rows = [...document.querySelectorAll('#cookie-list .cookie-row')]
          const bar = document.querySelector('.bottom-bar')
          if (!rows.length || !bar) return null
          const sc = document.scrollingElement || document.documentElement
          sc.scrollTop = sc.scrollHeight
          const last = rows[rows.length - 1].getBoundingClientRect()
          const b = bar.getBoundingClientRect()
          return { lastBottom: Math.round(last.bottom), barTop: Math.round(b.top) }
        })(),
        noteText: (document.getElementById('cookie-panel-note')?.textContent || '').trim(),
        metaText: (panel?.querySelector('.update-meta')?.textContent || '').trim(),
        scrollWidth: doc.scrollWidth, clientWidth: doc.clientWidth,
      }
    })()`,
  }, sessionId)
  results.push({ vp, m: result.value })
}

let failed = 0
const check = (name, ok, detail) => {
  console.log(`  ${ok ? '✅' : '❌'} ${name}${ok ? '' : ` — ${detail}`}`)
  if (!ok) failed++
}
for (const { vp, m } of results) {
  console.log(`\n=== ${vp.w}x${vp.h} ===`)
  check('G1 底栏按钮命中区 ≥44px', m.bottomBtnSizes.length >= 2 && m.bottomBtnSizes.every((b) => b.h >= 44),
    JSON.stringify(m.bottomBtnSizes))
  check('G2 面板打开且只显示可真清的站点', m.panelVisible && m.rowCount === 2,
    `visible=${m.panelVisible} rows=${m.rowCount} sites=${JSON.stringify(m.rowSites)}`)
  check('G2b 保护站点被过滤掉', !m.rowSites.some((s) => s.includes('relay.example.com')),
    JSON.stringify(m.rowSites))
  check('G3 每行清除按钮 ≥44px', m.clearBtnSizes.length === 2 && m.clearBtnSizes.every((b) => b.h >= 44),
    JSON.stringify(m.clearBtnSizes))
  check('G4 无横向溢出', m.scrollWidth === m.clientWidth, `scrollWidth=${m.scrollWidth} clientWidth=${m.clientWidth}`)
  check('G5 说明文字写明不影响 dsh 登录', /dsh/.test(m.metaText) && /不受影响|不影响/.test(m.metaText), m.metaText)
  check('G6 滚到底时最后一行在底栏之上（可点）',
    !!m.lastRowVsBar && m.lastRowVsBar.lastBottom <= m.lastRowVsBar.barTop + 1,
    JSON.stringify(m.lastRowVsBar))
}

console.log(failed ? `\n❌ ${failed} 条断言不通过` : '\n✅ 全部断言通过')
server.close()
process.exit(failed ? 1 : 0)
