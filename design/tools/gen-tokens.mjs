#!/usr/bin/env node
/* =============================================================================
 * gen-tokens.mjs —— 从插件自带文件机械生成 design/tokens.css
 * -----------------------------------------------------------------------------
 * 为什么要有这个脚本：设计集里最容易被"顺手美化"的就是数值。这里不让任何人
 * （包括模型）手抄令牌——浅色层逐字节取自 styles/apple.css，深色层逐值取自
 * styles/palettes.json 的 apple-night.tokens。派生层每条都带算式与判据，
 * 不达标的抛错不生成，算式写进 tools/derived.json 供 check.mjs 复算。
 *
 * 用法：node design/tools/gen-tokens.mjs
 * ========================================================================== */
import { readFileSync, writeFileSync, existsSync } from 'node:fs'
import { resolve, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = dirname(fileURLToPath(import.meta.url))
const DESIGN = resolve(HERE, '..')
const PLUGIN = process.env.UI_DESIGNER_ROOT
if (!PLUGIN) {
  console.error('需要 UI_DESIGNER_ROOT 环境变量（指向 dsh-ui-designer 插件根目录）')
  process.exit(2)
}
const OUT = resolve(DESIGN, 'tokens.css')

const appleCss = readFileSync(resolve(PLUGIN, 'styles', 'apple.css'), 'utf8')
const palettesPath = resolve(PLUGIN, 'styles', 'palettes.json')
if (!existsSync(palettesPath)) throw new Error(`插件没有 palettes.json：${palettesPath}`)
const night = JSON.parse(readFileSync(palettesPath, 'utf8')).palettes.find((p) => p?.id === 'apple-night')
if (!night) throw new Error('apple-night 不在 palettes.json 里，插件版本对不上')
const official = Object.entries(night.tokens ?? {})
if (official.length === 0) throw new Error('apple-night.tokens 是空的')

/* ── 颜色数学（check.mjs 用同一套，别两处各写一份） ─────────────────────── */
const hexToRgb = (h) => {
  const m = /^#?([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(String(h).trim())
  return m ? [1, 2, 3].map((i) => parseInt(m[i], 16)) : null
}
const lin = (v) => { const x = v / 255; return x <= 0.04045 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4 }
const lum = (c) => 0.2126 * lin(c[0]) + 0.7152 * lin(c[1]) + 0.0722 * lin(c[2])
const ratio = (a, b) => { const l1 = lum(a), l2 = lum(b); return +(((Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05)).toFixed(2)) }
const over = (fg, alpha, bg) => fg.map((v, i) => v * alpha + bg[i] * (1 - alpha))

/* 浅色侧官方值从 apple.css 的 :root 现取，不复制常量表（两处各存必漂）。 */
const rootBlock = appleCss.slice(appleCss.indexOf(':root'), appleCss.indexOf('}', appleCss.indexOf(':root')) + 1)
const lightVal = (key) => {
  const m = new RegExp(`--set-${key}:\\s*([^;]+);`).exec(rootBlock)
  if (!m) throw new Error(`apple.css 的 :root 里没有 --set-${key}`)
  return m[1].trim()
}
const nightVal = (key) => {
  const v = night.tokens[`--set-${key}`]
  if (v === undefined) throw new Error(`apple-night 没有 --set-${key}`)
  return String(v).trim()
}

/* ── 派生层的判据分类（这一版差点自己判错，写清楚） ─────────────────────
 *   tint   语义色**文字**压在染底上      → WCAG 1.4.3 文本对比，门槛 4.5
 *   line   染成的发丝分隔线 / 描边        → 不断言比值
 *   ring   焦点环外圈光晕                → 不断言比值
 *   material chrome 半透明底              → 断言"浅色档复现上游原值"
 *
 * 为什么 line/ring 不套 3:1：22% alpha 的发丝线合成后与表面只差 ~1.2:1，
 * 这是发丝线的**本意**（Apple 的 --set-line 本身就是 rgba(0,0,0,.08)）。
 * 1.4.11 的 3:1 针对"边界是唯一识别手段"的控件；这里的线旁边同时有语义色
 * 文字，不是唯一标识。上一版脚本对所有条目一刀切要求 4.5，报出
 * "--set-danger-line 3.58 不达标"——那是脚本的范畴错误，不是设计错误。
 * 真正该断言的是焦点指示物本身：1px 内描边（brand vs surface）≥3.0，
 * 见下面 focusIndicator 那条。 */
/* alpha 不是凭手感定的：tint 档由上面那段"取较差一侧"的断言反推出来 ——
 * 浅色 danger 用 0.06 时压画布只有 4.45（跌破 4.5），所以降到 0.05（4.53）；
 * 浅色 warn 同理 0.06→0.04（4.53）。深色两档本来就够，保持较明显的染色。 */
const plan = [
  { name: '--set-danger-tint', key: 'danger', kind: 'tint', aL: 0.05, aD: 0.08 },
  { name: '--set-warn-tint', key: 'warn', kind: 'tint', aL: 0.04, aD: 0.10 },
  /* 2026-11-08 胶囊控件规范（.wb-badge）：染底 12% / 描边 30% —— 但 tint 档受
   * 4.5 对比断言约束，官方 ink 色在 12% 染底上只有 4.3~4.4 会拦，所以染底沿用
   * 上面已反推过的 alpha；描边无断言，统一到控件的 30%。ok 色此前没有 line 档
   * （浅色 ok-ink 压白卡恰好 4.53，任何 tint 都会跌破 4.5），胶囊描边补上它。 */
  { name: '--set-ok-line', key: 'ok', kind: 'line', aL: 0.30, aD: 0.30 },
  { name: '--set-danger-line', key: 'danger', kind: 'line', aL: 0.30, aD: 0.30 },
  { name: '--set-warn-line', key: 'warn', kind: 'line', aL: 0.30, aD: 0.30 },
  { name: '--set-ring', key: 'brand', kind: 'ring', aL: 0.22, aD: 0.26 },
]

const lines = { light: [], dark: [] }
const derived = []

for (const { name, key, kind, aL, aD } of plan) {
  const fgL = hexToRgb(lightVal(key)), fgD = hexToRgb(nightVal(key))
  const surfL = hexToRgb(lightVal('surface')), surfD = hexToRgb(nightVal('surface'))
  const cvL = hexToRgb(lightVal('bg')), cvD = hexToRgb(nightVal('bg'))
  if (!fgL || !fgD || !surfL || !surfD || !cvL || !cvD) throw new Error(`${key} 不是 #rrggbb，派生算式需要重写`)
  const mixL = over(fgL, aL, surfL), mixD = over(fgD, aD, surfD)
  /* 染底有两种落点：压在卡片(--set-surface)上，或直接压在画布(--set-bg)上。
   * 只按卡片算会漏 —— 浅色 danger 压卡片 4.84 好看，压画布只有 4.45，跌破 4.5。
   * 所以取两者中的**最差值**当判据，alpha 也由这个最差值决定。 */
  const worst = (a, mixS, mixC) => Math.min(ratio(a, mixS), ratio(a, mixC))
  const rec = { name, key, kind, alphaLight: aL, alphaDark: aD,
    light: `rgba(${fgL.join(', ')}, ${aL})`, dark: `rgba(${fgD.join(', ')}, ${aD})`,
    textOnTintLight: worst(fgL, mixL, over(fgL, aL, cvL)),
    textOnTintDark: worst(fgD, mixD, over(fgD, aD, cvD)),
    lineVsSurfaceLight: ratio(mixL, surfL), lineVsSurfaceDark: ratio(mixD, surfD) }
  if (kind === 'tint') {
    if (rec.textOnTintLight < 4.5 || rec.textOnTintDark < 4.5) {
      throw new Error(`${name} 染底上文字对比不达标：浅 ${rec.textOnTintLight} / 深 ${rec.textOnTintDark}（门槛 4.5，已取"压卡片/压画布"较差一侧）。请降 alpha`)
    }
  }
  // line/ring 只记录实测值供评审，不设比值门槛（理由见上面注释）
  lines.light.push(`  ${name}: ${rec.light};`)
  lines.dark.push(`  ${name}: ${rec.dark};`)
  derived.push(rec)
}

/* 语义色的"文字档"（ink）：官方 --set-ok 是图形色，压在白卡上只有 4.14、
 * 压画布 3.80，做文字不达 AA 的 4.5。不能改官方值（palette 拥有它，且闸门逐值
 * 比对），所以另派生一档 ink：算法是"沿 RGB 三通道等比压暗，直到压卡片与压画布
 * 两侧都 ≥4.5 为止"，步长 2%，因此结果可复算、不是手挑的色号。
 * 深色档官方值本来就够（ok 8.42），算法自然返回原值 —— 不做无谓偏离。 */
const inkOf = (hexColor, surfaces) => {
  const base = hexToRgb(hexColor)
  for (let k = 1; k >= 0.2; k = +(k - 0.02).toFixed(4)) {
    const c = base.map((v) => Math.round(v * k))
    if (surfaces.every((s) => ratio(c, s) >= 4.5)) return { rgb: c, k }
  }
  return null
}
for (const key of ['ok', 'warn', 'danger', 'brand']) {
  const name = `--set-${key}-ink`
  const lHex = lightVal(key), dHex = nightVal(key)
  const lSurf = [hexToRgb(lightVal('surface')), hexToRgb(lightVal('bg'))]
  const dSurf = [hexToRgb(nightVal('surface')), hexToRgb(nightVal('bg'))]
  const li = inkOf(lHex, lSurf), di = inkOf(dHex, dSurf)
  if (!li || !di) throw new Error(`${key} 压暗到 0.2 仍达不到 4.5，ink 算法需要重设计`)
  const f2 = (v) => v.toString(16).padStart(2, '0')
  const lVal = `#${li.rgb.map(f2).join('')}`, dVal = `#${di.rgb.map(f2).join('')}`
  lines.light.push(`  ${name}: ${lVal};`)
  lines.dark.push(`  ${name}: ${dVal};`)
  derived.push({ name, kind: 'ink', sourceKey: key, threshold: 4.5,
    light: lVal, dark: dVal, lightScale: li.k, darkScale: di.k,
    contrastLight: Math.min(ratio(li.rgb, lSurf[0]), ratio(li.rgb, lSurf[1])),
    contrastDark: Math.min(ratio(di.rgb, dSurf[0]), ratio(di.rgb, dSurf[1])),
    sourceContrastLight: Math.min(ratio(hexToRgb(lHex), lSurf[0]), ratio(hexToRgb(lHex), lSurf[1])),
    note: `沿 RGB 等比压暗直到压卡片/压画布两侧都 ≥4.5（浅色系数 ${li.k}，深色 ${di.k}；系数 1 表示官方值已达标）` })
}

/* 焦点指示物本身：1px 内描边用 brand 实色，这才是"唯一识别手段"那一条。 */
{
  const bL = hexToRgb(lightVal('brand')), bD = hexToRgb(nightVal('brand'))
  const sL = hexToRgb(lightVal('surface')), sD = hexToRgb(nightVal('surface'))
  const cl = ratio(bL, sL), cd = ratio(bD, sD)
  if (cl < 3 || cd < 3) throw new Error(`焦点描边对比不足：浅 ${cl} / 深 ${cd}（WCAG 1.4.11 要 ≥3.0）`)
  derived.push({ name: '(focus 描边)', kind: 'focusIndicator', uses: '--set-brand vs --set-surface',
    contrastLight: cl, contrastDark: cd, threshold: 3.0 })
}

/* placeholder：官方 apple-night 的 text-3(#6e6e73) 压在深色 surface-2(#26262a) 上
 * 实测 2.97，差 0.03 够不到本设计集自设的"提示文字 ≥3.0"线。
 * 两种修法都试过的话：① 放宽门槛迁就官方值 = 为了绿灯改判据，不做；
 * ② 深色改用 text-2 = 提示与正文同灰，层级塌了，也不做。
 * 这里取第三条：显式派生一个 placeholder 值，算式写死可复算 ——
 *   浅色 = 官方 text-3 原值（3.48 ✓，不偏离）
 *   深色 = 官方 text-3 与 text-2 的逐通道中点（把"提示"留在两级灰之间）
 * 派生结果不是上游值，所以标 kind:'derived'，门槛不达就抛错。 */
{
  const t3L = hexToRgb(lightVal('text-3')), t3D = hexToRgb(nightVal('text-3'))
  const t2D = hexToRgb(nightVal('text-2'))
  const f2 = (v) => Math.round(v).toString(16).padStart(2, '0')
  const midD = t3D.map((v, i) => Math.round((v + t2D[i]) / 2))
  const lVal = `#${t3L.map(f2).join('')}`
  const dVal = `#${midD.map(f2).join('')}`
  const s2L = hexToRgb(lightVal('surface-2')), s2D = hexToRgb(nightVal('surface-2'))
  const cl = ratio(t3L, s2L), cd = ratio(midD, s2D)
  if (cl < 3 || cd < 3) throw new Error(`placeholder 对比不足：浅 ${cl} / 深 ${cd}（门槛 3.0）`)
  lines.light.push(`  --set-placeholder: ${lVal};`)
  lines.dark.push(`  --set-placeholder: ${dVal};`)
  derived.push({ name: '--set-placeholder', kind: 'derived', threshold: 3.0,
    light: lVal, dark: dVal, contrastLight: cl, contrastDark: cd,
    note: `浅色=官方 text-3 原值；深色=官方 text-3(${nightVal('text-3')}) 与 text-2(${nightVal('text-2')}) 的逐通道中点，因为官方 text-3 在深色 surface-2 上只有 2.97` })
}

/* 材质条：不是"前景压背景"的对比问题。
 * 上游浅色 --set-material-bar = rgba(255,255,255,.72)，而浅色 --set-surface 就是
 * #ffffff —— 所以"官方 surface 同色 + .72"在浅色下**复现上游原值**（下面断言）。
 * 同一写法搬到深色若照抄白纱会变成"白压黑"，故深色用 rgba(官方 surface, .72)。
 * --set-material-edge 是**有记录的偏离**：上游 rgba(255,255,255,.6) 是 macOS 顶沿
 * 高亮，压在浅 chrome 上看不见分隔线；这里两套主题都取"压在 chrome 上可见的发丝线"。 */
for (const m of [
  { name: '--set-material-bar', alpha: 0.72, base: 'surface' },
  { name: '--set-material-edge', alpha: null, light: 'rgba(0, 0, 0, .08)', dark: 'rgba(255, 255, 255, .1)', note: '偏离上游浅色 rgba(255,255,255,.6)' },
]) {
  let lVal, dVal
  if (m.alpha !== null) {
    const sL = hexToRgb(lightVal(m.base)), sD = hexToRgb(nightVal(m.base))
    lVal = `rgba(${sL.join(', ')}, ${m.alpha})`
    dVal = `rgba(${sD.join(', ')}, ${m.alpha})`
    const upstream = lightVal('material-bar')
    if (m.name === '--set-material-bar') {
      /* 数值比较，不比字符串：上游写 .72，模板写 0.72，字符串比会误报。
       * 比的是「同一组 RGB + 同一个 alpha」，这才是"复现上游"的含义。 */
      const up = /rgba?\(([^)]+)\)/.exec(upstream)
      if (!up) throw new Error(`上游 --set-material-bar 不是 rgba()：${upstream}，算式假设不成立`)
      const parts = up[1].split(/[,\s/]+/).filter(Boolean).map(Number)
      const same = parts.length === 4 && parts[0] === sL[0] && parts[1] === sL[1] && parts[2] === sL[2]
        && Math.abs(parts[3] - m.alpha) < 1e-9
      if (!same) throw new Error(`派生式不再复现上游浅色 ${upstream}（算出 ${lVal}）——上游改了，算式要重看`)
    }
  } else { lVal = m.light; dVal = m.dark }
  lines.light.push(`  ${m.name}: ${lVal};`)
  lines.dark.push(`  ${m.name}: ${dVal};`)
  derived.push({ name: m.name, kind: 'material', light: lVal, dark: dVal, note: m.note ?? '同 alpha 压官方 surface（浅色档复现上游原值）' })
}

