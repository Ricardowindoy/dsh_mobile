// 更新源解析（纯函数，可单测）：GitHub Release 优先，relay 站点兜底。
//
// 约定（发布时必须遵守，否则 App 找不到包）：
//   · GitHub Release tag：v0.1.19（可带 v 前缀）
//   · 资产名：app-arm64-release.apk
//   · 校验文件（可选但强烈建议）：app-arm64-release.apk.sha256，内容含 64 位 hex
// 私有仓库的 Release 资产需要鉴权，手机匿名下载不可用 —— 发布仓库必须公开。

export const GITHUB_REPO = "Ricardowindoy/dsh_mobile";
export const GITHUB_RELEASES_API = `https://api.github.com/repos/${GITHUB_REPO}/releases/latest`;

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

/** 从 .sha256 文件里取第一段 64 位 hex。 */
export function parseChecksum(text) {
  const m = String(text || "").match(/\b([0-9a-f]{64})\b/i);
  return m ? m[1].toLowerCase() : "";
}
