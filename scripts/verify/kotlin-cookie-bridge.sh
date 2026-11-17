#!/bin/bash
# scripts/verify/kotlin-cookie-bridge.sh —— 真的编译 CookieBridge Kotlin，并校验 JNI 契约。
#
# 为什么需要：本机没有可用的 gradle（wrapper dist 未缓存、重拉 137MB 必超时），
# tauri android build 跑不起来，所以新增的 Kotlin 只能等 CI 的 Android job 编，
# 一轮 ~9 分钟。1.0.24 前两轮 CI 全红在 Kotlin 上：
#   · CookieBridge.kt `Unclosed comment` —— KDoc 里写了 lib/＋星号，Kotlin 块注释
#     允许嵌套，于是开了个永不闭合的注释；
#   · CookieBridgeNative.kt `Unresolved reference: ValueCallback` —— 漏 import；
#   · CookieBridge.kt `Unresolved reference: removeExpiredCookies` —— 该 API 在
#     API 33 废弃、android-36 的 android.jar 里已删除。
# 这三类都能在本地秒级发现：本机有 kotlinc（.diag/kt/kotlinc，2.0.21）与
# android.jar（platforms/android-36）。编不过就是编不过，区别只是花 9 分钟还是 9 秒。
#
# 顺带校验 JNI 契约：Rust 用 call_static_method 调 listSites/clearSite，
# 所以它们在字节码里必须是 static，且签名与 Rust 侧写的描述符一致
# （()Ljava/lang/String; 与 (Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;）。
#
# 退出码：0 通过 / 1 失败 / 2 SKIP（缺 kotlinc 或 android.jar）
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
KTDIR="$ROOT/mobile/src-tauri/gen/android/app/src/main/java/com/namelessric/dsh_mobile"
KOTLINC="${KOTLINC:-$ROOT/.diag/kt/kotlinc/bin/kotlinc}"
ANDROID_JAR="${ANDROID_JAR:-/home/radxa/Android/Sdk/platforms/android-36/android.jar}"
JAVAP="${JAVAP:-/opt/jdk17/bin/javap}"

[ -x "$KOTLINC" ] || { echo "SKIP: 没有 kotlinc（$KOTLINC）"; exit 2; }
[ -f "$ANDROID_JAR" ] || { echo "SKIP: 没有 android.jar（$ANDROID_JAR）"; exit 2; }
[ -x "$JAVAP" ] || { echo "SKIP: 没有 javap（$JAVAP）"; exit 2; }
[ -f "$KTDIR/CookieBridge.kt" ] || { echo "SKIP: 找不到 CookieBridge.kt"; exit 2; }

OUT="$(mktemp -d "$ROOT/.probe/kt.XXXXXX" 2>/dev/null || mktemp -d)"
trap 'rm -rf "$OUT"' EXIT

export JAVA_HOME="${JAVA_HOME:-/opt/jdk17}"
# shellcheck disable=SC2086
"$KOTLINC" -nowarn -classpath "$ANDROID_JAR" -d "$OUT" \
  "$KTDIR/CookieBridge.kt" "$KTDIR/CookieBridgeNative.kt" >"$OUT/build.log" 2>&1
rc=$?
if [ $rc -ne 0 ] || ! ls "$OUT"/com >/dev/null 2>&1; then
  echo "❌ Kotlin 编译失败："
  grep -E "error:|^e: " "$OUT/build.log" | head -10
  exit 1
fi

# JNI 契约：必须是 static，且返回 String。
SIG="$("$JAVAP" -p -classpath "$OUT" com.namelessric.dsh_mobile.CookieBridgeNative 2>/dev/null)"
fail=0
check_static() {
  local name="$1" expect="$2"
  if ! echo "$SIG" | grep -qE "public static final java.lang.String ${name}\\("; then
    echo "❌ ${name} 不是 public static String（Rust 的 call_static_method 会失败）"; fail=1; return
  fi
  if ! echo "$SIG" | grep -qE "${expect}"; then
    echo "❌ ${name} 的签名与 Rust 侧描述符不一致（期望 ${expect}）"; fail=1
  fi
}
check_static listSites 'String listSites\(\);'
check_static clearSite 'String clearSite\(java\.lang\.String, java\.lang\.String\);'
[ $fail -ne 0 ] && exit 1

echo "✅ Kotlin 编译通过，JNI 契约一致（listSites / clearSite 均为 public static String）"
exit 0
