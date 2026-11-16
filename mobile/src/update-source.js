// 更新源解析（纯函数，可单测）：GitHub Release 优先，relay 站点兜底。
//
// 约定（发布时必须遵守，否则 App 找不到包）：
//   · GitHub Release tag：v1.0.22（可带 v 前缀）
//   · 每个平台各有一条资产，资产名必须与 build-manifest.py 的 CANONICAL 一致：
//       android  app-arm64-release.apk
//       windows  dsh-mobile_<ver>_x64-setup.exe
//       macos    dsh-mobile_<ver>_universal.dmg
//       linux    dsh-mobile_<ver>_amd64.deb（备选 AppImage/rpm）
//       ios      dsh-mobile-unsigned.ipa
//   · 校验文件（可选但强烈建议）：<资产名>.sha256，内容含 64 位 hex
// 私有仓库的 Release 资产需要鉴权，手机匿名下载不可用 —— 发布仓库必须公开。
//
// ⚠️ 2026-11-12 实测：桌面端（Windows v1.0.13）点更新下载到 7.1 MB 的
// app-arm64-release.apk，然后 install_update 报「仅支持 Android」。根因是本文件
// 过去只按 .apk 挑资产，完全没看平台；桌面 release 里的 x64-setup.exe 被忽略。

export const GITHUB_REPO = "Ricardowindoy/dsh_mobile";
export const GITHUB_RELEASES_API = `https://api.github.com/repos/${GITHUB_REPO}/releases/latest`;

// Gitee 镜像源：同样是公开仓库。Gitee 的 release API 结构与 GitHub 高度相似
// （tag_name / assets[].browser_download_url / body / published_at），但
// latest 端点不是官方的，要用 releases?per_page=1 取最新一条自行判定。
// ⚠️ Gitee 目前没有发布 release（releases 返回空数组）→ 选中时优雅报「无更新」，
// 一旦在 Gitee 上发布 Release（资产名与上面各平台的约定一致）即可直接生效。
export const GITEE_REPO = "namelessRic/dsh_mobile";
// ⚠️ per_page 必须拉一整页再自己挑最大版本：Gitee 的 releases 列表是
// **按创建时间升序**返回的（v1.0.13 → v1.0.16），`per_page=1` 拿到的是
// 最旧的一条，会让「选 Gitee」永远判定为已是最新（2026-11-11 实测踩坑）。
export const GITEE_RELEASES_API = `https://gitee.com/api/v5/repos/${GITEE_REPO}/releases?page=1&per_page=100`;

// 可选下载源（与 UI 下拉一一对应，也是持久化偏好的合法值）。
export const UPDATE_SOURCES = ["gitee", "github", "relay"];
export const DEFAULT_UPDATE_SOURCE = "github";

/** 把任意输入规整成合法源名；不认识的回落到默认值，绝不返回空。 */
export function normalizeSource(raw) {
  const s = String(raw ?? "").trim().toLowerCase();
  return UPDATE_SOURCES.includes(s) ? s : DEFAULT_UPDATE_SOURCE;
}

/** 去掉 v 前缀，只保留数字段（0.1.19-fix2 → 0.1.19）。 */
export function normalizeVersion(raw) {
  const s = String(raw ?? "").trim().replace(/^v/i, "");
  const m = s.match(/^\d+(?:\.\d+)*/);
  return m ? m[0] : "";
}

/** 语义化比较：a>b 返回 1，相等 0，a<b 返回 -1。缺段按 0 处理。 */
export function compareVersions(a, b) {
  const pa = normalizeVersion(a).split(".").filter(Boolean).map(Number);
  const pb = normalizeVersion(b).split(".").filter(Boolean).map(Number);
  const n = Math.max(pa.length, pb.length);
  for (let i = 0; i < n; i += 1) {
    const x = pa[i] || 0;
    const y = pb[i] || 0;
    if (x !== y) return x > y ? 1 : -1;
  }
  return 0;
}

export function isNewer(remote, local) {
  return compareVersions(remote, local) > 0;
}

/**
 * 从 UA 判定更新目标平台/架构。
 *
 * 发布端 manifest 的 platform 取值是 android / windows / macos / linux / ios，
 * arch 是 arm64 / amd64 / universal。这里只使用 UA 能可靠给出的粒度：
 * Windows 目前只有 x64 安装包、Linux 只有 amd64、macOS 是 universal。
 * 未知 UA 仍按 Android 处理（历史行为，避免浏览器预览误报）。
 */
export function platformArchFromUA(ua) {
  const s = String(ua || "");
  if (/android/i.test(s)) {
    if (/(aarch64|arm64)/i.test(s)) return { platform: "android", arch: "arm64" };
    if (/(x86_64|amd64|x64)/i.test(s)) return { platform: "android", arch: "x86_64" };
    return { platform: "android", arch: "arm64" };
  }
  if (/iphone|ipad|ipod/i.test(s)) return { platform: "ios", arch: "arm64" };
  if (/windows/i.test(s)) return { platform: "windows", arch: "amd64" };
  if (/macintosh|mac os x/i.test(s)) return { platform: "macos", arch: "universal" };
  if (/linux|x11/i.test(s)) return { platform: "linux", arch: "amd64" };
  return { platform: "android", arch: "arm64" };
}

