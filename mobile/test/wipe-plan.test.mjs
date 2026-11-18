// test/wipe-plan.test.mjs —— 「清掉登录态、但保留配对」的功能契约闸门。
//
// 用户的原话：「不把配对信息去掉，但是清理掉所有 cookie 和本地存储，现在还是
// 有登录态」。所以这条闸门要钉住三类事实：
//   A. 纯逻辑（wipe-plan.js）：保留哪些配置键、判据、文案与结果措辞；
//   B. 接线：前端确实先读配置 → 整店清 → 写回配对 → 用凭据重连；
//   C. 原生侧：整店清必须走系统 API 且带复核，并把「绝不出现全清调用」的
//      那条不变式留在按站点清的文件里。
//
// 每条断言都做过变异检验（注掉实现即红），见文件末尾。
import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  PAIRING_KEYS,
  pickPairingConfig,
  hasReconnectCredential,
  wipeConfirmText,
  describeWipeOutcome,
} from '../src/wipe-plan.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const MOBILE = resolve(HERE, '..')
const read = (rel) => readFileSync(join(MOBILE, rel), 'utf8')
const KT_DIR = 'src-tauri/gen/android/app/src/main/java/com/namelessric/dsh_mobile'

/* ---------------- A. 纯逻辑 ---------------- */

test('A1 配对键覆盖「不用重新扫码」所需的全部凭据', () => {
  for (const k of ['relayUrl', 'deviceToken', 'deviceId', 'accessToken']) {
    assert.ok(PAIRING_KEYS.includes(k), `缺少配对键 ${k}`)
  }
  // 一次性 ticket 不许进白名单：它 60 秒就失效，带回来只会让 connect 走进
  // 「用一次性 ticket」分支，然后 404。
  assert.ok(!PAIRING_KEYS.includes('ticket'), 'ticket 是一次性的，不能当配对信息保留')
})

test('A2 空值被丢弃（不许留下 deviceToken:"" 这种假凭据）', () => {
  const out = pickPairingConfig({
    relayUrl: ' https://relay.example.com ',
    deviceToken: '',
    deviceId: '   ',
    accessToken: null,
  })
  assert.deepEqual(out, { relayUrl: 'https://relay.example.com' },
    '空串/null 必须整键丢弃，否则下游 if(cfg.deviceToken) 会误判为有凭据')
})

test('A3 无关字段与站点残留一律不带（清理函数不是配置搬运工）', () => {
  const out = pickPairingConfig({
    relayUrl: 'https://r.example.com',
    deviceToken: 'tok',
    updateSource: 'gitee',
    ticket: 'one-shot',
    dsh_mobile_config: { localStorage: 'mirror' },
  })
  assert.deepEqual(Object.keys(out).sort(), ['deviceToken', 'relayUrl'])
})

test('A4 脏输入不炸', () => {
  for (const bad of [null, undefined, 'junk', 42, []]) {
    assert.deepEqual(pickPairingConfig(bad), {})
  }
})

test('A5 免扫码重连的判据只看凭据本身', () => {
  assert.equal(hasReconnectCredential({ deviceToken: 'a' }), true)
  assert.equal(hasReconnectCredential({ accessToken: 'b' }), true)
  assert.equal(hasReconnectCredential({ relayUrl: 'https://x' }), false,
    '只有地址没有凭据 = 清完还必须手输/扫码，界面要提前说')
  assert.equal(hasReconnectCredential({ deviceToken: '  ' }), false)
  assert.equal(hasReconnectCredential(null), false)
})

