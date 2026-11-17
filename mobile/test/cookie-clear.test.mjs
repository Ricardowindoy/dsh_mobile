// test/cookie-clear.test.mjs —— 「按站点清登录 cookie」的功能契约闸门。
//
// 背景：GUI 里登录 JetHub 跳转的外部站点（Trae/WorkBuddy 等）后，登录态落在
// App WebView 的 cookie store 里。用户切号需要按站点清，**不能**动 dsh 自己的
// 登录态（清了要重新扫码配对）。
//
// 这条闸门钉四件事：
//   A. 纯逻辑（cookie-sites.js）：保护清单、空行丢弃、计数重算
//   B. 界面：设置页有入口按钮与列表面板
//   C. 前端接线：两个平台各走自己的命令
//   D. 原生侧存在且带保护清单（Android Kotlin / Rust 命令注册）
//
// 每条都做过变异检验（注掉实现即红），见文件末尾注释。
import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, existsSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { isProtectedCookieName, normalizeCookieSites, PROTECTED_COOKIE_PREFIXES } from '../src/cookie-sites.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const MOBILE = resolve(HERE, '..')
const read = (rel) => readFileSync(join(MOBILE, rel), 'utf8')

/* ---------------- A. 纯逻辑 ---------------- */

test('A1 保护清单覆盖 dsh 自身三种登录 cookie', () => {
  // 三种都要在：设备 cookie、legacy 共享 token、每节点签名 cookie。
  assert.ok(PROTECTED_COOKIE_PREFIXES.includes('dsh_relay_device'))
  assert.ok(PROTECTED_COOKIE_PREFIXES.includes('dsh_relay_token'))
  assert.ok(PROTECTED_COOKIE_PREFIXES.includes('dsh-auth-'))
  for (const n of ['dsh_relay_device', 'dsh_relay_token', 'dsh-auth-abc123']) {
    assert.equal(isProtectedCookieName(n), true, `${n} 必须受保护`)
  }
})

test('A2 保护判定大小写不敏感，且不误伤相似名', () => {
  assert.equal(isProtectedCookieName('DSH_RELAY_DEVICE'), true)
  assert.equal(isProtectedCookieName('dsh-auth-9f2c'), true)
  // 相似但不同的名字不能被当成保护项（否则会漏清用户真想清的 cookie）
  assert.equal(isProtectedCookieName('sid'), false)
  assert.equal(isProtectedCookieName('dsh_token_other'), false)
  assert.equal(isProtectedCookieName(''), false)
  assert.equal(isProtectedCookieName(undefined), false)
})

test('A3 只剩保护项的行整行丢弃（不画「清了也没用」的行）', () => {
  const rows = normalizeCookieSites([
    { site: 'relay.example.com', names: ['dsh_relay_device', 'dsh-auth-x'] },
    { site: 'www.trae.cn', names: ['sid', 'dsh_relay_device'] },
  ])
  assert.equal(rows.length, 1, '只有真能清的站点才留一行')
  assert.equal(rows[0].site, 'www.trae.cn')
})

test('A4 cookieCount 重算为「实际可清」数量，不信上游数字', () => {
  const rows = normalizeCookieSites([
    { site: 'www.trae.cn', cookieCount: 99, names: ['sid', 'dsh_relay_device', 'uid'] },
  ])
  assert.equal(rows[0].cookieCount, 2, '上游把保护项也算进去了，必须重算')
  assert.deepEqual(rows[0].names, ['sid', 'uid'])
})

test('A5 没有 names 的行不可清，直接丢弃（宁可少说）', () => {
  const rows = normalizeCookieSites([
    { site: 'x.com', cookieCount: 3 },
    { site: 'y.com', names: [] },
  ])
  assert.equal(rows.length, 0, '不知道名字就不能假装能清')
})

test('A6 缺 site / 脏输入不炸', () => {
  const rows = normalizeCookieSites([null, undefined, {}, { site: '' }, { site: '  ' }, 'junk', 42])
  assert.deepEqual(rows, [])
  assert.deepEqual(normalizeCookieSites(undefined), [])
  assert.deepEqual(normalizeCookieSites('not-an-array'), [])
})

