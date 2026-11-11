#!/bin/sh
# 构建 dsh-mobile 的 release APK（aarch64）。
#
# 本机要点（内存紧张的 aarch64 构建机）：
#   · JAVA_HOME 必须指向含 javac 的完整 JDK17（系统 JRE 没有 javac）
#   · CARGO_HOME / GRADLE_USER_HOME 必须落进仓库（宿主 ~/.cargo、~/.gradle 在文件沙箱里不可写）
#   · 内存紧张：CARGO_BUILD_JOBS=4 + nice -n 10，gradle 用 --no-daemon
#     （daemon 会跨调用存活并在旧 /tmp 命名空间里握手失败，见 gen/android/gradle.properties）
#   · 若 aapt2 是 x86_64 版，靠 gradle.properties 里的 qemu 包装器（AAPT2_WRAPPER，
#     本机特定路径，不入库）
#
# 前置：mobile/node_modules 已安装（提供 node_modules/.bin/tauri）、
#       签名 keystore 在 mobile/android-keystore/（不入库）。
# 产物：mobile/src-tauri/gen/android/app/build/outputs/apk/universal/release/app-universal-release.apk
# 日志：.verify-tls/evidence/apk-build-ui.log（本脚本每次覆盖）
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
MOBILE="$ROOT/mobile"
LOG="$ROOT/.verify-tls/evidence/apk-build-ui.log"
mkdir -p "$(dirname "$LOG")"

# 机器特定路径一律 env 注入，缺失即报错（fail closed）。
[ -n "${JAVA_HOME:-}" ] || { echo "!! 请设置 JAVA_HOME（需含 javac 的 JDK17）"; exit 1; }
[ -n "${ANDROID_HOME:-}" ] || { echo "!! 请设置 ANDROID_HOME（Android SDK 路径）"; exit 1; }
export ANDROID_SDK_ROOT="$ANDROID_HOME"
export CARGO_HOME="$MOBILE/.cargo-home"
export GRADLE_USER_HOME="$MOBILE/.gradle-home"
export CARGO_BUILD_JOBS=4
export PATH="$HOME/.local/bin:$HOME/.cargo/bin:/usr/local/bin:/usr/bin:/bin"
if [ -n "${BUILD_PROXY:-}" ]; then
  export http_proxy="$BUILD_PROXY" https_proxy="$BUILD_PROXY"
fi
unset ANDROID_PREFS_ROOT || true
unset ANDROID_SDK_HOME || true

cd "$MOBILE"

status=0
{
  echo "== build-apk start $(date -Iseconds) =="
  echo "JAVA_HOME=$JAVA_HOME"
  nice -n 10 node_modules/.bin/tauri android build --target aarch64
  status=$?
  echo "== build-apk EXIT=$status $(date -Iseconds) =="
} >"$LOG" 2>&1

tail -n 3 "$LOG"
ls -la "$MOBILE/src-tauri/gen/android/app/build/outputs/apk/universal/release/" 2>/dev/null || true
exit "$status"
