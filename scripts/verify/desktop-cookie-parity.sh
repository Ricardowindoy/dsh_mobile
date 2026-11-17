#!/bin/bash
# scripts/verify/desktop-cookie-parity.sh —— 桌面端 cookie 命令的结构编译闸门。
#
# 为什么需要：本机没有 GTK/WebKitGTK（glib 2.66 < 2.70、webkit2gtk 缺失），
# `cargo check` 编不了 tauri 的 Linux 后端，所以 `#[cfg(not(target_os = "android"))]`
# 里的代码**本地一行都编不到**——它只在 CI 的 Windows/Linux/macOS/iOS 四个 job 里
# 才被编译，一轮 20 分钟。
#
# 1.0.24 就是这么翻车的：delete_cookie 我照 wry 的签名写了 &Cookie，而
# Tauri 的 WebviewWindow::delete_cookie 收的是**按值** Cookie<'_>，CI 四个 job
# 一起 E0308。这个脚本把那段代码抽出来，配上**照 Tauri 真实签名**写的 mock，
# 用 cargo check 做类型检查，把这类错误提前到本地秒级暴露。
#
# ⚠️ mock 的签名必须逐条对照 .cargo-home 里的 tauri 源码抄，不能凭记忆——
#    第一版 mock 抄的是 wry 的签名（&Cookie），于是它「验证通过」了错误的代码。
#
# 退出码：0 通过 / 1 失败 / 2 SKIP（缺 cargo、缺 vendored registry、抽不到代码）
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
LIB="$ROOT/mobile/src-tauri/src/lib.rs"
CARGO_HOME_DIR="$ROOT/mobile/.cargo-home"

# cargo 装在本机 ~/.cargo/bin，非交互 shell 的 PATH 里可能没有 —— 先补再判。
export PATH="$HOME/.cargo/bin:$PATH"
command -v cargo >/dev/null 2>&1 || { echo "SKIP: 没有 cargo"; exit 2; }
[ -f "$LIB" ] || { echo "SKIP: 找不到 $LIB"; exit 2; }
[ -d "$CARGO_HOME_DIR/registry" ] || { echo "SKIP: 缺 vendored registry（$CARGO_HOME_DIR/registry）"; exit 2; }

WORK="$(mktemp -d "$ROOT/.probe/parity.XXXXXX" 2>/dev/null || mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/src"

cat > "$WORK/Cargo.toml" <<'TOML'
[package]
name = "desktop-cookie-parity"
version = "0.1.0"
edition = "2021"
[dependencies]
cookie = "0.18"
serde_json = "1"
TOML

python3 - "$LIB" "$WORK/src/lib.rs" <<'PY'
import io, sys
src = io.open(sys.argv[1], encoding='utf-8').read()
try:
    start = src.index('#[cfg(not(target_os = "android"))]\nfn cookie_site')
    end = src.index('// --- Android：按站点清登录 cookie')
except ValueError:
    print("SKIP: lib.rs 里找不到桌面 cookie 段落（结构变了？）", file=sys.stderr)
    raise SystemExit(3)
body = src[start:end].replace('#[cfg(not(target_os = "android"))]\n', '').replace('#[tauri::command]\n', '')

# mock 逐条对照 tauri-2.12.5/src/webview/webview_window.rs:
#   cookies()                -> Result<Vec<Cookie<'static>>>
#   delete_cookie(Cookie<'_>)-> Result<()>          ← 按值，不是 &Cookie
#   AppHandle::get_webview_window(&str) -> Option<WebviewWindow>
mock = '''use cookie::Cookie;
use serde_json::{json, Value};

pub mod tauri {
    use super::*;
    pub struct WebviewWindow;
    impl WebviewWindow {
        pub fn cookies(&self) -> Result<Vec<Cookie<'static>>, String> { unimplemented!() }
        pub fn delete_cookie(&self, _c: Cookie<'_>) -> Result<(), String> { unimplemented!() }
    }
    pub struct AppHandle;
    impl AppHandle {
        pub fn get_webview_window(&self, _label: &str) -> Option<WebviewWindow> { None }
    }
    pub mod async_runtime {
        pub fn spawn_blocking<F, T>(f: F) -> JoinHandle<T>
        where F: FnOnce() -> T + Send + 'static, T: Send + 'static {
            let _ = f; JoinHandle(std::marker::PhantomData)
        }
        pub struct JoinHandle<T>(pub std::marker::PhantomData<T>);
        impl<T> std::future::Future for JoinHandle<T> {
            type Output = Result<T, String>;
            fn poll(self: std::pin::Pin<&mut Self>, _cx: &mut std::task::Context<'_>)
                -> std::task::Poll<Self::Output> { std::task::Poll::Pending }
        }
    }
}

'''
io.open(sys.argv[2], 'w', encoding='utf-8').write(mock + body)
PY
rc=$?
[ $rc -eq 3 ] && { echo "SKIP: 抽不到桌面 cookie 段落"; exit 2; }
[ $rc -ne 0 ] && { echo "❌ 抽取失败"; exit 1; }

export CARGO_HOME="$CARGO_HOME_DIR"
export CARGO_TARGET_DIR="$WORK/target"
export PATH="$HOME/.cargo/bin:$PATH"

if cargo check --offline --manifest-path "$WORK/Cargo.toml" >"$WORK/out.log" 2>&1; then
  # 只允许 dead_code 类警告（这两个 fn 在 mock 里没人调用，属预期）。
  if grep -qE "^error" "$WORK/out.log"; then
    echo "❌ 编译有错误："; grep -A3 -E "^error" "$WORK/out.log" | head -20; exit 1
  fi
  echo "✅ 桌面 cookie 命令结构编译通过（mock 按 Tauri 真实签名）"
  exit 0
else
  echo "❌ 桌面 cookie 命令编译失败："
  grep -E "^error|-->|expected|found" "$WORK/out.log" | head -15
  exit 1
fi