test('A7 site 原样保留，不做「整理」', () => {
  const rows = normalizeCookieSites([{ site: 'WWW.WorkBuddy.CN', names: ['a'] }])
  assert.equal(rows[0].site, 'WWW.WorkBuddy.CN', '显示哪个 host 就哪个，不改成小写')
})

/* ---------------- B. 界面 ---------------- */

test('B1 设置页有入口按钮与列表面板', () => {
  const html = read('src/index.html')
  assert.ok(/id="cookie-clear"/.test(html), '缺少底栏入口按钮 #cookie-clear')
  assert.ok(/id="cookie-panel"/.test(html), '缺少列表面板 #cookie-panel')
  assert.ok(/id="cookie-list"/.test(html), '缺少列表容器 #cookie-list')
  assert.ok(/id="cookie-refresh"/.test(html), '缺少重新扫描按钮')
  // 入口必须是图形按钮 + aria-label（与「推送日志」同一套规范）
  assert.ok(/id="cookie-clear"[^>]*aria-label="[^"]+"/.test(html), '入口按钮缺 aria-label')
})

test('B2 面板说明里明确写了「不影响 dsh 自身登录」', () => {
  const html = read('src/index.html')
  const block = html.slice(html.indexOf('id="cookie-panel"'), html.indexOf('id="cookie-panel"') + 800)
  assert.ok(/dsh/.test(block) && /登录态不受影响|不影响/.test(block),
    '面板必须说明它不碰 dsh 登录态，否则用户不敢点')
})

/* ---------------- C. 前端接线 ---------------- */

test('C1 两个平台各走自己的命令', () => {
  const js = read('src/main.js')
  assert.ok(/android_cookie_sites/.test(js), '缺 Android 站点枚举命令')
  assert.ok(/android_clear_site_cookies/.test(js), '缺 Android 清理命令')
  assert.ok(/desktop_cookie_sites/.test(js), '缺桌面站点枚举命令')
  assert.ok(/desktop_clear_site_cookies/.test(js), '缺桌面清理命令')
  // 平台分流必须在：Android 用 android_*，否则用 desktop_*
  assert.ok(/android/i.test(js) && /invoke\("android_cookie_sites"\)/.test(js))
})

