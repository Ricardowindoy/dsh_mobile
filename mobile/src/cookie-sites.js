// 登录站点清理的纯逻辑（无 DOM / 无 Tauri 依赖，可在 node 下直测）。
//
// 抽出来的原因：这段有两条会写错、且写错了**看不出来**的规则 ——
//   ① 哪些 cookie 永远不许清（清了就要重新扫码配对 / 重新登录 dsh）；
//   ② 哪些站点行不该展示（一枚能清的都没有时，画一行"清了也没用"是在撒谎）。
// 把它们放在纯函数里，才能用测试钉住、用变异检验证明闸门真能红。

/**
 * dsh 自身的登录 cookie 前缀。语义是「前缀匹配」而不是全等：
 * dsh 用 `dsh-auth-<sha256(host)>` 给每个节点各签一枚，前缀必须能覆盖全部。
 */
export const PROTECTED_COOKIE_PREFIXES = ["dsh_relay_device", "dsh_relay_token", "dsh-auth-"];

/** 是否属于本 App 自身的登录态（不可清）。 */
export function isProtectedCookieName(name) {
  const n = String(name ?? "").trim().toLowerCase();
  if (!n) return false;
  return PROTECTED_COOKIE_PREFIXES.some((p) => n.startsWith(p));
}

/**
 * 归一化两个平台返回的站点列表，得到「可以展示给人清」的行。
 *
 * 输入形态（两侧统一）：[{site, cookieCount, names[]}]
 * 输出形态：同上，但
 *   · 受保护的 cookie 名被剔除；
 *   · 剔除后一枚不剩的行整行丢弃（该站点没有可清的登录态）；
 *   · 缺 site 的行丢弃；
 *   · cookieCount 重算为实际可清数量（不信上游给的数字 —— 上游把保护项
 *     也算进去了）。
 *
 * 不排序、不改 site 原文：是哪个 host 就显示哪个 host，不做"整理"。
 */
export function normalizeCookieSites(raw) {
  const rows = Array.isArray(raw) ? raw : [];
  const out = [];
  for (const row of rows) {
    if (!row || typeof row !== "object") continue;
    const site = String(row.site ?? "").trim();
    if (!site) continue;
    const names = Array.isArray(row.names)
      ? row.names.map((n) => String(n ?? "").trim()).filter((n) => n && !isProtectedCookieName(n))
      : [];
    // 上游可能只给数量不给名字（Android JNI 的某些分支）→ 没有名字就无法
    // 判断哪些受保护，此时按"整行不可清"处理：宁可少说，不假装能清。
    if (names.length === 0) continue;
    out.push({ site, cookieCount: names.length, names });
  }
  return out;
}
