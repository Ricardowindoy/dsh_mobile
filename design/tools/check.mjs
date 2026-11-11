#!/usr/bin/env node
/* =============================================================================
 * check.mjs —— 设计集闸门（会失败的，不是提示）
 * -----------------------------------------------------------------------------
 * 规范写在 docs/设计规范.md，但"写着"不等于"成立"。这里把能机械检查的十二条
 * 变成闸门；任何一条红就 exit 1。每条都做过变异检验（注入已知错误必须报红），
 * 没被咬过的规则只是装饰。下面这份清单与代码里 fail() 首参的取值一一对应，
 * 加了规则不更新这里，就等于文档在撒谎。
 *
 *   1. 结构            design/design.json 过插件自己的 validateManifest（不复制规则）；
 *                      entry / tokens / docs 指向的文件必须真的存在
 *   2. 令牌同源        tokens.css 浅色层逐字节 == styles/apple.css；
 *                      深色层逐值 == styles/palettes.json#apple-night；
 *                      深色块每个键都要有浅色基线（主题完整性）
 *   3. 派生复算        tools/derived.json 的每条算式独立重算（tint 取"压卡片/压画布"
 *                      较差一侧 ≥4.5；ink 重跑压暗算法；material 断言复现上游浅色）
 *   4. 令牌优先        <style>、内联 style="" 与 <script> 里都不许出现裸色值
 *   5. 令牌存在性      引用的 var(--set-*) 必须有声明（拼错会静默失效）
 *   6. 动效白名单      transition/@keyframes 只碰 transform/opacity（含 transition: all）
 *   7. 畸形声明        括号不配平 —— 浏览器会静默丢掉整条声明（不报错）
 *   8. 命中区          <html data-target="touch"> 的稿子里 min-height 必须 ≥44px
 *   9. 兼容底线        禁用 WebView（chrome100）上会静默失效的新特性（color-mix 等）
 *  10. 共享层硬规矩    base.css 必须常驻 [hidden]{display:none!important}
 *  11. 文字色白名单    color: 不许引用图形/弱档令牌（text-3、ok、warn、danger、brand）
 *  12. 死类            base.css 与页内 <style> 定义的类都必须真的被用到
 *
 * 用法：node design/tools/check.mjs
 * ========================================================================== */