test('A6 确认文案必须同时说清「清什么」「留什么」', () => {
  const withPair = wipeConfirmText({ relayUrl: 'https://r', deviceToken: 't' })
  assert.ok(/配对信息/.test(withPair), '没说配对信息会保留 —— 用户不敢点')
  assert.ok(/不用再扫码/.test(withPair), '没说清完免扫码')
  assert.ok(/所有站点/.test(withPair) && /localStorage|IndexedDB/.test(withPair), '没列清什么')

  const withoutPair = wipeConfirmText({ relayUrl: 'https://r' })
  assert.ok(/需要回到设置页扫码或填写 token/.test(withoutPair),
    '没有凭据时必须提前说清要重新配对，不能清完才告诉用户')
})

test('A7 结果措辞：只在复核为 0 时才算成功', () => {
  const ok = describeWipeOutcome({ ok: true, remaining: 0, pairing: { deviceToken: 't' } }, 'android')
  assert.equal(ok.tone, 'ok')
  assert.ok(/配对信息已保留/.test(ok.text), '成功文案要确认配对还在（这是用户最担心的一点）')

  // ok=true 但 remaining>0 是自相矛盾的上报 —— 按残留处理，不许当成功。
  const contradictory = describeWipeOutcome({ ok: true, remaining: 3, remainingHosts: ['qoder.cn'] }, 'android')
  assert.equal(contradictory.tone, 'bad')
  assert.ok(/3/.test(contradictory.text))
})

test('A8 残留要点名，Android 要标注「只覆盖已探测站点」', () => {
  const r = describeWipeOutcome(
    { ok: false, remaining: 2, remainingHosts: ['qoder.cn', 'www.trae.cn'] }, 'android')
  assert.ok(/qoder\.cn/.test(r.text), '残留站点必须点名，否则用户不知道该清哪个')
  assert.ok(/已探测/.test(r.text),
    'Android 的复核只能覆盖已知登录主机（无枚举 API），措辞必须如实标出范围')
  const d = describeWipeOutcome({ ok: false, remaining: 1, remainingHosts: [] }, 'desktop')
  assert.equal(d.tone, 'bad')
})

test('A9 复核数字缺失 ≠ 0（不许兜底成「已清空」）', () => {
  const r = describeWipeOutcome({ ok: true }, 'android')
  assert.equal(r.tone, 'bad', '没拿到 remaining 就不能说成功')
  assert.ok(/没有拿到复核结果/.test(r.text))
})

/* ---------------- B. 前端接线 ---------------- */

test('B1 main.js 先读配置、再清、再写回配对、最后重连', () => {
  const js = read('src/main.js')
  const start = js.indexOf('async function wipeAllKeepingPairing')
  assert.ok(start >= 0, '缺少 wipeAllKeepingPairing')
  const end = js.indexOf('cookieWipeAllBtn?.addEventListener', start)
  assert.ok(end > start, '找不到函数结束锚点')
  const body = js.slice(start, end)

  const iPick = body.indexOf('pickPairingConfig(')
  const iWipe = body.indexOf('_wipe_web_data')
  const iSave = body.indexOf('invokeSave(')
  const iConnect = body.indexOf('connect(')
  assert.ok(iPick >= 0 && iWipe >= 0 && iSave >= 0 && iConnect >= 0, '流程缺一步')
  // 顺序是不变式：清理之后 localStorage 镜像已经没了，必须清之前就读到手。
  assert.ok(iPick < iWipe, '必须先读出配对配置再清理（清完再读等于赌 Rust 文件还在）')
  assert.ok(iWipe < iSave, '必须清理之后写回配对信息')
  assert.ok(iSave < iConnect, '必须写回配对之后再重连')
  // 重连要走 connect（用设备凭据换 ticket），不能直接 reload 当前 GUI 地址。
  assert.ok(!/location\.reload\(\)/.test(body), '有 cookie 被清后 reload 当前页 = 401 白屏')
  // 没有凭据时不许硬闯（会带着注定 401 的地址撞上去）。
  assert.ok(/hasReconnectCredential\(restored\)/.test(body), '没有凭据时没有单独处理')
})

