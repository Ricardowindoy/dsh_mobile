#!/usr/bin/env node
/* =============================================================================
 * verify-desktop-merge.mjs —— 桌面平台配置**真的被合并**了吗？（不是"文件写了"）
 * -----------------------------------------------------------------------------
 * 为什么需要这一步：闸门只能断言文件里有 maximizable:true / width:960，
 * 但 Tauri 到底会不会读那份平台配置、合并进主配置，是另一回事。
 * 本脚本用**真实的 tauri-utils**（tauri-utils 2.10.2 的 read_from）跑一遍，
 * 直接读出四个平台各自最终生效的窗口配置。
 *
 * 判定（硬门禁，失败即 exit 1）：
 *   Windows / Linux / macOS：合并路径数 = 2（主配置 + 平台配置），
 *                            width ≥ 768（越过宿主 GUI 抽屉断点），
 *                            maximizable ≠ false（系统标题栏有最大化按钮）
 *   Android：合并路径数 = 1（**不读**桌面平台配置），
 *             width = 420 / height = 760 —— 手机排版必须原样不动
 *
 * 依赖：mobile/.cargo-home 里已有 tauri-utils 2.10.2 的 vendored 源（离线可用）。
 *       缺依赖时如实报 SKIP 而**不是** PASS —— 没验到不等于成立。
 * 用法：node .verify-ui/verify-desktop-merge.mjs
 * ========================================================================== */
import { existsSync, readFileSync, writeFileSync, mkdirSync, readdirSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { spawnSync } from 'node:child_process'

const HERE = dirname(fileURLToPath(import.meta.url))
const REPO = resolve(HERE, '..', '..')
const MOBILE = join(REPO, 'mobile')
const SRC_TAURI = join(MOBILE, 'src-tauri')
const CARGO_HOME = join(MOBILE, '.cargo-home')
const WORK = join(REPO, '.probe', 'mergeproof')

/** 宿主 DSH GUI 的抽屉断点：视口窄于此值就渲染手机档。 */
const GUI_DRAWER_BREAKPOINT = 768

/* ---- 1. 依赖检查：缺就 SKIP，不许 PASS ---- */
const tauriUtilsSrc = (() => {
  const reg = join(CARGO_HOME, 'registry', 'src')
  if (!existsSync(reg)) return null
  for (const org of readdirSync(reg)) {
    const p = join(reg, org, 'tauri-utils-2.10.2')
    if (existsSync(p)) return p
  }
  return null
})()

if (!tauriUtilsSrc) {
  console.log('SKIP: 找不到 vendored tauri-utils 2.10.2（mobile/.cargo-home/registry/src/*/tauri-utils-2.10.2）')
  console.log('      → 没验到合并行为，不等于成立。本步需 SKIP，不算通过。')
  process.exit(2)
}

/* ---- 2. 写一个最小 Rust 程序，用真实 tauri-utils 读四个平台的最终配置 ---- */
mkdirSync(join(WORK, 'src'), { recursive: true })
writeFileSync(join(WORK, 'Cargo.toml'), `[package]
name = "mergeproof"
version = "0.1.0"
edition = "2021"

[dependencies]
tauri-utils = { version = "2.10.2" }
`)
writeFileSync(join(WORK, 'src', 'main.rs'), `use std::path::Path;
use tauri_utils::config::parse::read_from;
use tauri_utils::platform::Target;

fn main() {
    let root = Path::new(${JSON.stringify(SRC_TAURI)});
    for target in [Target::Windows, Target::Linux, Target::MacOS, Target::Android] {
        match read_from(target, root) {
            Ok((cfg, paths)) => {
                let w = cfg["app"]["windows"][0].clone();
                println!(
                    "{}\\t{}\\t{}\\t{}\\t{}\\t{}",
                    format!("{:?}", target),
                    paths.len(),
                    w["width"], w["height"], w["maximizable"], w["center"]
                );
            }
            Err(e) => println!("{:?}\\tERROR\\t{}", format!("{:?}", target), e),
        }
    }
}
`)

/* ---- 3. 跑它 ---- */
const cargo = join(process.env.HOME || '/root', '.cargo', 'bin', 'cargo')
if (!existsSync(cargo)) {
  console.log(`SKIP: 找不到 cargo（${cargo}）—— 无法验证合并行为，不算通过。`)
  process.exit(2)
}
const r = spawnSync(cargo, ['run', '--offline', '-q'], {
  cwd: WORK,
  encoding: 'utf8',
  env: {
    ...process.env,
    CARGO_HOME,
    CARGO_TARGET_DIR: join(WORK, 'target'),
    PATH: `${join(process.env.HOME || '/root', '.cargo', 'bin')}:${process.env.PATH || ''}`,
  },
})
if (r.status !== 0) {
  console.error('合并验证跑不起来（不算通过）：')
  console.error(r.stderr?.slice(-2000) || r.stdout?.slice(-2000) || 'no output')
  process.exit(2)
}

/* ---- 4. 判定 ---- */
const rows = r.stdout.trim().split('\n').filter(Boolean).map((line) => {
  const [platform, paths, width, height, maximizable, center] = line.split('\t')
  return { platform, paths: Number(paths), width: Number(width), height: Number(height), maximizable, center }
})

const failures = []
const DESKTOP = ['Windows', 'Linux', 'MacOS']
for (const row of rows) {
  if (row.platform === 'Android') {
    // Android 绝不能被桌面平台配置污染。
    if (row.paths !== 1) failures.push(`Android 读了 ${row.paths} 份配置（应为 1：手机排版不得受桌面配置影响）`)
    if (row.width !== 420 || row.height !== 760) failures.push(`Android 窗口变成 ${row.width}x${row.height}（应为 420x760）`)
    if (row.maximizable !== 'null') failures.push(`Android maximizable=${row.maximizable}（应为 null，即主配置原样）`)
    continue
  }
  if (!DESKTOP.includes(row.platform)) continue
  if (row.paths !== 2) failures.push(`${row.platform} 合并了 ${row.paths} 份配置（应为 2：主配置 + 平台配置，说明平台配置没被读到）`)
  if (!(row.width >= GUI_DRAWER_BREAKPOINT)) failures.push(`${row.platform} width=${row.width} < ${GUI_DRAWER_BREAKPOINT}（内嵌 GUI 仍是手机档）`)
  if (row.maximizable === 'false') failures.push(`${row.platform} maximizable=false（系统标题栏没有最大化按钮）`)
  if (row.center !== 'true') failures.push(`${row.platform} center=${row.center}（桌面窗口应居中启动）`)
}

console.log('=== 真实 tauri-utils 合并结果 ===')
for (const row of rows) {
  console.log(`  ${row.platform.padEnd(8)} paths=${row.paths}  ${row.width}x${row.height}  maximizable=${row.maximizable}  center=${row.center}`)
}
if (failures.length) {
  console.error('\n❌ 合并验证失败：')
  for (const f of failures) console.error(`  - ${f}`)
  process.exit(1)
}
console.log('\n✅ 桌面三平台都合并了平台配置（可最大化 + 宽过抽屉断点），Android 手机排版未受影响')
process.exit(0)
