#!/bin/bash
# dsh-mobile 发布脚本：构建 release APK → 生成 updates/manifest.json → （可选）上传到服务器。
# 学自 cloudreve-music 的发布流程：updates/<platform>/<arch>/<pkg> + manifest.json，
# Relay 的 /__dsh_tunnel__/update/check 与 /download 读取它做 App 内更新。
#
# 用法:
#   ./scripts/release.sh                 # 构建 + 写 manifest（本地）
#   ./scripts/release.sh --deploy        # 额外 scp 到更新服务器（需设 DEPLOY_TARGET / UPDATE_CHECK_URL）
#   VERSION=0.1.1 ./scripts/release.sh   # 覆盖版本号（默认读 mobile/src-tauri/tauri.conf.json）
#
# 环境（--deploy 时必填，缺失即报错）:
#   DEPLOY_TARGET      scp 目标，如 user@host:/opt/dsh-forwarder/updates
#   UPDATE_CHECK_URL   上传后的自检 URL（update/check 端点）
# 发布前务必: 1) 在 tauri.conf.json / Cargo.toml / package.json bump 版本号
#            2) 写 releaseNote 到 manifest 模板（或下方 RELEASE_NOTE）
set -euo pipefail

cd "$(dirname "$0")/.."

DEPLOY=0
[ "${1:-}" = "--deploy" ] && DEPLOY=1

VERSION="${VERSION:-}"
if [ -z "$VERSION" ]; then
  VERSION=$(python3 -c "import json;print(json.load(open('mobile/src-tauri/tauri.conf.json'))['version'])")
fi

RELEASE_NOTE="${RELEASE_NOTE:-$(git log -1 --pretty=%s 2>/dev/null || echo "dsh-mobile release")}"
RELEASE_AT=$(date +%F)

echo ">> 版本: $VERSION"
echo ">> 构建 release APK (arm64, 自签证书) ..."

# JAVA_HOME 必须指向含 javac 的完整 JDK17（JRE 不行）；ANDROID_HOME 指向本机 SDK。
[ -n "${JAVA_HOME:-}" ] || { echo "!! 请设置 JAVA_HOME（需含 javac 的 JDK17）"; exit 1; }
[ -n "${ANDROID_HOME:-}" ] || { echo "!! 请设置 ANDROID_HOME（Android SDK 路径）"; exit 1; }
export ANDROID_SDK_ROOT="$ANDROID_HOME"
export PATH="$HOME/.cargo/bin:$PATH"
export CARGO_HOME="${CARGO_HOME:-$PWD/mobile/.cargo-home}"
export GRADLE_USER_HOME="${GRADLE_USER_HOME:-$PWD/mobile/.gradle-home}"

cd mobile
pnpm tauri android build --target aarch64
cd ..

# tauri CLI 的 --target aarch64 输出目录因版本而异（arm64/release 或 universal/release），两个都找。
APK_SRC=""
for cand in \
  mobile/src-tauri/gen/android/app/build/outputs/apk/arm64/release/app-arm64-release.apk \
  mobile/src-tauri/gen/android/app/build/outputs/apk/universal/release/app-universal-release.apk; do
  [ -f "$cand" ] && APK_SRC="$cand" && break
done
[ -n "$APK_SRC" ] || { echo "!! 未找到 release APK"; exit 1; }

mkdir -p updates/android/arm64
cp "$APK_SRC" updates/android/arm64/app-arm64-release.apk

SIZE=$(stat -c %s updates/android/arm64/app-arm64-release.apk)
SHA256=$(sha256sum updates/android/arm64/app-arm64-release.apk | cut -d' ' -f1)

cat > updates/manifest.json <<EOF
{
    "items": [
        {
            "platform": "android",
            "arch": "arm64",
            "version": "$VERSION",
            "filename": "app-arm64-release.apk",
            "size": $SIZE,
            "sha256": "$SHA256",
            "releaseNote": "$RELEASE_NOTE",
            "releaseAt": "$RELEASE_AT",
            "force": false
        }
    ]
}
EOF

echo ">> manifest.json:"
cat updates/manifest.json
echo ">> APK: updates/android/arm64/app-arm64-release.apk ($SIZE bytes)"

if [ "$DEPLOY" = "1" ]; then
  [ -n "${DEPLOY_TARGET:-}" ] || { echo "!! --deploy 需要设置 DEPLOY_TARGET（如 user@host:/opt/dsh-forwarder/updates）"; exit 1; }
  [ -n "${UPDATE_CHECK_URL:-}" ] || { echo "!! --deploy 需要设置 UPDATE_CHECK_URL（update/check 端点）"; exit 1; }
  echo ">> 上传到 $DEPLOY_TARGET ..."
  sshpass -e scp -q updates/android/arm64/app-arm64-release.apk "$DEPLOY_TARGET/android/arm64/"
  sshpass -e scp -q updates/manifest.json "$DEPLOY_TARGET/manifest.json"
  echo ">> 已上传，验证："
  curl -s "$UPDATE_CHECK_URL"
  echo
fi