test('C2 清理前有确认、清理后有如实结果反馈', () => {
  const js = read('src/main.js')
  assert.ok(/window\.confirm\(/.test(js), '清理前必须确认（会丢登录态）')
  // 「没清到任何东西」不许兜底成成功文案
  assert.ok(/没有可清的 cookie/.test(js), '清到 0 枚时必须如实说，不能报成功')
})

test('C3 归一化只在纯模块里做，main.js 不再自写保护清单', () => {
  const js = read('src/main.js')
  assert.ok(/normalizeCookieSites/.test(js), 'main.js 没接纯模块')
  assert.ok(!/PROTECTED_COOKIES\s*=\s*\[/.test(js),
    'main.js 仍有第二份保护清单（判定规则必须单一来源）')
})

/* ---------------- D. 原生侧 ---------------- */

test('D1 Kotlin 桥存在，带保护清单与真实探测', () => {
  const kt = join(MOBILE, 'src-tauri/gen/android/app/src/main/java/com/namelessric/dsh_mobile/CookieBridge.kt')
  assert.ok(existsSync(kt), '缺少 CookieBridge.kt')
  const src = readFileSync(kt, 'utf8')
  assert.ok(/CookieManager\.getInstance\(\)/.test(src), '没走系统 CookieManager')
  assert.ok(/dsh_relay_device/.test(src) && /dsh-auth-/.test(src), 'Kotlin 侧缺保护清单')
  // 删除必须是「过期覆盖」而不是 removeAllCookies（全清会踢掉 dsh 自己）。
  // ⚠️ 判据必须先剥注释：源码注释里**提到** removeAllCookies 是在解释
  //    "为什么不用它"，按字面断言会把正确的实现判红（这条自己就踩过一次）。
  const codeOnly = src
    .replace(/\/\*[\s\S]*?\*\//g, '')   // 块注释
    .replace(/\/\/.*$/gm, '')             // 行注释
  assert.ok(/Max-Age=0/.test(codeOnly), '删除没用过期覆盖')
  assert.ok(!/removeAllCookies/.test(codeOnly), '代码里出现了全清调用（会清掉 dsh 登录态）')
})

// 下面两条是「CI 才会红」的错误类型：本地没有 Android 编译链，Kotlin 的
// import 漏写、注释不闭合在本地看不见，只有 GitHub 的 Android job 才会炸，
// 一轮 20 分钟。所以在本地用静态检查把它们提前钉住。

const KT_DIR = 'src-tauri/gen/android/app/src/main/java/com/namelessric/dsh_mobile'

/** 剥掉 Kotlin 的注释（块注释**可嵌套**，必须按深度走，不能只找星号斜杠）。 */
function stripKotlinComments(src) {
  let out = ''
  let depth = 0
  let i = 0
  while (i < src.length) {
    if (src.startsWith('/*', i)) { depth++; i += 2; continue }
    if (depth > 0 && src.startsWith('*/', i)) { depth--; i += 2; continue }
    if (depth === 0 && src.startsWith('//', i)) {
      while (i < src.length && src[i] !== '\n') i++
      continue
    }
    if (depth === 0) out += src[i]
    i++
  }
  return { code: out, depth }
}

test('D3 Kotlin 文件块注释必须闭合（注释可嵌套，漏一个整文件失效）', () => {
  const { readdirSync } = { readdirSync: null }
  const files = ['CookieBridge.kt', 'CookieBridgeNative.kt']
  for (const f of files) {
    const src = readFileSync(join(MOBILE, KT_DIR, f), 'utf8')
    const { depth } = stripKotlinComments(src)
    assert.equal(depth, 0,
      `${f} 的块注释没闭合（Kotlin 注释允许嵌套，KDoc 里出现斜杠加星号就会开一个永不闭合的嵌套注释）`)
  }
})

test('D4 用到的 Android 平台符号必须 import（漏了本地看不见、CI 才红）', () => {
  // 只覆盖本项目实际用到的平台符号，避免猜出一堆误报。
  const PKG = {
    ValueCallback: 'android.webkit.ValueCallback',
    CookieManager: 'android.webkit.CookieManager',
    WebView: 'android.webkit.WebView',
    Handler: 'android.os.Handler',
    Looper: 'android.os.Looper',
    Activity: 'android.app.Activity',
  }
  for (const f of ['CookieBridge.kt', 'CookieBridgeNative.kt']) {
    const raw = readFileSync(join(MOBILE, KT_DIR, f), 'utf8')
    const { code } = stripKotlinComments(raw)
    for (const [sym, fq] of Object.entries(PKG)) {
      const used = new RegExp(`\\b${sym}\\b`).test(code)
      if (!used) continue
      const imported = new RegExp(`^\\s*import\\s+${fq.replace(/\./g, '\\.')}\\s*$`, 'm').test(code)
      const qualified = code.includes(fq)
      assert.ok(imported || qualified, `${f} 用了 ${sym} 却没 import ${fq}`)
    }
  }
})

test('D2 Rust 注册了四条 cookie 命令', () => {
  const rs = read('src-tauri/src/lib.rs')
  for (const cmd of ['android_cookie_sites', 'android_clear_site_cookies',
    'desktop_cookie_sites', 'desktop_clear_site_cookies']) {
    assert.ok(new RegExp(`\\b${cmd}\\b`).test(rs), `lib.rs 里没有 ${cmd}`)
  }
  // 桌面两个命令必须是 async（Windows 上同步调 cookies() 会死锁）
  assert.ok(/async fn desktop_cookie_sites/.test(rs), 'desktop_cookie_sites 必须 async')
  assert.ok(/async fn desktop_clear_site_cookies/.test(rs), 'desktop_clear_site_cookies 必须 async')
})

// 变异检验（逐个注入实测全红）：
//  · 把 PROTECTED_COOKIE_PREFIXES 里任一前缀删掉     → A1/A2 红
//  · normalizeCookieSites 去掉 names.length===0 守卫  → A5 红
//  · cookieCount 直接取上游 row.cookieCount           → A4 红
//  · main.js 换回自写 PROTECTED_COOKIES               → C3 红
//  · Kotlin 改用 removeAllCookies                     → D1 红
//  · desktop_cookie_sites 去掉 async                  → D2 红（并由 .probe/desktopparity 编译验证抓 E0728）
