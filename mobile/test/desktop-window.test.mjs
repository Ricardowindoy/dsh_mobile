// test/desktop-window.test.mjs —— 桌面窗口形态的契约闸门。
//
// 为什么需要它（2026-11-16 实测的故障）：
//   桌面端跑的是 github/windows 分支构建的安装包，那个分支的
//   tauri.windows.conf.json 写了 maximizable:false + width:420/height:760。
//   两件事同时发生：
//     ① tao 的 Windows 样式构造只在 WindowFlags::MAXIMIZABLE 置位时才加
//        WS_MAXIMIZEBOX（tao-0.37.2 window_state.rs:247）→ 系统标题栏**没有最大化按钮**；
//     ② 窗口钉死在 420 宽 → 内嵌的 DSH GUI 视口 420 < 宿主抽屉断点 768
//        → GUI 整体渲染成手机档，桌面端看起来跟手机一模一样。
//   relay 日志实测印证：Windows 设备 vv=420x760 dpr=2（27 次 boot 全是这个尺寸）。
//
// 闸门断言的是**配置与前端契约**，不是"跑起来看着对"：
//   G1 桌面平台配置存在，且 maximizable !== false（缺按钮的根因）
//   G2 桌面窗口宽度 ≥ 桌面断点（否则 GUI 仍是手机档）
//   G3 桌面配置不含把窗口钉回 420/760 的字面量
//   G4 前端按平台给设备名，桌面不再自称 Android
//   G5 前端在桌面保留「相册/粘贴」、只收掉「相机扫码」
//   G6 桌面不隐藏「手动连接」分区标题（桌面与移动端界面一致）
//   G7 CSS 有 ≥700px 的桌面档，且该档把主区放宽到 560
//   G8 窄屏档不受影响（420 档仍是 460 主区）
//
// 变异检验（闸门必须能红）：每个用例都有对应的 mutant 注入，见同目录 README。
import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, existsSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = dirname(fileURLToPath(import.meta.url))
const MOBILE = resolve(HERE, '..')          // .../mobile
const read = (rel) => readFileSync(join(MOBILE, rel), 'utf8')
const readJson = (rel) => JSON.parse(read(rel))

/** 桌面三平台：Windows / macOS / Linux 各自的 tauri 平台配置（相对 mobile/ 根）。 */
const DESKTOP_TARGETS = [
  { platform: 'windows', file: 'src-tauri/tauri.windows.conf.json' },
  { platform: 'macos', file: 'src-tauri/tauri.macos.conf.json' },
  { platform: 'linux', file: 'src-tauri/tauri.linux.conf.json' },
]

/** 宿主 DSH GUI 的抽屉断点：视口窄于此值就渲染手机档。 */
const GUI_DRAWER_BREAKPOINT = 768
/** 连接页自己的桌面断点（CSS 媒体查询）：低于它仍是手机排版。 */
const CSS_DESKTOP_BREAKPOINT = 700

test('G1/G2/G3 桌面平台配置：可最大化、宽度越过 GUI 抽屉断点、不钉回 420×760', () => {
  for (const { platform, file } of DESKTOP_TARGETS) {
    const path = join(MOBILE, file)
    assert.ok(existsSync(path), `${platform}: 缺少平台配置 ${file}（桌面端会退回主配置的 420×760）`)
    const cfg = readJson(file)
    const win = cfg?.app?.windows?.[0]
    assert.ok(win, `${platform}: ${file} 没有 app.windows[0]`)

    // G1 根因：maximizable 不得为 false（tao 据此决定是否加 WS_MAXIMIZEBOX）。
    assert.notEqual(win.maximizable, false,
      `${platform}: maximizable:false → 系统标题栏没有最大化按钮（tao window_state.rs:247 只在 MAXIMIZABLE 置位时加 WS_MAXIMIZEBOX）`)

    // G2 宽度必须越过宿主抽屉断点，否则内嵌 GUI 仍是手机档。
    assert.ok(Number(win.width) >= GUI_DRAWER_BREAKPOINT,
      `${platform}: width=${win.width} < ${GUI_DRAWER_BREAKPOINT} → 内嵌 DSH GUI 仍渲染手机抽屉档`)

    // G3 不得出现把窗口钉回手机尺寸的字面量。
    const raw = read(file)
    assert.ok(!/"width"\s*:\s*420/.test(raw), `${platform}: 配置里仍有 width:420`)
    assert.ok(!/"height"\s*:\s*760/.test(raw), `${platform}: 配置里仍有 height:760`)

    // 桌面窗口仍可缩放、可最小化，且居中启动。
    assert.equal(win.resizable, true, `${platform}: 桌面窗口必须可缩放`)
    assert.notEqual(win.minimizable, false, `${platform}: 桌面窗口必须可最小化`)
    assert.equal(win.center, true, `${platform}: 桌面窗口应居中启动`)
  }
})

