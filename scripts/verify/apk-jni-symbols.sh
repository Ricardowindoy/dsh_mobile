#!/bin/bash
# scripts/verify/apk-jni-symbols.sh <apk> —— 检查产出的 APK 里真的还有 JNI 入口符号。
#
# 为什么需要：release 构建是 isMinifyEnabled=true，R8 会把「只被 native 调用」的
# 静态方法当无用代码删掉 —— Java 侧没有任何引用，R8 无从知道 Rust 在 JNI 里按名字
# 找它。1.0.25 真机就是这样挂的：
#   java.lang.NoSuchMethodError: no static method
#     "Lcom/namelessric/dsh_mobile/CookieBridgeNative;.listSites()Ljava/lang/String;"
# 类还在、方法没了 —— 光看源码或本地编译（编译不跑 R8）完全看不出来，只有把产物
# 拆开查 dex 才知道。所以这条闸门对着**产物**验，不对源码验。
#
# 判据：lib.rs 里 find_class 找的每个类名，以及 Rust 对这些类调的静态方法名，
# 必须能在 APK 的 classes*.dex 字符串表里找到。
#
# 退出码：0 通过 / 1 失败 / 2 SKIP（APK 不存在或缺 unzip）
set -uo pipefail

APK="${1:-}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
LIB="$ROOT/mobile/src-tauri/src/lib.rs"

[ -n "$APK" ] || { echo "SKIP: 用法 apk-jni-symbols.sh <apk>"; exit 2; }
[ -f "$APK" ] || { echo "SKIP: APK 不存在（$APK）"; exit 2; }
command -v unzip >/dev/null 2>&1 || { echo "SKIP: 没有 unzip"; exit 2; }
[ -f "$LIB" ] || { echo "SKIP: 找不到 lib.rs"; exit 2; }

# Rust 侧按名字查找的类（find_class 用 JNI 斜杠名，dex 里是点号名）。
CLASSES=$(grep -oE 'find_class\(\s*"com/namelessric/dsh_mobile/[A-Za-z0-9_$]+"' "$LIB" \
  | sed -E 's|.*dsh_mobile/([A-Za-z0-9_$]+)"|com.namelessric.dsh_mobile.\1|' | sort -u)
[ -n "$CLASSES" ] || { echo "SKIP: lib.rs 里没找到 find_class（结构变了？）"; exit 2; }

# Rust 对这些类调用的静态方法名（call_static_method 的实参）。
# 名单短且稳定，直接列；新增 JNI 入口时同步加这里。
METHODS="listSites clearSite"

DEX=$(mktemp)
trap 'rm -f "$DEX"' EXIT
unzip -p "$APK" 'classes*.dex' > "$DEX" 2>/dev/null
[ -s "$DEX" ] || { echo "❌ 拆不出 classes*.dex"; exit 1; }

fail=0
for c in $CLASSES; do
  if grep -qa -- "$c" "$DEX"; then echo "  ✅ 类 $c"
  else echo "  ❌ 类 $c 不在 dex 里（find_class 会 ClassNotFoundException）"; fail=1; fi
done
for m in $METHODS; do
  if grep -qa -- "$m" "$DEX"; then echo "  ✅ 方法 $m"
  else echo "  ❌ 方法 $m 不在 dex 里（R8 删了？真机会 NoSuchMethodError）"; fail=1; fi
done

[ $fail -ne 0 ] && { echo "❌ APK 缺少 JNI 入口符号 —— 检查 proguard-rules.pro 的 -keep"; exit 1; }
echo "✅ APK 里的 JNI 入口符号齐全"
exit 0