const HEADER = `/* ============================================================================
 * dsh_mobile 设计集 · 令牌（apple 风格）
 * ----------------------------------------------------------------------------
 * 本文件由 tools/gen-tokens.mjs 生成，请勿手改（改了会被下次生成覆盖）。
 *   浅色层  = ${PLUGIN}/styles/apple.css —— 逐字节原文
 *   深色层  = 同目录 styles/palettes.json 的 apple-night.tokens —— 逐值原文
 * 深色层挂在 html[data-theme="dark"] 与任意 [data-theme="dark"] 子树上，好让一页里
 * 并排对照两套主题。不在 design.json 声明 palette：插件的 palette 层注入到 <head>
 * 的 :root（index.js paletteStyleTag），会把所有页面统一压成深色。
 *
 * 末尾派生层不是上游值：算式、alpha、判据与实测对比都在 tools/derived.json，
 * 由 tools/check.mjs 复算。不用 color-mix()：vite build.target=chrome100，
 * color-mix 要 Chrome 111+，在 Android WebView 上整条声明会静默失效。
 * ========================================================================== */

`

const darkBlock = Object.entries(Object.fromEntries(official)).map(([k, v]) => `  ${k}: ${v};`).join('\n')

const EXTENSION = `
/* ---- 派生层（算式见 tools/derived.json，check.mjs 复算） ---- */
:root {
  /* 出处：apple.css 头部注释「控件 8、卡片 12、面板 18」——注释有值、令牌缺失 */
  --set-radius-panel: 18px;
  /* 出处：本设计集自加，上游没有胶囊档 */
  --set-radius-pill: 999px;
  /* 出处：本设计集自加，面板级抬起 */
  --set-shadow-lift: 0 2px 6px rgba(0, 0, 0, .06), 0 18px 40px -20px rgba(0, 0, 0, .18);
${lines.light.join('\n')}
}

html[data-theme="dark"],
[data-theme="dark"] {
  /* 深色下抬起阴影要更重，否则分层看不出来（浅色那档压黑底等于没有） */
  --set-shadow-lift: 0 2px 8px rgba(0, 0, 0, .5), 0 20px 44px -20px rgba(0, 0, 0, .7);
${lines.dark.join('\n')}
}
`

writeFileSync(OUT, `${HEADER}${appleCss.trimEnd()}\n\n/* ---- 深色：apple-night 官方值 ---- */\nhtml[data-theme="dark"],\n[data-theme="dark"] {\n${darkBlock}\n}\n${EXTENSION}`)
writeFileSync(resolve(HERE, 'derived.json'), JSON.stringify({
  note: '派生层算式与判据。tint=WCAG 1.4.3 文本 ≥4.5（会拦）；line/ring 只记录实测（发丝线按设计就是低对比，唯一标识另有语义色文字）；focusIndicator=1.4.11 非文本 ≥3.0（会拦）；material=断言浅色档复现上游原值（会拦）。',
  surfaceLight: lightVal('surface'), surfaceDark: nightVal('surface'),
  items: derived,
}, null, 2) + '\n')
console.log(`✅ ${OUT}`)
console.log(`   浅色 ${appleCss.length}B 原文 · 深色 ${official.length} 键官方 · 派生 ${derived.length} 条（含判据断言）`)
