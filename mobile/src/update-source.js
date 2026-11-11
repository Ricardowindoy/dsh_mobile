// 更新源解析（纯函数，可单测）：GitHub Release 优先，relay 站点兜底。
//
// 约定（发布时必须遵守，否则 App 找不到包）：
//   · GitHub Release tag：v0.1.19（可带 v 前缀）
//   · 资产名：app-arm64-release.apk
//   · 校验文件（可选但强烈建议）：app-arm64-release.apk.sha256，内容含 64 位 hex
// 私有仓库的 Release 资产需要鉴权，手机匿名下载不可用 —— 发布仓库必须公开。

export const GITHUB_REPO = "Ricardowindoy/dsh_mobile";
export const GITHUB_RELEASES_API = `https://api.github.com/repos/${GITHUB_REPO}/releases/latest`;

// Gitee 镜像源：同样是公开仓库。Gitee 的 release API 结构与 GitHub 高度相似
// （tag_name / assets[].browser_download_url / body / published_at），但
// latest 端点不是官方的，要用 releases?per_page=1 取最新一条自行判定。
// ⚠️ Gitee 目前没有发布 release（releases 返回空数组）→ 选中时优雅报「无更新」，
// 一旦在 Gitee 上发布 Release（资产名同为 app-arm64-release.apk）即可直接生效。
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

/** 从 release 资产里挑 APK 与可选校验文件。arm64 优先，其次任意 apk。 */
export function pickReleaseAssets(release) {
  const assets = Array.isArray(release?.assets) ? release.assets : [];
  const apks = assets.filter((a) => /\.apk$/i.test(a?.name || ""));
  const apk =
    apks.find((a) => /^app-arm64-release\.apk$/i.test(a.name)) ||
    apks.find((a) => /arm64/i.test(a.name)) ||
    apks[0] ||
    null;
  const sha =
    assets.find((a) => apk && a.name === `${apk.name}.sha256`) ||
    assets.find((a) => /\.sha256$/i.test(a?.name || "") || /sha256sum/i.test(a?.name || "")) ||
    null;
  return { apk, sha };
}

/**
 * 把一个 GitHub release JSON 映射成 App 的 update 对象。
 * 返回 null = 该 release 不比当前版本新（或 draft/pre-release）；
 * 抛错 = 不可用（没资产/载荷异常），调用方应回退 relay。
 */
export function githubUpdateFromRelease(release, currentVersion) {
  if (!release || typeof release !== "object") throw new Error("release payload invalid");
  if (release.draft === true || release.prerelease === true) return null;
  const tag = String(release.tag_name || "").trim().replace(/^v/i, "");
  if (!normalizeVersion(tag)) throw new Error("release tag missing");
  if (!isNewer(tag, currentVersion)) return null;
  const { apk, sha } = pickReleaseAssets(release);
  if (!apk || !apk.browser_download_url) throw new Error("release has no APK asset");
  return {
    version: tag,
    url: apk.browser_download_url,
    sha256: "",
    size: Number(apk.size) || 0,
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
export function giteeUpdateFromRelease(release, currentVersion) {
  if (!release || typeof release !== "object") throw new Error("gitee release payload invalid");
  const tag = String(release.tag_name || release.name || "").trim().replace(/^v/i, "");
  if (!normalizeVersion(tag)) throw new Error("gitee release tag missing");
  if (release.prerelease === true) return null;
  if (!isNewer(tag, currentVersion)) return null;
  const { apk, sha } = pickReleaseAssets(release);
  if (!apk || !apk.browser_download_url) throw new Error("gitee release has no APK asset");
  return {
    version: tag,
    url: apk.browser_download_url,
    sha256: "",
    size: Number(apk.size) || 0,
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