test('G4 前端按平台生成设备名：桌面不再自称 Android', () => {
  const src = read('src/main.js')
  assert.ok(/function platformLabel\(\)/.test(src), 'main.js 缺少 platformLabel()')
  assert.ok(/case "windows"\s*:\s*return "Windows"/.test(src),
    'platformLabel() 没有 windows 分支（桌面配出来的设备会被叫成 Android）')
  assert.ok(/const name = `\$\{platformLabel\(\)\} /.test(src),
    '配对设备名没有走 platformLabel()')
  // 旧的写死写法不得复活。
  assert.ok(!/const name = `Android /.test(src), 'main.js 仍有写死的 `Android ${...}` 设备名')
  // 平台判定必须**只有一份**：main.js 里不再自己解析 UA 判平台，改用
  // update-source.js 导出的 platformArchFromUA（它与更新取包用的是同一份判定）。
  assert.ok(/platformArchFromUA/.test(src), 'main.js 没有复用 platformArchFromUA')
  // platformLabel() 必须按 platformArch().platform 分派，而不是自己再判一次 UA。
  // ⚠️ 不能笼统地禁掉 main.js 里所有 /android/i.test —— 那三处（deviceToken
  //    分支、isTauriAndroid、isAndroidRuntime）都是**合法的 Android 判断**，
  //    禁掉它们会把正常的功能也判红。要禁的是「又写了一份平台判定」。
  const labelBody = src.slice(src.indexOf('function platformLabel() {'), src.indexOf('function platformLabel() {') + 500)
  assert.ok(/platformArch\(\)\.platform/.test(labelBody),
    'platformLabel() 没有按 platformArch().platform 分派（平台判定出现第二份）')
  assert.ok(!/navigator\.userAgent/.test(labelBody),
    'platformLabel() 自己解析 UA 判平台（平台判定必须与更新取包共用一份）')
})

test('G5 桌面保留相册/粘贴，只收掉相机扫码', () => {
  const src = read('src/main.js')
  assert.ok(/isTauriRuntime/.test(src), 'main.js 没有区分 Tauri 运行时')
  assert.ok(/scanPairBtn\?\.setAttribute\("hidden", ""\)/.test(src),
    '桌面没有收掉「相机扫码」按钮')
  assert.ok(/scanNote\.textContent = "从相册选二维码截图/.test(src),
    '桌面没有改写扫码说明文字（会指向不存在的相机入口）')
  // 浏览器预览仍整区隐藏。
  assert.ok(/scanPairBtn\?\.closest\("section"\)\?\.setAttribute\("hidden", ""\)/.test(src),
    '浏览器预览没有整区隐藏扫码分区')
})

test('G6 桌面不隐藏「手动连接」分区标题', () => {
  const src = read('src/main.js')
  assert.ok(!/document\.querySelector\("\.group-h"\)\?\.setAttribute\("hidden", ""\)/.test(src),
    '桌面仍在隐藏「手动连接（救急）」分区标题（桌面与移动端应保持一致）')
})

test('G7 CSS 有 ≥700px 桌面档，且该档把主区放宽到 560', () => {
  const css = read('src/styles.css')
  const m = css.match(/@media\s*\(min-width:\s*(\d+)px\)/)
  assert.ok(m, 'styles.css 没有 min-width 媒体查询（桌面仍是手机排版）')
  const bp = Number(m[1])
  assert.ok(bp === CSS_DESKTOP_BREAKPOINT,
    `桌面断点是 ${bp}px，与约定的 ${CSS_DESKTOP_BREAKPOINT}px 不一致`)

  // 取出该媒体查询块，断言桌面档的宽度档位。
  const start = css.indexOf(`@media (min-width: ${bp}px)`)
  const block = css.slice(start, css.indexOf('\n}', start))
  assert.ok(/max-width:\s*560px/.test(block), `桌面档没有把主区放宽到 560px（实际：${block.match(/max-width:[^;]*/)?.[0] ?? '无'}）`)
  assert.ok(/min-height:\s*34px/.test(block), '桌面档没有收紧字段行高（44px 是触屏档）')
})

test('G8 窄屏档不受影响：主区仍是 460', () => {
  const css = read('src/styles.css')
  const base = css.split('@media (min-width:')[0]
  assert.ok(/max-width:\s*460px/.test(base),
    '主区基准 max-width 不是 460px（手机档被宽屏改动带偏）')
})
