// scripts/verify/desktop-window-geometry.mjs —— 桌面窗口档的几何量测（真实浏览器，不是"看着对"）。
//
// 跑法：node scripts/verify/desktop-window-geometry.mjs
// 依赖：本机 CDP 9222 的常驻 chromium（node 内置 WebSocket，无需 playwright）。
// 前置：mobile/ 前端已构建（cd mobile && ./node_modules/.bin/vite build）。
//
// 量什么（对着目标值断言，不是打印给人看）：
//   420×760  手机档：主区未被桌面档接管、字段行高 46（改动前实测基线）、零横向溢出
//   900×700  桌面档：主区 560 居中、字段行高 34、零横向溢出
//   1280×800 桌面档：同上，主区仍 560（不拉满）
//
// ⚠️ 行高用 ±1px 容差：字体加载竞态会让同一页面量出 33 或 34（亚像素舍入）。
//    容差是**亚像素级**的，不掩盖真实回归 —— 桌面档真退回手机档会是 46（差 12px）。
import { createServer } from 'node:http'
import { readFileSync, existsSync, statSync } from 'node:fs'
import { extname, join, resolve, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = dirname(fileURLToPath(import.meta.url))
const DIST = resolve(HERE, '..', '..', 'mobile', 'dist')

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
}

if (!existsSync(join(DIST, 'index.html'))) {
  console.error(`SKIP: ${DIST}/index.html 不存在 —— 先跑 (cd mobile && ./node_modules/.bin/vite build)。没测到不等于成立。`)
  process.exit(2)
}

// 产物新鲜度：dist 比源码旧就是陈旧产物，量出来的几何是**上一版**的。
// （实测踩过：改了 CSS 忘了重建，几何门禁报了 5 条假失败 —— 门禁指向的不是真凶。）
const SRC_DIR = resolve(HERE, '..', '..', 'mobile', 'src')
const newestSrc = Math.max(
  ...['styles.css', 'index.html', 'main.js'].map((f) => statSync(join(SRC_DIR, f)).mtimeMs),
)
const distTime = statSync(join(DIST, 'index.html')).mtimeMs
if (distTime < newestSrc) {
  console.error(`SKIP: dist 比源码旧（${new Date(distTime).toISOString()} < ${new Date(newestSrc).toISOString()}）`)
  console.error('      → 先跑 (cd mobile && ./node_modules/.bin/vite build)。量陈旧产物得到的几何不是当前代码的。')
  process.exit(2)
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

/* ---- CDP 客户端（node 内置 WebSocket） ---- */
const ver = await (await fetch('http://127.0.0.1:9222/json/version')).json()
const ws = new WebSocket(ver.webSocketDebuggerUrl)
await new Promise((r, j) => { ws.onopen = r; ws.onerror = j })
let seq = 0
const pending = new Map()
ws.onmessage = (e) => {
  const m = JSON.parse(e.data)
  if (m.id && pending.has(m.id)) {
    const { res, rej } = pending.get(m.id)
    pending.delete(m.id)
    m.error ? rej(new Error(JSON.stringify(m.error))) : res(m.result)
  }
}
const send = (method, params = {}, sessionId) => new Promise((res, rej) => {
  const id = ++seq
  pending.set(id, { res, rej })
  ws.send(JSON.stringify({ id, method, params, sessionId }))
})

const { targetId } = await send('Target.createTarget', { url: 'about:blank' })
const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true })
await send('Page.enable', {}, sessionId)
await send('Runtime.enable', {}, sessionId)

const VIEWPORTS = [
  { label: '420x760  手机档', width: 420, height: 760, expect: 'phone' },
  // 700 是本项目桌面断点的**正下方**：它必须仍是手机档。这一档是「断点被改」
  // 的探测器 —— 断点若被改成 600/650，700 这一档就会误进桌面排版而报红。
  { label: '699x760  断点下方（应仍是手机档）', width: 699, height: 760, expect: 'phone' },
  { label: '700x760  断点上（应进桌面档）', width: 700, height: 760, expect: 'desktop' },
  { label: '900x700  桌面档', width: 900, height: 700, expect: 'desktop' },
  { label: '1280x800 桌面档', width: 1280, height: 800, expect: 'desktop' },
]

