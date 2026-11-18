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
//   G6 滚到底时最后一行在底栏之上（可点，不被磨砂底栏盖住）
//   G7 「清除全部站点数据」按钮存在、可见、≥44px，且不与上面的行重叠
//   G8 点它 → 先 confirm、再调整店清理命令、然后**把配对信息写回**并重连
//      （这条是用户要求的核心：清掉登录态但不丢配对；判据看 invoke 调用序列，
//       不看"我调用了函数"）
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

/* 已保存的 App 配置：真实配对产物 + 两个"无关偏好"（清理后不许被搬回来） */
const SAVED_CFG = {
  relayUrl: 'https://relay.example.com',
  deviceToken: 'dtok-abc',
  deviceId: 'dev-1',
  updateSource: 'gitee',
  ticket: undefined,
}

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
  window.__invokeLog = [];
  window.__savedConfig = null;
  window.__TAURI_INTERNALS__ = {
    invoke: async (cmd, args) => {
      if (cmd === 'android_cookie_sites' || cmd === 'desktop_cookie_sites') return ${JSON.stringify(MOCK)};
      if (cmd === 'android_clear_site_cookies' || cmd === 'desktop_clear_site_cookies') return { requested: 2, cleared: 2, deleted: 2 };
      if (cmd === 'android_wipe_web_data' || cmd === 'desktop_wipe_web_data') {
        window.__invokeLog.push(cmd);
        return { ok: true, remaining: 0, remainingHosts: [], storageCleared: true, formCleared: true };
      }
      if (cmd === 'save_config') { window.__savedConfig = args && args.config; window.__invokeLog.push(cmd); return null; }
      // 喂一台**已配对**的设备：这是用户真实场景（"配对信息不要丢"），
      // 也是 G8d 能验的东西 —— 配空配置量出来的结果证明不了任何事。
      if (cmd === 'load_config') return ${JSON.stringify(SAVED_CFG)};
      if (cmd === 'read_crash_log') return '';
      if (cmd === 'boot_elapsed_ms') return 1;
      return null;
    },
    transformCallback: (cb) => { const id = Math.floor(Math.random() * 1e9); window['_' + id] = cb; return id; },
  };
  // 记录 confirm 文案（要验"清什么/留什么"是否说清），并一律确认。
  window.__confirmTexts = [];
  window.confirm = (msg) => { window.__confirmTexts.push(String(msg)); return true; };
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
        wipeBtn: (() => {
          const b = document.getElementById('cookie-wipe-all')
          if (!b) return null
          const box = b.getBoundingClientRect()
          const rows = [...document.querySelectorAll('#cookie-list .cookie-row')]
          const lastRow = rows.length ? rows[rows.length - 1].getBoundingClientRect() : null
          return {
            text: (b.textContent || '').trim(),
            w: Math.round(box.width), h: Math.round(box.height),
            visible: box.width > 0 && box.height > 0 && !b.hidden,
            // 危险动作与每行的「清除」必须分开：间距 < 8px 会被误点。
            gapFromRows: lastRow ? Math.round(box.top - lastRow.bottom) : null,
          }
        })(),
        noteText: (document.getElementById('cookie-panel-note')?.textContent || '').trim(),
        metaText: (panel?.querySelector('.update-meta')?.textContent || '').trim(),
        scrollWidth: doc.scrollWidth, clientWidth: doc.clientWidth,
      }
    })()`,
  }, sessionId)
  // G8：点「清除全部站点数据」，看 invoke 序列与写回的配置。
  // 点之前先把面板滚回顶部，否则按钮可能在视口外（点击会落到别处）。
  await send('Runtime.evaluate', {
    sessionId,
    expression: `(() => {
      const sc = document.scrollingElement || document.documentElement
      sc.scrollTop = sc.scrollHeight
      const b = document.getElementById('cookie-wipe-all')
      if (b) b.click()
      return true
    })()`,
  }, sessionId)
  await new Promise((r) => setTimeout(r, 1200))
  const { result: after } = await send('Runtime.evaluate', {
    sessionId, returnByValue: true,
    expression: `({
      log: window.__invokeLog.slice(),
      saved: window.__savedConfig,
      confirms: window.__confirmTexts.slice(),
      status: (document.getElementById('cookie-clear-status')?.textContent || '').trim(),
    })`,
  }, sessionId)
  results.push({ vp, m: result.value, after: after.value })
}

let failed = 0
const check = (name, ok, detail) => {
  console.log(`  ${ok ? '✅' : '❌'} ${name}${ok ? '' : ` — ${detail}`}`)
  if (!ok) failed++
}
for (const { vp, m, after } of results) {
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
  check('G7 整店清理按钮可见、≥44px、写明保留配对',
    !!m.wipeBtn && m.wipeBtn.visible && m.wipeBtn.h >= 44 && /保留配对/.test(m.wipeBtn.text),
    JSON.stringify(m.wipeBtn))
  check('G7b 危险按钮与逐行「清除」有间距（不误触）',
    !!m.wipeBtn && m.wipeBtn.gapFromRows !== null && m.wipeBtn.gapFromRows >= 8,
    JSON.stringify(m.wipeBtn))

  // G8：invoke 序列必须证明「清 → 写回配对 → 重连」，且配对凭据原样回来。
  // 这里喂的 load_config 是 null（走 localStorage 兜底=空），所以真正能保住
  // 配对的路径只有"清理前先读到"——这正是我们要钉住的那一步。
  const a = after
  const seq = a.log
  const iWipe = seq.findIndex((c) => c.endsWith('_wipe_web_data'))
  // ⚠️ 用 lastIndexOf：boot() 在页面就绪时会先把配置镜像写一遍（防旧版本只写
  //    localStorage），那是清理**之前**的写，拿 indexOf 会误判成"写回在清理前"。
  const iSave = seq.lastIndexOf('save_config')
  check('G8 点按钮会先 confirm（危险动作）', a.confirms.length >= 1,
    JSON.stringify(a.confirms))
  check('G8b confirm 文案说清保留配对与免扫码',
    a.confirms.some((t) => /配对信息/.test(t) && /不用再扫码/.test(t)),
    JSON.stringify(a.confirms[0] || ''))
  check('G8c 先调整店清理命令，再 save_config 写回配对',
    iWipe >= 0 && iSave > iWipe, JSON.stringify(seq))
  check('G8d 写回的是配对信息（凭据原样回来，无关偏好/一次性 ticket 不带）',
    !!a.saved &&
      a.saved.relayUrl === SAVED_CFG.relayUrl &&
      a.saved.deviceToken === SAVED_CFG.deviceToken &&
      a.saved.deviceId === SAVED_CFG.deviceId &&
      !('updateSource' in a.saved) && !('ticket' in a.saved),
    JSON.stringify(a.saved))
  check('G8e 结果状态位报成功（复核 remaining=0）', /已清除全部站点数据/.test(a.status), a.status)
}

console.log(failed ? `\n❌ ${failed} 条断言不通过` : '\n✅ 全部断言通过')
server.close()
process.exit(failed ? 1 : 0)