import { readFileSync, readdirSync, existsSync } from 'node:fs'
import { resolve, dirname, join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const HERE = dirname(fileURLToPath(import.meta.url))
const DESIGN = resolve(HERE, '..')
const PLUGIN = process.env.UI_DESIGNER_ROOT
if (!PLUGIN) {
  console.error('需要 UI_DESIGNER_ROOT 环境变量（指向 dsh-ui-designer 插件根目录）')
  process.exit(2)
}

const failures = []
const notes = []

/* 规则清单是**唯一真值来源**：汇总行由它生成，fail() 也校验它。
 * 加一条规则忘了登记 → 闸门自己报错；汇总行也不会再和实际规则脱节
 * （上一轮漏登记「死类」就是手写清单漂了）。 */
const RULES = ['结构', '令牌同源', '主题完整性', '派生复算', '令牌优先', '令牌存在性',
  '动效白名单', '畸形声明', '命中区', '兼容底线', '共享层硬规矩', '文字色白名单', '死类']

const fail = (rule, detail) => {
  if (!RULES.includes(rule) && rule !== '闸门自身') {
    failures.push({ rule: '闸门自身', detail: `规则名 ${JSON.stringify(rule)} 不在 RULES 清单里（登记它或改正拼写）` })
  }
  failures.push({ rule, detail })
}

const read = (rel) => readFileSync(resolve(DESIGN, rel), 'utf8')
const pages = readdirSync(DESIGN).filter((f) => /\.html?$/i.test(f)).sort()

/* ── 1. 结构合法：直接调用插件的校验器，不在这里复制它的规则 ────────────── */
const setPath = join(PLUGIN, 'design-set.js')
if (!existsSync(setPath)) {
  fail('闸门自身', `找不到插件校验器 ${setPath}（用 UI_DESIGNER_ROOT 指定插件仓库）`)
} else {
  try {
    const mod = await import(pathToFileURL(setPath).href)
    const manifest = JSON.parse(read('design.json'))
    const verdict = mod.validateManifest(manifest)
    for (const e of verdict.errors ?? []) fail('结构', `design.json → ${e.code}: ${e.message}（修法：${e.fix}）`)
    for (const w of verdict.warnings ?? []) notes.push(`结构警告：${w.code} ${w.message}`)
    if (verdict.errors?.length === 0) notes.push(`结构：design.json 通过插件 validateManifest（style=${manifest.style} palette=${manifest.palette ?? '—'}）`)
    // entry 与 tokens 指向的文件必须真的在
    for (const key of ['entry', 'tokens']) {
      const v = manifest[key]
      if (typeof v === 'string' && v !== '' && !existsSync(resolve(DESIGN, v))) fail('结构', `manifest.${key} 指向的文件不存在：${v}`)
    }
    for (const d of manifest.docs ?? []) {
      if (!existsSync(resolve(DESIGN, d))) fail('结构', `docs 里的文件不存在：${d}`)
    }
  } catch (e) {
    fail('闸门自身', `校验 design.json 失败：${e.message}`)
  }
}

/* ── 2. 令牌同源：tokens.css 必须由上游生成，不许手改 ─────────────────── */
try {
  const tokens = read('tokens.css')
  const apple = readFileSync(join(PLUGIN, 'styles', 'apple.css'), 'utf8')

  /* 定位 :root 块必须用"行首锚定"：注释里也会出现 ":root" 这个字串
   * （本文件与生成器的头注释都有），不锚定就会命中注释、然后一路找到
   * 下一个 } —— 上一版就是这么误报"浅色层不一致"的。 */
  const blockOf = (css) => {
    const m = /(^|\n):root\s*\{[\s\S]*?\n\}/.exec(css)
    return m ? m[0].trim() : ''
  }
  const lightBlock = blockOf(tokens)
  const appleBlock = blockOf(apple)
  if (lightBlock === '' || appleBlock === '') {
    fail('闸门自身', `找不到 :root 块（tokens.css=${lightBlock === '' ? '缺' : '有'} / apple.css=${appleBlock === '' ? '缺' : '有'}）`)
  } else if (lightBlock !== appleBlock) {
    fail('令牌同源', 'tokens.css 的浅色层与 styles/apple.css 不一致（跑 tools/gen-tokens.mjs 重新生成，别手改）')
  } else notes.push('令牌同源：浅色层逐字节 == styles/apple.css')

  const night = JSON.parse(readFileSync(join(PLUGIN, 'styles', 'palettes.json'), 'utf8'))
    .palettes.find((p) => p?.id === 'apple-night')
  /* 必须先剥注释再解析：注释里也会出现 "data-theme=\"dark\"" 与 ":root" 这些字串，
   * 不剥就会命中注释、再往后找到下一个 {，把浅色值当成深色值比 —— 上一版正是
   * 这么误报"浅色层不一致"的（假警报比没有闸门更糟）。 */
  const clean = tokens.replace(/\/\*[\s\S]*?\*\//g, '')
  const norm = (s) => String(s).toLowerCase().replace(/\s+/g, '')

  /* 按选择器把 tokens.css 拆成声明块：派生值分布在第二个 :root 与第二个
   * [data-theme="dark"] 里，只取第一个块会漏掉派生条目（最需要盯的部分）。 */
  const lightDeclared = {}, darkDeclared = {}
  for (const m of clean.matchAll(/(^|\n)\s*(:root|html\[data-theme="dark"\],\s*\[data-theme="dark"\])\s*\{([\s\S]*?)\n\}/g)) {
    const target = m[2] === ':root' ? lightDeclared : darkDeclared
    for (const d of m[3].matchAll(/(--set-[\w-]+)\s*:\s*([^;]+);/g)) target[d[1]] = d[2].trim()
  }

  let mismatch = 0
  for (const [k, v] of Object.entries(night.tokens)) {
    if (darkDeclared[k] === undefined) { fail('令牌同源', `深色层缺 apple-night 的 ${k}`); mismatch++; continue }
    /* 精确比较。旧版用 startsWith 截 7 个字符，那样 --set-shadow 这类长值
     * 只要开头一样就放行，等于没验。 */
    if (norm(darkDeclared[k]) !== norm(v)) { fail('令牌同源', `深色层 ${k}=${darkDeclared[k]} ≠ apple-night ${v}`); mismatch++ }
  }
  if (mismatch === 0) notes.push(`令牌同源：深色层 ${Object.keys(night.tokens).length} 个键逐值 == palettes.json#apple-night`)

  /* 深色有、浅色没有 = 浅色模式下这个 var() 解析为空，整条声明静默失效。
   * 反过来（只在 :root 声明）是合法的：主题不变的值不需要深色覆盖。
   * 这条是 M4 变异检验逼出来的：删掉浅色的 --set-ring 后闸门仍然放行，
   * 因为"令牌存在性"只看名字在不在，而名字在深色块里也算在。 */
  const orphanDark = Object.keys(darkDeclared).filter((k) => lightDeclared[k] === undefined)
  if (orphanDark.length) fail('主题完整性', `深色块声明了、浅色块没有（浅色模式会解析为空）：${orphanDark.join(', ')}`)
  else notes.push(`主题完整性：深色块 ${Object.keys(darkDeclared).length} 个键都有浅色基线`)

  /* ── 2b. 派生层复算 ────────────────────────────────────────────────
   * 注释里写着"check.mjs 复算"，那就真的复算：用同一套颜色数学独立重算
   * derived.json 的每条算式，再和 tokens.css 里实际声明的值对。
   * 空头承诺比不写更糟 —— 它会让后面的人以为已经验过了。 */
  const hexRgb = (h) => { const m = /^#?([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(String(h).trim()); return m ? [1, 2, 3].map((i) => parseInt(m[i], 16)) : null }
  const lin = (v) => { const x = v / 255; return x <= 0.04045 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4 }
  const lumC = (c) => 0.2126 * lin(c[0]) + 0.7152 * lin(c[1]) + 0.0722 * lin(c[2])
  const rat = (a, b) => { const l1 = lumC(a), l2 = lumC(b); return +(((Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05)).toFixed(2)) }
  const ovr = (fg, al, bg) => fg.map((v, i) => v * al + bg[i] * (1 - al))
  const lightSrc = {}
  for (const m of apple.replace(/\/\*[\s\S]*?\*\//g, '').matchAll(/(--set-[\w-]+)\s*:\s*([^;]+);/g)) lightSrc[m[1]] = m[2].trim()
  const darkSrc = Object.fromEntries(Object.entries(night.tokens).map(([k, v]) => [k, String(v).trim()]))

  try {
    const dj = JSON.parse(read('tools/derived.json'))
    let bad = 0
    for (const it of dj.items) {
      if (it.kind === 'focusIndicator') {
        for (const [side, src, rec] of [['浅', lightSrc, it.contrastLight], ['深', darkSrc, it.contrastDark]]) {
          const got = rat(hexRgb(src['--set-brand']), hexRgb(src['--set-surface']))
          if (got !== rec) { fail('派生复算', `焦点描边 ${side}色记录 ${rec}，复算 ${got}`); bad++ }
          if (got < it.threshold) { fail('派生复算', `焦点描边 ${side}色 ${got} < ${it.threshold}（WCAG 1.4.11）`); bad++ }
        }
        continue
      }
      for (const [side, src, declared, alpha, expect] of [
        ['浅', lightSrc, lightDeclared, it.alphaLight, it.light],
        ['深', darkSrc, darkDeclared, it.alphaDark, it.dark],
      ]) {
        if (it.kind === 'material') {
          if (it.name === '--set-material-bar') {
            const s = hexRgb(src['--set-surface'])
            const want = `rgba(${s.join(', ')}, 0.72)`
            if (norm(want) !== norm(expect)) { fail('派生复算', `${it.name} ${side}色记 ${expect}，算式应为 ${want}`); bad++ }
          }
          // edge 是"有记录的偏离"，只要求 tokens.css 与清单一致
        } else if (it.kind === 'derived') {
          /* placeholder 的算式两套不对称（这是刻意的）：
             浅色 = 官方 text-3 原值（本来就 ≥3.0，不必偏离上游）
             深色 = 官方 text-3 与 text-2 的逐通道中点（官方 text-3 只有 2.97） */
          const t3 = hexRgb(src['--set-text-3'])
          const use = side === '浅' ? t3 : t3.map((v, i) => Math.round((v + hexRgb(src['--set-text-2'])[i]) / 2))
          const want = `#${use.map((v) => v.toString(16).padStart(2, '0')).join('')}`
          if (norm(want) !== norm(expect)) { fail('派生复算', `${it.name} ${side}色记 ${expect}，算式应为 ${want}`); bad++ }
          const got = rat(use, hexRgb(src['--set-surface-2']))
          if (got < it.threshold) { fail('派生复算', `${it.name} ${side}色对比 ${got} < ${it.threshold}`); bad++ }
          if (got !== (side === '浅' ? it.contrastLight : it.contrastDark)) { fail('派生复算', `${it.name} ${side}色对比记录不符（复算 ${got}）`); bad++ }
        } else if (it.kind === 'ink') {
          /* 文字档：独立重跑"沿 RGB 等比压暗直到压卡片/压画布两侧都 ≥4.5"。
           * 不复用生成器给的系数——两边各自算一遍还能对上，才算真的复算。 */
          const inkRecompute = (srcHex, surfs) => {
            const b = hexRgb(srcHex)
            if (!b) return null
            for (let k = 1; k >= 0.2; k = +(k - 0.02).toFixed(4)) {
              const c = b.map((v) => Math.round(v * k))
              if (surfs.every((s) => rat(c, s) >= 4.5)) return { hex: `#${c.map((v) => v.toString(16).padStart(2, '0')).join('')}`, k }
            }
            return null
          }
          const surfs = [hexRgb(src['--set-surface']), hexRgb(src['--set-bg'])]
          const officialHex = src[`--set-${it.sourceKey}`]
          if (surfs.some((s) => !s) || !officialHex) { fail('派生复算', `${it.name} 的 ${side}色源缺失或非 #rrggbb`); bad++; continue }
          const got = inkRecompute(officialHex, surfs)
          if (!got) { fail('派生复算', `${it.name} ${side}色压暗到 0.2 仍不达 4.5，算法需重设计`); bad++ }
          else {
            if (norm(got.hex) !== norm(expect)) { fail('派生复算', `${it.name} ${side}色记 ${expect}，复算 ${got.hex}（系数 ${got.k}）`); bad++ }
            if (got.k !== (side === '浅' ? it.lightScale : it.darkScale)) { fail('派生复算', `${it.name} ${side}色系数记录不符（复算 ${got.k}）`); bad++ }
            const worst = Math.min(...surfs.map((s) => rat(hexRgb(got.hex), s)))
            if (worst < 4.5) { fail('派生复算', `${it.name} ${side}色最差侧 ${worst} < 4.5`); bad++ }
            /* ink 存在的唯一理由是"官方值做文字不达标"。官方值本来就够却压暗，
             * 就是无谓偏离上游 —— 这条判据防止 ink 层悄悄膨胀。 */
            const offWorst = Math.min(...surfs.map((s) => rat(hexRgb(officialHex), s)))
            if (got.k === 1 && offWorst < 4.5) { fail('派生复算', `${it.name} ${side}色系数记 1 但官方值最差只有 ${offWorst}`); bad++ }
            if (got.k !== 1 && offWorst >= 4.5) { fail('派生复算', `${it.name} ${side}色官方值已达标（${offWorst}），不该压暗`); bad++ }
          }
        } else {
          const fg = hexRgb(src[`--set-${it.key}`]), surf = hexRgb(src['--set-surface']), canvas = hexRgb(src['--set-bg'])
          if (!fg || !surf || !canvas) { fail('派生复算', `${it.name} 的 ${side}色源不是 #rrggbb`); bad++; continue }
          const want = `rgba(${fg.join(', ')}, ${alpha})`
          if (norm(want) !== norm(expect)) { fail('派生复算', `${it.name} ${side}色记 ${expect}，算式应为 ${want}`); bad++ }
          if (it.kind === 'tint') {
            // 与生成器同一判据：取"压卡片 / 压画布"较差的一侧
            const worst = Math.min(rat(fg, ovr(fg, alpha, surf)), rat(fg, ovr(fg, alpha, canvas)))
            if (worst < 4.5) { fail('派生复算', `${it.name} ${side}色染底文字对比 ${worst} < 4.5`); bad++ }
            if (worst !== (side === '浅' ? it.textOnTintLight : it.textOnTintDark)) { fail('派生复算', `${it.name} ${side}色对比记录不符（复算 ${worst}）`); bad++ }
          }
        }
        // 清单对了但 tokens.css 被手改，同样要红
        if (declared[it.name] !== undefined && norm(declared[it.name]) !== norm(expect)) {
          fail('派生复算', `tokens.css 的 ${it.name}（${side}）= ${declared[it.name]}，与 derived.json 的 ${expect} 不符`); bad++
        }
      }
    }
    if (bad === 0) notes.push(`派生复算：${dj.items.length} 条算式独立复算一致（tint 取压卡片/压画布较差一侧）`)
  } catch (e) {
    fail('闸门自身', `读 tools/derived.json 失败：${e.message}`)
  }
} catch (e) {
  fail('闸门自身', `读 tokens.css 失败：${e.message}`)
}

/* ── 3–6. 逐文件检查（页面 + 组件层 base.css）─────────────────────────
 * base.css 必须一起扫：它是所有页面都链接的层，不扫就等于留了个后门
 * ——在页面里禁止裸色值，却允许组件层随便写死。 */
const COLOR_NAME = /\b(red|blue|green|orange|purple|yellow|pink|brown|gray|grey|cyan|magenta|lime|navy|teal|olive|white|black|silver|gold)\b/
const declaredTokens = new Set()
try {
  /* 声明源只有 tokens.css。base.css 不许声明 --set-*：那是调色板的唯一出口，
   * 组件层偷偷补一个值，换 palette 时就不会跟着变。 */
  for (const m of read('tokens.css').matchAll(/(--set-[\w-]+)\s*:/g)) declaredTokens.add(m[1])
} catch { /* 上面已报 */ }

const inlineStyles = (html) => [...html.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/gi)].map((m) => m[1])

const filesToScan = [...pages.map((p) => ({ name: p, kind: 'page' })), { name: 'base.css', kind: 'css' }]
for (const target of filesToScan) {
  const page = target.name
  const src = read(target.name)
  let html = '', css = ''
  if (target.kind === 'page') {
    html = src
    css = inlineStyles(html).join('\n')
  } else {
    css = src
  }
  const cssNoComments = css.replace(/\/\*[\s\S]*?\*\//g, '')
  /* 内联 style="…" 也算：只查 <style> 的话，把裸色写进 style 属性就能绕过闸门。 */
  const inlineAttrs = [...html.matchAll(/\sstyle="([^"]*)"/gi)].map((m) => m[1]).join(';')
  const scanCss = cssNoComments + '\n' + inlineAttrs.replace(/\/\*[\s\S]*?\*\//g, '')

  /* <script> 里的裸色单独判，且只认"颜色赋值"语境。
   * 不能整段扫：querySelector('#add')、'#feed'、'#aced' 这类 id 选择器
   * 本身就是合法十六进制字面量，整段扫会大量误报。
   * 认三种形态：style.<色属性> = '#…'、setProperty('<色属性>', '#…')、
   * cssText/模板里写 `color: #…`。 */
  const scriptBodies = [...html.matchAll(/<script(?![^>]*\ssrc=)[^>]*>([\s\S]*?)<\/script>/gi)].map((m) => m[1]).join('\n')
    .replace(/\/\/[^\n]*/g, '').replace(/\/\*[\s\S]*?\*\//g, '')
  const COLOR_PROP = '(?:color|backgroundColor|background|borderColor|borderTopColor|borderBottomColor|outlineColor|fill|stroke|boxShadow)'
  const jsColorRaw = [
    /* 分隔符必须同时认 `=` 和 `,`：
     *   el.style.color = '#fff'            ← 赋值，最常见
     *   setProperty('color', '#fff')       ← 逗号
     * 上一版只写了可选逗号、没写等号，结果 `style.color = '#ff0000'` 整类漏检
     * （变异检验里注入了 3 处只抓到 2 处，差点当成规则正常）。 */
    ...scriptBodies.matchAll(new RegExp(`(?:style\\s*\\.\\s*${COLOR_PROP}|setProperty\\s*\\(\\s*["']${COLOR_PROP}["'])\\s*(?:[=,]|:)\\s*(?::\\s*)?["'\`]([^"'\`]+)["'\`]`, 'gi')),
    ...scriptBodies.matchAll(new RegExp(`(?:style\\.cssText|cssText\\s*=)\\s*["'\`]([^"'\`]+)["'\`]`, 'gi')),
    ...scriptBodies.matchAll(new RegExp(`(?:${COLOR_PROP})\\s*:\\s*([^;"'\`]+)`, 'gi')),
  ].map((m) => m[1]).filter((v) => /#[0-9a-fA-F]{3,8}\b|\brgba?\s*\(|\bhsla?\s*\(|\b(red|blue|green|white|black|gray|grey)\b/.test(v))
  const jsColorHits = [...new Set(jsColorRaw)]
  for (const v of jsColorHits) {
    fail('令牌优先', `${page} 的 <script> 里给颜色写了字面值：${String(v).trim().slice(0, 44)} → 改成切换类名，颜色仍由 tokens.css 决定`)
  }
  const svgAttrs = [...html.matchAll(/<(?:svg|path|circle|rect|line|polyline|polygon)[^>]*>/gi)].map((m) => m[0]).join('\n')
  for (const [what, re] of [['hex 色值', /#[0-9a-fA-F]{3,8}\b/g], ['rgb()/rgba()', /\brgba?\s*\(/g], ['hsl()/hwb()', /\bhsla?\s*\(|\bhwb\s*\(/g]]) {
    const hits = [...scanCss.matchAll(re)]
    if (hits.length) fail('令牌优先', `${page} 里有 ${hits.length} 处裸 ${what}（含内联 style；例：${scanCss.slice(hits[0].index, hits[0].index + 40).replace(/\s+/g, ' ')}）`)
  }
  for (const m of scanCss.matchAll(/(?:^|[;{\s])(color|background|border-color|fill|stroke)\s*:\s*([^;}]+)/gmi)) {
    const val = m[2]
    if (COLOR_NAME.test(val) && !/var\(/.test(val)) fail('令牌优先', `${page} 用了颜色关键字：${m[1]}: ${val.trim().slice(0, 40)}`)
  }
  // SVG 的 fill/stroke 允许（图标是资产不是主题色），但页面若给图标写死颜色也记一笔
  const svgColorHits = [...svgAttrs.matchAll(/(?:fill|stroke)="(?!none|currentColor)([^"]+)"/g)]
  if (svgColorHits.length) notes.push(`${page}: ${svgColorHits.length} 处 SVG 图标写死颜色（图标资产可接受，不判失败）`)

  // 4. 引用的令牌必须存在（去重：同一个拼错的令牌只报一次）
  const missing = new Set()
  for (const m of (html + css).matchAll(/var\((--set-[\w-]+)/g)) {
    if (!declaredTokens.has(m[1])) missing.add(m[1])
  }
  for (const name of missing) fail('令牌存在性', `${page} 引用了未声明的 ${name}`)

  // 5. 动效只碰 transform/opacity
  const TRANSITION_BANNED = ['width', 'height', 'margin', 'padding', 'top', 'left', 'right', 'bottom', 'font-size', 'border-width']
  for (const m of cssNoComments.matchAll(/(?:^|[;{\s])(transition)\s*:\s*([^;}]+)/gmi)) {
    const val = m[2].toLowerCase()
    // `transition: all …` 也算越界：它必然连带尺寸属性
    const bad = TRANSITION_BANNED.filter((p) => new RegExp(`(^|[\\s,])${p}\\b`).test(val))
    if (bad.length) fail('动效白名单', `${page} transition 动了尺寸/位置类属性：${bad.join(', ')}（${val.trim().slice(0, 50)}）`)
    if (/^\s*all\b/.test(val)) fail('动效白名单', `${page} 用 transition: all（会顺带动画尺寸）：${val.trim().slice(0, 50)}`)
  }
  for (const m of cssNoComments.matchAll(/@keyframes\s+[\w-]+\s*\{/g)) {
    // 取这个 keyframes 的整段（按花括号配平，不能靠 "\n}" —— 单行写法会漏检）
    let i = m.index + m[0].length, depth = 1, body = ''
    while (i < cssNoComments.length && depth > 0) {
      const ch = cssNoComments[i]
      if (ch === '{') depth++
      else if (ch === '}') { depth--; if (depth === 0) break }
      body += ch
      i++
    }
    const bad = ['width', 'height', 'margin', 'padding', 'top', 'left', 'bottom', 'right'].filter((p) =>
      new RegExp(`(^|[;{\\s])${p}\\s*:`).test(body))
    if (bad.length) fail('动效白名单', `${page} 的 @keyframes ${m[0].match(/[\w-]+(?=\s*\{)/)[0]} 动画了 ${bad.join('/')}（只许 transform/opacity）`)
  }

  /* 6. 畸形声明：括号不配平时浏览器会**静默丢掉整条声明**（不报错），症状是
   * "样式明明写了却没生效"。这一类是并行 agent 在 base.css 里发现的
   * （`box-shadow: var(--set-shadow-lift));` 多一个右括号，抬起阴影从未生效），
   * 而当时六条规则全部放行 —— 说明缺的就是这条。
   * 只扫 css（页面取 <style> 块、base.css 取全文）：正文里用 <code> 描述这个
   * bug 属于文档，不该被当成代码报错。url() 先中和掉，因为 data URI 里的
   * 分号是合法的，不中和会误报。 */
  {
    const stripped = cssNoComments
      .replace(/(["'])(?:\\.|(?!\1)[^\\])*\1/g, '""')
      .replace(/url\((?:[^()]|\([^)]*\))*\)/gi, 'url()')
    let depth = 0, start = 0, malformed = 0
    for (let i = 0; i < stripped.length; i++) {
      const c = stripped[i]
      if (c === '(') { depth++; continue }
      if (c === ')') {
        depth--
        if (depth < 0) {
          fail('畸形声明', `${page} 括号不配平（浏览器会丢掉整条声明）：…${stripped.slice(Math.max(0, start - 20), i + 1).replace(/\s+/g, ' ').trim()}`)
          malformed++; depth = 0; start = i + 1
        }
        continue
      }
      if (c === ';' || c === '}' || c === '{') {
        if (depth > 0) {
          fail('畸形声明', `${page} 声明结束仍有未闭合括号：${stripped.slice(start, i + 1).replace(/\s+/g, ' ').trim().slice(0, 60)}`)
          malformed++
        }
        depth = 0; start = i + 1
      }
    }
    if (malformed === 0 && target.kind === 'css') notes.push('畸形声明：base.css 括号配平')
  }

  /* 8. 兼容底线：这套稿子最终落进 Android WebView（vite build.target=chrome100）。
   * 下面这些特性在低版本上是**整条声明静默失效**——不报错、不留痕，最难回头查。
   * 只扫剥完注释的 css：base.css 的注释里就在解释"不用 color-mix()"，
   * 不剥注释会把自己的说明当违规（上一轮 grep 就是这么误判的）。 */
  const BANNED_FEATURES = [
    ['color-mix(', 'Chrome 111+', '用 tools/gen-tokens.mjs 预先把混色算成显式 rgba()'],
    ['corner-shape', 'Chrome 139+', '整圆直接用 border-radius: 50%'],
    ['@container', 'Chrome 105+', '改用媒体查询'],
    ['text-wrap: balance', 'Chrome 114+', '用 max-width 控制换行'],
  ]
  for (const [needle, since, alt] of BANNED_FEATURES) {
    const idx = cssNoComments.indexOf(needle)
    if (idx >= 0) {
      fail('兼容底线', `${page} 用了 ${needle}（需 ${since}；WebView 上整条声明静默失效）→ ${alt}。命中处：…${cssNoComments.slice(Math.max(0, idx - 20), idx + 40).replace(/\s+/g, ' ').trim()}`)
    }
  }

  /* 7. 触屏命中区：判据只看 <html> 标签上的声明，不做整页子串扫描。
   * 上一版用 /data-target="touch"/.test(html) 扫全文，结果注释或正文里只要
   * 写出这个字面量，桌面稿就被误判成触屏稿、合法的 min-height:28px 全数报红
   * （并行 agent 实际踩到并绕开了）。"目标设备"是文档属性，不是文内提及。 */
  const htmlTag = /<html\b[^>]*>/i.exec(html)?.[0] ?? ''
  const isTouch = /\bdata-target="touch"/.test(htmlTag)
  if (isTouch) {
    for (const m of cssNoComments.matchAll(/min-height\s*:\s*(\d+(?:\.\d+)?)px/g)) {
      const v = parseFloat(m[1])
      if (v < 44) fail('命中区', `${page} 的 <html> 声明了 data-target="touch" 却有 min-height:${v}px（触屏要求 ≥44px）`)
    }
    for (const m of html.matchAll(/<(?:button|a|input)[^>]*class="[^"]*\bbtn\b[^"]*"[^>]*>/gi)) {
      if (!/btn--touch|min-height/.test(m[0])) notes.push(`${page}: 有 .btn 未标 .btn--touch（触屏稿请显式加）`)
    }
  } else if (/data-target="touch"/.test(html)) {
    notes.push(`${page}: 正文提到 data-target="touch" 但 <html> 未声明（按桌面档 28px 判）`)
  }

  /* 10. 文字色白名单：color: 只许引用"为文字而生"的令牌。
   * 图形/弱档令牌压表面都够不到 AA 的 4.5:1（实测）：
   *   --set-ok 浅 4.14 · --set-brand 浅压画布 4.31 · --set-text-3 浅 3.62 / 深 3.36
   * 它们当点子、描边、填充是合格的（非文本 ≥3:1），当小字就不合格。
   * 文字走 --set-text / --set-text-2 / --set-*-ink（ink 由生成器压暗算出，
   * 且断言过"压卡片与压画布两侧都 ≥4.5"）。
   * 这条是浏览器探针逼出来的：01/02 的 .field__help 与 th 用 text-3，两套主题都红，
   * 而前面十条文本规则全绿 —— 文本规则看不见"用了合规令牌但不适合当文字"。 */
  const TEXT_FORBIDDEN = ['--set-text-3', '--set-ok', '--set-warn', '--set-danger', '--set-brand']
  const textHits = new Set()
  for (const m of scanCss.matchAll(/(?:^|[;{\s])color\s*:\s*([^;}]+)/gi)) {
    const val = m[1]
    for (const tok of TEXT_FORBIDDEN) {
      if (new RegExp(`var\\(\\s*${tok}\\s*\\)`).test(val)) textHits.add(`${tok}（用在 ${val.trim().slice(0, 30)}）`)
    }
  }
  for (const h of textHits) {
    fail('文字色白名单', `${page} 把图形/弱档令牌当文字色：${h} → 换 --set-text / --set-text-2 / --set-*-ink；图形色只给点、描边、填充`)
  }

  /* 11b. 页内 <style> 也不许有死类：只查 base.css 的话，页面自己定义完、
   * 后来删掉用法的类会漏网（我删 .row--busy 的用法时就留下过孤儿定义）。
   * 判据：类名在 <style> 之外是否还出现 —— HTML 的 class 属性与 JS 模板字符串里
   * 拼出的类名（如 ' ep--on'）都算用到，因为运行时真的会产生它。 */
  if (target.kind === 'page') {
    const ownDefs = new Set([...cssNoComments.matchAll(/\.([a-z][\w-]*)/g)].map((m) => m[1]))
    /* 判存在用"切词集合"，不用拼字符串的正则：上一版写
     * new RegExp('\\\\b' + c + '\\\\b')，在 JS 源码里那是「字面反斜杠 + b」，
     * 永远匹配不上，于是四个页面里上百个正常使用的类全被误报成死类。
     * 切词按所有非类名字符断开：class="a b"、JS 模板里的 ' ep--on' 都会成为 token。 */
    const outsideStyle = src.replace(/<style[\s\S]*?<\/style>/gi, () => ' ')
    const tokens = new Set(outsideStyle.split(/[^A-Za-z0-9_-]+/).filter(Boolean))
    const ownDead = [...ownDefs].filter((c) => !tokens.has(c)).sort()
    if (ownDead.length) fail('死类', `${page} 的 <style> 里定义了却没在 HTML/JS 中出现：${ownDead.join(', ')} → 用上它或删掉`)
  }
}

/* ── 9. 共享层必须自带本仓的强制规矩 ──────────────────────────────────
 * base.css 是所有页面共用的层，它漏一条就等于所有稿子一起漏，而且漏的是
 * "静默出错"那一类（不报错、只是画多了东西）。本仓 README「构建与发布约定」把
 * `[hidden]{display:none!important}` 定为常驻硬规矩：#setup/#status 都是
 * display:flex 的容器，缺这条两个界面会同时渲染。
 * 这条规则是真实踩出来的：稿子首屏挂了一个空的错误条，DOM 里 hidden=true
 * 而计算样式是 display:flex —— 前七条文本规则全部放行。 */
try {
  const base = read('base.css')
  if (!/\[hidden\]\s*\{\s*display\s*:\s*none\s*!important/.test(base)) {
    fail('共享层硬规矩', 'base.css 缺 `[hidden] { display: none !important; }`（见 README「构建与发布约定」：带 hidden 的 flex 容器照样会被画出来）')
  } else notes.push('共享层硬规矩：base.css 常驻 [hidden]{display:none!important}')
} catch (e) {
  fail('闸门自身', `读 base.css 失败：${e.message}`)
}

/* ── 11. 组件层不许有死类 ────────────────────────────────────────────
 * base.css 定义的类必须至少被一个页面用到。理由与"令牌必须被消费"完全一样：
 * 定义了却没人用的类是装饰 —— 读代码的人会以为存在某种约定，而它从没被渲染验证过。
 * 这条是我自己踩出来的：为了"补动效"我往 base.css 加了 stagger / sheet-in /
 * fade-in / swap 四个类，结果一个都没被引用，正是我批评并行 agent 的那个毛病。 */
try {
  const baseClean = read('base.css').replace(/\/\*[\s\S]*?\*\//g, '')
  const defined = new Set([...baseClean.matchAll(/\.([a-z][a-z0-9_-]*)/g)].map((m) => m[1]))
  const used = new Set()
  for (const p of pages) {
    const s = read(p)
    for (const m of s.matchAll(/class="([^"]*)"/g)) m[1].split(/\s+/).forEach((c) => used.add(c))
    for (const m of s.matchAll(/className="([^"]*)"/g)) m[1].split(/\s+/).forEach((c) => used.add(c))
    for (const m of s.matchAll(/classList\.(?:add|remove|toggle)\(\s*['"]([^'"]+)/g)) used.add(m[1])
  }
  const dead = [...defined].filter((c) => !used.has(c)).sort()
  if (dead.length) fail('死类', `base.css 定义了但没有任何页面用到：${dead.join(', ')} → 要么在页面里真正使用，要么删掉（留着等于撒谎）`)
  else notes.push(`死类：base.css 的 ${defined.size} 个类全部被页面引用`)
} catch (e) {
  fail('闸门自身', `死类检查失败：${e.message}`)
}

/* ── 汇总 ────────────────────────────────────────────────────────────── */
for (const n of notes) console.log(`· ${n}`)
if (failures.length) {
  console.error(`\n❌ ${failures.length} 项失败：`)
  for (const f of failures) console.error(`  [${f.rule}] ${f.detail}`)
  process.exit(1)
}
console.log(`\n全部通过 ✅ ${RULES.join(' / ')}（${pages.length} 页）`)