const results = []
for (const vp of VIEWPORTS) {
  await send('Emulation.setDeviceMetricsOverride', { width: vp.width, height: vp.height, deviceScaleFactor: 1, mobile: false }, sessionId)
  await send('Page.navigate', { url: BASE }, sessionId)
  await new Promise((r) => setTimeout(r, 900))
  const { result } = await send('Runtime.evaluate', {
    sessionId,
    returnByValue: true,
    expression: `(() => {
      const main = document.querySelector('.screen-main')
      const row = document.querySelector('.field-row')
      const cs = (el) => el ? getComputedStyle(el) : null
      const r = (el) => el ? el.getBoundingClientRect() : null
      const mainR = r(main), rowR = r(row)
      const doc = document.documentElement
      return {
        mainWidth: mainR ? Math.round(mainR.width) : null,
        mainLeft: mainR ? Math.round(mainR.left) : null,
        rowHeight: rowR ? Math.round(rowR.height) : null,
        scrollWidth: doc.scrollWidth,
        clientWidth: doc.clientWidth,
        appBarHeight: Math.round(r(document.querySelector('.app-bar'))?.height ?? 0),
        scanBtnHidden: !!document.querySelector('#scan-pair')?.hasAttribute('hidden'),
        scanSectionHidden: !!document.querySelector('.scan')?.hasAttribute('hidden'),
        groupTitleHidden: !!document.querySelector('.group-h')?.hasAttribute('hidden'),
        titleSize: cs(document.querySelector('.large-title'))?.fontSize,
      }
    })()`,
  }, sessionId)
  const m = result.value
  const overflow = m.scrollWidth !== m.clientWidth
  const near = (actual, expected) => Math.abs(actual - expected) <= 1
  const checks = []
  if (vp.expect === 'phone') {
    // 460 是 max-width：视口只有 420 时主区就是 420（撑满），不是 460。
    // 手机档的判据是「主区没有被桌面档的 560 接管」。
    checks.push(['主区 ≤ 460（手机档，未被桌面档接管）', m.mainWidth <= 460, `实际 ${m.mainWidth}`])
    checks.push(['主区未被桌面 560 接管', m.mainWidth !== 560, `实际 ${m.mainWidth}`])
    // 46 是**改动前就有的实测基线**，不是本次引入的回归：
    //   input(line-height 22 + padding 6×2 = 34) + 行 padding 6×2 = 46 > min-height 44。
    // 规范写的是 44，但那一栏从一开始就不是 44 —— 本次不动窄屏档，只保证它不变。
    checks.push(['字段行高 46（窄屏档基线未被桌面档改动）', near(m.rowHeight, 46), `实际 ${m.rowHeight}`])
    checks.push(['无横向溢出', !overflow, `scrollWidth=${m.scrollWidth} clientWidth=${m.clientWidth}`])
  } else {
    checks.push(['主区 560（桌面档）', m.mainWidth === 560, `实际 ${m.mainWidth}`])
    checks.push(['字段行高 34（桌面密度）', near(m.rowHeight, 34), `实际 ${m.rowHeight}`])
    // 主区在桌面档居中：期望左边距 = (视口 - 主区宽) / 2。
    // 700 视口下主区 560 + 左右各 70 留白，仍是「居中但两侧留白」而不是撑满。
    const expectLeft = Math.round((vp.width - m.mainWidth) / 2)
    checks.push(['主区居中（两侧留白）', m.mainLeft === expectLeft, `left=${m.mainLeft} 期望=${expectLeft}`])
    checks.push(['主区未撑满视口', m.mainWidth < vp.width, `main=${m.mainWidth} viewport=${vp.width}`])
    checks.push(['无横向溢出', !overflow, `scrollWidth=${m.scrollWidth} clientWidth=${m.clientWidth}`])
  }
  results.push({ vp, m, checks })
}

let failed = 0
for (const { vp, m, checks } of results) {
  console.log(`\n=== ${vp.label} ===`)
  console.log(`  main=${m.mainWidth}px@left${m.mainLeft}  row=${m.rowHeight}px  title=${m.titleSize}  appbar=${m.appBarHeight}px`)
  console.log(`  scan-btn-hidden=${m.scanBtnHidden}  scan-section-hidden=${m.scanSectionHidden}  group-title-hidden=${m.groupTitleHidden}`)
  for (const [name, ok, detail] of checks) {
    console.log(`  ${ok ? '✅' : '❌'} ${name}${ok ? '' : ` — ${detail}`}`)
    if (!ok) failed++
  }
}
console.log(failed ? `\n❌ ${failed} 条几何断言不通过` : '\n✅ 全部几何断言通过')
server.close()
process.exit(failed ? 1 : 0)
