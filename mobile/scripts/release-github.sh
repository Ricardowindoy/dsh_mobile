#!/usr/bin/env bash
# 发布 App 更新到 GitHub Release（App 更新源的第一优先级）。
#
# 背景：App 的更新检查顺序是 GitHub Release → relay 站点。
# GitHub Release 资产必须能**匿名下载**，所以发布仓库必须是公开仓；
# 私有仓的 release 资产手机匿名请求会 401/404（脚本会检测并拒绝）。
#
# 用法：
#   ./scripts/release-github.sh                     # 用 tauri.conf 的版本 + 已构建的 APK
#   ./scripts/release-github.sh --apk path/to.apk   # 指定 APK
#   GITHUB_RELEASE_REPO=owner/public-repo ./scripts/release-github.sh
#
# 环境：
#   GITHUB_TOKEN        必填（或在环境中先行导出）
#   GITHUB_RELEASE_REPO 发布仓（owner/public-repo，必填）
#   HTTPS_PROXY         可选；缺省直连
set -euo pipefail

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)   # mobile/scripts → 工作区根
APP="$ROOT/mobile"
[ -n "${GITHUB_RELEASE_REPO:-}" ] || { echo "!! 缺 GITHUB_RELEASE_REPO（owner/public-repo，须为公开仓）"; exit 1; }
REPO="$GITHUB_RELEASE_REPO"
ASSET_NAME="app-arm64-release.apk"
API="https://api.github.com/repos/$REPO"
if [ -n "${HTTPS_PROXY:-}" ]; then
  CURL=(curl -sS --proxy "$HTTPS_PROXY")
else
  CURL=(curl -sS --noproxy '*')
fi

APK=""
while [ $# -gt 0 ]; do
  case "$1" in
    --apk) APK="$2"; shift 2 ;;
    *) echo "未知参数: $1"; exit 2 ;;
  esac
done
VERSION=$(python3 -c "import json;print(json.load(open('$APP/src-tauri/tauri.conf.json'))['version'])")
if [ -z "$APK" ]; then
  for cand in \
    "$ROOT/dist/dsh-mobile-$VERSION.apk" \
    "$APP/src-tauri/gen/android/app/build/outputs/apk/universal/release/app-universal-release.apk"; do
    [ -f "$cand" ] && APK="$cand" && break
  done
fi
[ -n "$APK" ] && [ -f "$APK" ] || { echo "!! 找不到 APK（先跑 mobile-app/build-apk-workcopy.sh，或 --apk 指定）"; exit 1; }

TOKEN="${GITHUB_TOKEN:-}"
[ -n "$TOKEN" ] || { echo "!! 缺 GITHUB_TOKEN（在环境中导出，勿写进脚本或仓库）"; exit 1; }

TAG="v$VERSION"
echo ">> 版本 $VERSION → tag $TAG"
echo ">> 发布仓 $REPO"

echo "=== 仓库可见性（release 资产必须匿名可下） ==="
"${CURL[@]}" -H "Authorization: Bearer $TOKEN" "$API" -o /tmp/repo.json -w "http=%{http_code}\n"
python3 - <<'PY'
import json
d=json.load(open('/tmp/repo.json'))
if d.get('private'):
    print(f"  !! {d['full_name']} 是私有仓：release 资产无法匿名下载，App 会一直回退 relay。")
    print("  → 把仓库转公开，或 GITHUB_RELEASE_REPO=owner/public-releases-repo 指向一个公开仓。")
    raise SystemExit(1)
print(f"  公开仓 {d['full_name']} ✓")
PY

SIZE=$(stat -c %s "$APK")
SHA256=$(sha256sum "$APK" | cut -d' ' -f1)
NOTE="${RELEASE_NOTE:-dsh-mobile $VERSION}"
echo "=== APK $SIZE bytes sha256=$SHA256 ==="

echo "=== 创建 / 复用 release ==="
RELEASE_JSON=$(TAG="$TAG" VERSION="$VERSION" NOTE="$NOTE" python3 - <<'PY'
import json, os
print(json.dumps({
    "tag_name": os.environ["TAG"],
    "name": f"dsh-mobile {os.environ['VERSION']}",
    "body": os.environ["NOTE"],
    "draft": False,
    "prerelease": False,
}, ensure_ascii=False))
PY
)
"${CURL[@]}" -X POST -H "Authorization: Bearer $TOKEN" -H "Accept: application/vnd.github+json" \
  "$API/releases" -d "$RELEASE_JSON" \
  -o /tmp/release.json -w "create=%{http_code}\n" || true
REL_ID=$(python3 - <<'PY'
import json
try:
    d=json.load(open('/tmp/release.json'))
    print(d.get('id',''))
except Exception:
    print('')
PY
)
if [ -z "$REL_ID" ]; then
  echo "  创建失败（可能已存在），按 tag 查询…"
  "${CURL[@]}" -H "Authorization: Bearer $TOKEN" "$API/releases/tags/$TAG" -o /tmp/release.json
  REL_ID=$(python3 -c "import json;print(json.load(open('/tmp/release.json'))['id'])")
fi
echo "  release id=$REL_ID"

upload() { # $1=file $2=asset name
  echo "  上传 $2 ..."
  "${CURL[@]}" -X POST -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/octet-stream" \
    --data-binary "@$1" \
    "https://uploads.github.com/repos/$REPO/releases/$REL_ID/assets?name=$2" \
    -o /tmp/asset.json -w "  upload=%{http_code}\n"
  python3 -c "import json;d=json.load(open('/tmp/asset.json'));print('  asset:',d.get('name'),d.get('state'),d.get('size'))"
}

# 幂等：同名资产已存在时先删（GitHub 不允许同名覆盖）
DEL_IDS=$("${CURL[@]}" -H "Authorization: Bearer $TOKEN" "$API/releases/$REL_ID/assets" | python3 -c "
import json,sys
names={'$ASSET_NAME','$ASSET_NAME.sha256'}
print(' '.join(str(a['id']) for a in json.load(sys.stdin) if a['name'] in names))
")
for id in $DEL_IDS; do
  echo "  删除旧资产 id=$id"
  "${CURL[@]}" -X DELETE -H "Authorization: Bearer $TOKEN" "$API/releases/assets/$id" -o /dev/null
done

upload "$APK" "$ASSET_NAME"
printf '%s  %s\n' "$SHA256" "$ASSET_NAME" > /tmp/app-arm64-release.apk.sha256
upload /tmp/app-arm64-release.apk.sha256 "$ASSET_NAME.sha256"

echo "=== 匿名下载验证（不带任何 Authorization） ==="
URL="https://github.com/$REPO/releases/download/$TAG/$ASSET_NAME"
if [ -n "${HTTPS_PROXY:-}" ]; then
  CODE=$(curl -sSL --proxy "$HTTPS_PROXY" -o /tmp/dl.apk -w '%{http_code}' "$URL")
else
  CODE=$(curl -sSL --noproxy '*' -o /tmp/dl.apk -w '%{http_code}' "$URL")
fi
echo "  GET $URL → $CODE"
if [ "$CODE" = "200" ]; then
  GOT=$(sha256sum /tmp/dl.apk | cut -d' ' -f1)
  [ "$GOT" = "$SHA256" ] && echo "  sha256 一致 ✓" || { echo "  !! sha256 不一致: $GOT"; exit 1; }
else
  echo "  !! 匿名下载失败：GitHub Release 不能作为更新源"
  exit 1
fi

echo "完成：$TAG 已发布到 $REPO（App 下次启动会优先从 GitHub 检查更新）"