// 每个平台的资产匹配规则（按优先级）。注意排除 dsh-tunnel-* 数据面二进制：
// 它们和安装包在同一个 release 里，Windows 上同样是 .exe。
const PLATFORM_ASSET_RULES = {
  android: [
    (name) => /^app-arm64-release\.apk$/i.test(name),
    (name) => /arm64/i.test(name) && /\.apk$/i.test(name),
    (name) => /\.apk$/i.test(name),
  ],
  windows: [
    (name) => /\.exe$/i.test(name) && /(setup|installer)/i.test(name) && !/^dsh-tunnel-/i.test(name),
    (name) => /\.exe$/i.test(name) && !/^dsh-tunnel-/i.test(name),
  ],
  macos: [(name) => /\.dmg$/i.test(name), (name) => /\.app$/i.test(name)],
  linux: [
    (name) => /\.deb$/i.test(name),
    (name) => /\.AppImage$/i.test(name),
    (name) => /\.rpm$/i.test(name),
  ],
  ios: [(name) => /\.ipa$/i.test(name)],
};

/**
 * 从 release 资产里挑当前平台的安装包与同名校验文件。
 * 默认 target=android/arm64，保持旧调用与旧测试的语义。
 * 校验文件只认 `<资产名>.sha256`：release 里同时有多平台的 .sha256，
 * 随便拿一个会让下载后的 SHA-256 校验稳定失败。
 */
export function pickReleaseAssets(release, target = { platform: "android", arch: "arm64" }) {
  const assets = Array.isArray(release?.assets) ? release.assets : [];
  const platform = String(target?.platform || "android").toLowerCase();
  const rules = PLATFORM_ASSET_RULES[platform] || PLATFORM_ASSET_RULES.android;
  let asset = null;
  for (const matches of rules) {
    asset = assets.find((a) => a && matches(String(a.name || ""))) || null;
    if (asset) break;
  }
  const sha = asset ? assets.find((a) => a.name === `${asset.name}.sha256`) || null : null;
  return { asset, sha };
}

/**
 * 把一个 GitHub release JSON 映射成 App 的 update 对象。
 * 返回 null = 该 release 不比当前版本新（或 draft/pre-release）；
 * 抛错 = 不可用（没资产/载荷异常），调用方应回退 relay。
 */
export function githubUpdateFromRelease(release, currentVersion, target = { platform: "android", arch: "arm64" }) {
  if (!release || typeof release !== "object") throw new Error("release payload invalid");
  if (release.draft === true || release.prerelease === true) return null;
  const tag = String(release.tag_name || "").trim().replace(/^v/i, "");
  if (!normalizeVersion(tag)) throw new Error("release tag missing");
  if (!isNewer(tag, currentVersion)) return null;
  const { asset, sha } = pickReleaseAssets(release, target);
  const platform = String(target?.platform || "android").toLowerCase();
  if (!asset || !asset.browser_download_url) throw new Error(`release has no ${platform} installer`);
  return {
    version: tag,
    url: asset.browser_download_url,
    filename: String(asset.name || ""),
    sha256: "",
    size: Number(asset.size) || 0,
    releaseNote: String(release.body || release.name || "").replace(/\s+/g, " ").trim().slice(0, 400),
    releaseAt: String(release.published_at || "").slice(0, 10),
    force: false,
    source: "github",
    checksumUrl: sha?.browser_download_url || "",
  };
}

/**
 * 把一个 Gitee release 对象映射成 App 的 update 对象。
 * Gitee release 结构与 GitHub 基本同构（tag_name / assets / body / created_at），
 * 但字段名略有差异：资产下载地址在 assets[].browser_download_url，时间用
 * created_at（无 published_at）。prerelease 用 prerelease 字段。
 * 返回 null = 不比当前版本新；抛错 = 不可用。调用方据此回退。
 */
export function giteeUpdateFromRelease(release, currentVersion, target = { platform: "android", arch: "arm64" }) {
  if (!release || typeof release !== "object") throw new Error("gitee release payload invalid");
  const tag = String(release.tag_name || release.name || "").trim().replace(/^v/i, "");
  if (!normalizeVersion(tag)) throw new Error("gitee release tag missing");
  if (release.prerelease === true) return null;
  if (!isNewer(tag, currentVersion)) return null;
  const { asset, sha } = pickReleaseAssets(release, target);
  const platform = String(target?.platform || "android").toLowerCase();
  if (!asset || !asset.browser_download_url) throw new Error(`gitee release has no ${platform} installer`);
  return {
    version: tag,
    url: asset.browser_download_url,
    filename: String(asset.name || ""),
    sha256: "",
    size: Number(asset.size) || 0,
    releaseNote: String(release.body || release.name || "").replace(/\s+/g, " ").trim().slice(0, 400),
    releaseAt: String(release.created_at || release.published_at || "").slice(0, 10),
    force: false,
    source: "gitee",
    checksumUrl: sha?.browser_download_url || "",
  };
}

/**
 * Pick the highest-version release out of a Gitee release list.
 *
 * Gitee's `/releases` endpoint returns releases in **ascending creation
 * order**, so taking `arr[0]` returns the OLDEST release. That made the
 * "download from Gitee" preference permanently report "already up to date"
 * (the oldest tag never beats the installed version). Select by version
 * instead of by position so the result is order-independent.
 *
 * @param releases - raw list from the Gitee API.
 * @returns the release with the highest normalizable tag, or null.
 */
export function pickLatestGiteeRelease(releases) {
  const list = Array.isArray(releases) ? releases : [];
  let best = null;
  let bestVersion = "";
  for (const release of list) {
    if (!release || typeof release !== "object") continue;
    const version = normalizeVersion(release.tag_name || release.name || "");
    if (!version) continue;
    if (best === null || compareVersions(version, bestVersion) > 0) {
      best = release;
      bestVersion = version;
    }
  }
  return best;
}

/** 从 .sha256 文件里取第一段 64 位 hex。 */
export function parseChecksum(text) {
  const m = String(text || "").match(/\b([0-9a-f]{64})\b/i);
  return m ? m[1].toLowerCase() : "";
}
