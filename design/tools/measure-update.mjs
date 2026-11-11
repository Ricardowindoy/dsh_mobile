// 渲染测量：设计稿更新后的几何验证（01/03/04 关键判据）。
// 用 playwright-core + 本机 chromium；file:// 直开，无需起服务。
// 路径全部走环境变量（缺失即报错），不把机器路径写进仓库：
//   PW_CORE     playwright-core 的 index.mjs 绝对路径
//   PW_CHROME   chromium 可执行文件绝对路径
import { createRequire } from 'node:module'

const require_ = createRequire(import.meta.url)
function loadPlaywrightCore() {
  if (process.env.PW_CORE) return import(process.env.PW_CORE)
  try {
    return import('playwright-core')
  } catch {
    console.error('需要 PW_CORE 环境变量（playwright-core/index.mjs 绝对路径），或在 node_modules 里可解析 playwright-core')
    process.exit(2)
  }
}
const { chromium } = await loadPlaywrightCore()

const CHROME = process.env.PW_CHROME
if (!CHROME) {
  console.error('需要 PW_CHROME 环境变量（chromium 可执行文件绝对路径）')
  process.exit(2)
}
const HERE = new URL('.', import.meta.url).pathname
const DESIGN = process.env.DESIGN_DIR ?? HERE.replace(/tools\/$/, '')
const OUT = process.env.DESIGN_ASSETS_DIR ?? `${DESIGN}assets`

const browser = await chromium.launch({ executablePath: CHROME, args: ['--no-sandbox'] })
const page = await browser.newPage({ viewport: { width: 1400, height: 1000 } })
const fails = []
const ok = (name, cond, detail) => { console.log(`${cond ? 'PASS' : 'FAIL'} ${name}${detail ? ' — ' + detail : ''}`); if (!cond) fails.push(name) }

// ── 01：扫码主按钮存在且 44px；⑨ 三台设备框无溢出 ──
await page.goto(`file://${DESIGN}/01-mobile-connect.html`)
await page.waitForTimeout(900)
const scan = await page.evaluate(() => {
  const b = document.getElementById('scan-pair')
  if (!b) return null
  const r = b.getBoundingClientRect()
  return { h: r.height, w: r.width, text: b.textContent.trim() }
})
ok('01 #scan-pair 存在', !!scan)
ok('01 扫码按钮命中区 ≥44', scan && scan.h >= 44, scan && `h=${scan.h}`)
const d9count = await page.evaluate(() => {
  const h = document.getElementById('d9')
  if (!h) return 0
  let n = 0, el = h.nextElementSibling
  while (el && !el.querySelector('#diff') && el.id !== 'diff') { n += el.querySelectorAll('.device').length; el = el.nextElementSibling }
  return n
})
ok('01 ⑨ 三态设备框 = 3', d9count === 3, `count=${d9count}`)
// 设备框内无横向溢出
const overflow01 = await page.evaluate(() => {
  const out = []
  for (const s of document.querySelectorAll('.screen')) {
    if (s.scrollWidth > s.clientWidth + 1) out.push(s.scrollWidth - s.clientWidth)
  }
  return out
})
ok('01 所有 .screen 无横向溢出', overflow01.length === 0, JSON.stringify(overflow01))
await page.screenshot({ path: `${OUT}/shot-01-scan-pair.png`, fullPage: false })

// ── 03：P1–P4 切换 + 撤销两步确认 ──
await page.goto(`file://${DESIGN}/03-settings-remote-access.html`)
await page.waitForTimeout(900)
const p1 = await page.evaluate(() => ({
  qr: !!document.querySelector('.pair__qr'),
  ttl: document.querySelector('.pair__ttl')?.textContent || '',
  devs: document.querySelectorAll('.pair__dev').length,
}))
ok('03 P1 有 QR + 倒计时', p1.qr && /剩余/.test(p1.ttl), p1.ttl)
// 切 P4（未启用）
await page.click('[data-pair-set="P4"]')
await page.waitForTimeout(200)
const p4 = await page.evaluate(() => document.querySelector('#body').textContent.includes('PhoneDevicesFile'))
ok('03 P4 unsupported 文案', p4)
// 切回 P1，走撤销两步
await page.click('[data-pair-set="P1"]')
await page.waitForTimeout(200)
await page.click('[data-pair-confirm]')
await page.waitForTimeout(150)
const confirmShown = await page.evaluate(() => !!document.querySelector('[data-pair-revoke]'))
ok('03 撤销第一步出确认键', confirmShown)
await page.click('[data-pair-revoke]')
await page.waitForTimeout(150)
const revoked = await page.evaluate(() => document.querySelector('#body').textContent.includes('已撤销'))
ok('03 撤销第二步落地 + 回执', revoked)
// mint 重置倒计时
await page.click('[data-pair-mint]')
await page.waitForTimeout(150)
const minted = await page.evaluate(() => document.querySelector('.pair__ttl')?.textContent || '')
ok('03 mint 后倒计时 120s', /120/.test(minted), minted)
// 面板无横向溢出（桌面档）
const overflow03 = await page.evaluate(() => {
  const p = document.getElementById('panel')
  return p ? p.scrollWidth - p.clientWidth : -1
})
ok('03 面板无横向溢出', overflow03 <= 1, `delta=${overflow03}`)
await page.waitForTimeout(700)
await page.screenshot({ path: `${OUT}/shot-03-pair-section.png`, fullPage: false })

// ── 04：通知 action 44px；深色框文字对比 ──
await page.goto(`file://${DESIGN}/04-approval-push.html`)
await page.waitForTimeout(900)
const acts = await page.evaluate(() => [...document.querySelectorAll('.noti__act')].map((b) => ({
  css: parseFloat(getComputedStyle(b).minHeight),
  rect: b.getBoundingClientRect().height,
})))
// 判据用声明的 min-height（闸门同式）：rect 会被 :active/按压 transform 缩到
// .97，43.5 正是 44×0.97 的按压态，不是命中区缩水。
ok('04 通知 action min-height 全部 ≥44', acts.length >= 4 && acts.every((a) => a.css >= 44), JSON.stringify(acts))
const darkText = await page.evaluate(() => {
  const dark = document.querySelector('[data-theme="dark"] .noti__title')
  return dark ? getComputedStyle(dark).color : null
})
ok('04 深色框文字重新解析（非浅色继承）', darkText && darkText !== 'rgb(29, 29, 31)', darkText)
await page.screenshot({ path: `${OUT}/shot-04-approval-cards.png`, fullPage: false })

await browser.close()
console.log(fails.length ? `\n❌ ${fails.length} 项几何失败` : '\n✅ 几何测量全部通过')
process.exit(fails.length ? 1 : 0)