test('B2 两个平台各走自己的整店清理命令', () => {
  const js = read('src/main.js')
  assert.ok(/android_wipe_web_data/.test(js), '缺少 Android 整店清理命令调用')
  assert.ok(/desktop_wipe_web_data/.test(js), '缺少桌面整店清理命令调用')
})

test('B3 结果文案只在纯模块里拼（单一来源）', () => {
  const js = read('src/main.js')
  const start = js.indexOf('async function wipeAllKeepingPairing')
  const body = js.slice(start, js.indexOf('cookieWipeAllBtn?.addEventListener', start))
  assert.ok(/describeWipeOutcome\(/.test(body), 'main.js 没接结果措辞模块')
  // 判据：函数体里不许再出现自己拼的"已清除全部"类文案。
  assert.ok(!/已清除全部站点数据/.test(body), 'main.js 自拼了结果文案（措辞必须单一来源）')
})

/* ---------------- C. 界面与原生 ---------------- */

test('C1 面板有整店清理入口，且说明写清「保留配对」', () => {
  const html = read('src/index.html')
  assert.ok(/id="cookie-wipe-all"/.test(html), '缺少整店清理按钮')
  assert.ok(/清除全部站点数据（保留配对）/.test(html), '按钮文案没写清保留配对')
  const js = read('src/main.js')
  assert.ok(/cookieWipeAllBtn\?\.addEventListener/.test(js), '按钮没接监听')
  // 危险动作必须二次确认
  assert.ok(/window\.confirm\(\s*wipeConfirmText/.test(js), '危险动作没有 confirm（文案还要走纯模块）')
})

test('C2 按站点清的 Kotlin 里**绝不**出现全清调用（不变式仍然成立）', () => {
  // 整店清被刻意放进另一个类（WebDataWipe），就是为了让 CookieBridge 这条
  // 「按站点清、永不全清」的不变式继续被逐字钉住。
  const raw = readFileSync(join(MOBILE, KT_DIR, 'CookieBridge.kt'), 'utf8')
  const codeOnly = raw.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/.*$/gm, '')
  assert.ok(!/removeAllCookies/.test(codeOnly), 'CookieBridge 里出现了整店清（会踢掉 dsh 登录态）')
  assert.ok(!/deleteAllData/.test(codeOnly), 'CookieBridge 里出现了全量 storage 清理')
})

test('C3 整店清走系统 API 并复核残留', () => {
  const kt = readFileSync(join(MOBILE, KT_DIR, 'WebDataWipe.kt'), 'utf8')
  const codeOnly = kt.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/.*$/gm, '')
  assert.ok(/removeAllCookies\s*\{/.test(codeOnly), '没有用 removeAllCookies（带回调的重载）')
  assert.ok(/deleteAllData\(\)/.test(codeOnly), '没有清 web storage')
  assert.ok(/clearFormData\(\)/.test(codeOnly) && /clearHttpAuthUsernamePassword\(\)/.test(codeOnly),
    '没清表单数据 / HTTP 认证缓存（这两处也存着登录痕迹）')
  assert.ok(/hostsWithCookies\(/.test(codeOnly), '清完没有重新探测复核 —— 报数必须来自复核')
  assert.ok(/postDelayed/.test(codeOnly), '没有延迟复核（整店 API 是异步落盘的）')
  assert.ok(/clearCache\(true\)/.test(codeOnly), '没清网页缓存（会出现"数据清了页面还是旧的"）')
})

test('C4 整店清禁止用 Map/CookieManager 的「枚举后逐个删」替代', () => {
  // 这条防的是"看起来更安全、其实清不干净"的改法：按已知主机逐个删就是
  // CookieBridge 那条路，用户已经踩过（清单外的站点清不到）。
  const kt = readFileSync(join(MOBILE, KT_DIR, 'WebDataWipe.kt'), 'utf8')
  assert.ok(!/LOGIN_HOSTS/.test(kt), '整店清不该再依赖已知主机清单（那正是清不干净的原因）')
})

test('C5 桥类不抛异常过 JNI，超时报错而不是报成功', () => {
  const kt = stripKotlin(readFileSync(join(MOBILE, KT_DIR, 'WebDataWipeNative.kt'), 'utf8')).code
  assert.ok(/catch\s*\(\s*t:\s*Throwable\s*\)/.test(kt), '没有 catch(Throwable)')
  assert.ok(/清理超时/.test(kt), '超时没有单独报错')
  assert.ok(/清理回调返回空结果/.test(kt), '回调空结果被当成成功了')
})

test('C6 Rust 注册了两条整店清命令，并对复核字段做解析', () => {
  const rs = read('src-tauri/src/lib.rs')
  for (const cmd of ['android_wipe_web_data', 'desktop_wipe_web_data']) {
    assert.ok(new RegExp(`\\b${cmd}\\b`).test(rs), `lib.rs 里没有 ${cmd}`)
  }
  assert.ok(/async fn desktop_wipe_web_data/.test(rs), '桌面命令必须 async（Windows 同步调会死锁）')
  assert.ok(/clear_all_browsing_data\(\)/.test(rs), '桌面端没有调 wry 的整店清 API')
  // 桌面复核：报真实残留总数，不是「我调用成功了」。
  const start = rs.indexOf('async fn desktop_wipe_web_data')
  const body = rs.slice(start, rs.indexOf('// --- Android：按站点清登录 cookie', start))
  assert.ok(/"remaining"/.test(body), '桌面端结果里没有 remaining')
  assert.ok(/\.cookies\(\)/.test(body), '桌面端清完没有重新枚举复核')
})

test('C7 整店清的类引用必须在主线程 find_class 缓存（attached 线程找不到应用类）', () => {
  // 与 cookie 桥同一条铁律：调用发生在 Tauri 的异步运行时线程上，
  // 那些线程的 find_class 只看系统 classloader，应用类一律 ClassNotFoundException
  // （1.0.24 真机事故的同一类病）。所以 WIPE_CLASS 必须在 nativeInitBridge 里
  // 缓存好，调用路径只取 local ref。
  // ⚠️ cookie-clear.test.mjs 的 D7 只钉住了 COOKIE_CLASS；这里补上第二个类。
  const rs = read('src-tauri/src/lib.rs')
  const codeOnly = rs.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/.*$/gm, '')
  assert.ok(/WIPE_CLASS/.test(codeOnly), '没有缓存 WebDataWipeNative 的类引用')
  assert.ok(/find_class\(\s*"com\/namelessric\/dsh_mobile\/WebDataWipeNative"/.test(codeOnly),
    '没有在 nativeInitBridge 里 find_class 缓存整店清的类（find_class 才吃斜杠名）')
  // 调用路径不许再按名字加载
  assert.ok(!/loadClass\s*\(/.test(codeOnly), 'Rust 里又用 loadClass 动态加载类了')
  // 两个桥必须共用一个调用路径（各写一份 = 两份会漂移的实现）
  assert.ok(/fn call_bridge</.test(codeOnly), '没有共用的 call_bridge')
})

/** 剥 Kotlin 注释（块注释可嵌套）。 */
function stripKotlin(src) {
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

// 变异检验（逐个注入实测，期望全红）：
//  · PAIRING_KEYS 删掉 deviceToken                → A1/A3/A5 红
//  · pickPairingConfig 改成原样返回入参            → A2/A3 红
//  · 确认文案去掉「配对信息会保留」                → A6 红
//  · describeWipeOutcome 只看 ok 不看 remaining    → A7/A9 红
//  · main.js 把 pickPairingConfig 挪到清理之后     → B1 红
//  · main.js 改成 location.reload()               → B1 红
//  · WebDataWipe 改用 LOGIN_HOSTS 逐个删          → C4 红
//  · 去掉 removeAllCookies 只留 deleteOrigin      → C3 红
