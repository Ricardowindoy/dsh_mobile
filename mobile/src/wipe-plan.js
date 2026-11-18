// 「清掉登录态、但保留配对信息」的清理计划（纯逻辑，无 DOM / 无 Tauri）。
//
// 用户的原话是：「不把配对信息去掉，但是清理掉所有 cookie 和本地存储，现在还是
// 有登录态」。这句话里有两个必须同时成立、又互相矛盾的事实：
//
//   ① 要清掉**所有**站点痕迹 —— 按站点清单清是不够的（清单外的站点不会出现
//      在面板里，用户就会遇到"清完还是登录态"）；
//   ② 清完之后**不能要求重新扫码配对** —— 配对信息（relayUrl / deviceToken /
//      deviceId）必须留着，否则用户要再找一遍桌面二维码。
//
// 能做到的关键事实：配对信息**不在 WebView 存储里**。它是 Rust 写在 app 配置
// 目录 config.json 的内容（localStorage 只是镜像）。所以只要在清理前后把这份
// 配置保住，整店清 cookie / localStorage / IndexedDB 是安全的 —— dsh 自己的
// 登录 cookie 会没，但那正是 ② 要解决的：清完立刻用设备凭据换一次性 ticket
// 重新登录（pair/refresh → login?ticket=），用户无感。
//
// 抽成纯函数的原因：这里有两条写错了**看不出来**的规则 ——
//   · 该留的配置项丢了（清一次就得重新扫码，用户会以为 App 出 bug）；
//   · 不该留的站点残留也算进"配对信息"里带回来了（清了等于没清）。
// 放到纯函数里才能用测试钉住、用变异检验证明闸门真能红。

/**
 * 清理时必须原样保留的配置键。**只留配对与连接所需的最小集合**：
 *   · relayUrl     —— 要连哪台 relay；
 *   · deviceToken  —— 设备凭据，换 ticket 的唯一凭据（丢了就要重新扫码）；
 *   · deviceId     —— 审批推送订阅需要它，且 pair/refresh 会回填；
 *   · accessToken  —— 旧手输路径的共享 token（没有 deviceToken 时是唯一凭据）。
 *
 * 刻意**不含**：ticket（一次性，本来就不落盘）、updateSource（用户偏好，
 * 清了等于重置下拉）、deviceName 之类展示性字段 —— 清完之后按默认值重建即可。
 */
export const PAIRING_KEYS = ["relayUrl", "deviceToken", "deviceId", "accessToken"];

/**
 * 从配置里裁出「配对信息」本身：只保留 [PAIRING_KEYS] 里**有真实值**的项。
 *
 * 两个刻意的取舍：
 *   · 空串 / null / undefined 一律丢弃，不留 `deviceToken: ""` 这种假字段 ——
 *     下游 `if (cfg.deviceToken)` 判断会把它当有凭据，然后在 pair/refresh 上
 *     撞 401，报成"设备凭据已被撤销"，把用户引向错误的结论。
 *   · 不认识的键一并不带（管理面收口：清理函数不是配置搬运工）。
 */
export function pickPairingConfig(cfg) {
  const src = cfg && typeof cfg === "object" ? cfg : {};
  const out = {};
  for (const key of PAIRING_KEYS) {
    const v = src[key];
    if (typeof v === "string" && v.trim()) out[key] = v.trim();
  }
  return out;
}

/**
 * 配对信息是否**足以免扫码重连**。
 *
 * 判据只有两条路：要么有设备凭据（deviceToken），要么有旧的共享 token
 * （accessToken）。两样都没有 = 清完之后必须回设置页手输/扫码，界面必须
 * 提前说清楚，而不是清完才告诉用户。
 */
export function hasReconnectCredential(pairing) {
  const p = pairing && typeof pairing === "object" ? pairing : {};
  return Boolean((p.deviceToken && p.deviceToken.trim()) || (p.accessToken && p.accessToken.trim()));
}

/**
 * 给用户看的确认文案：清什么、留什么、清完会怎样。
 *
 * 为什么不让调用方自己拼：这三句话是**产品承诺**（留配对、要重新登录、
 * 可能仍有个别站点残留），拼错一处就是骗人。而且它们必须与
 * [describeWipeOutcome] 的措辞一致，否则「清之前说的」和「清之后报的」
 * 会互相矛盾。
 */
export function wipeConfirmText(pairing) {
  const keeps = hasReconnectCredential(pairing)
    ? "配对信息（Relay 地址与设备凭据）会保留，清完自动重新登录 dsh，不用再扫码。"
    : "这台设备没有可用的配对凭据，清完之后需要回到设置页扫码或填写 token。";
  return [
    "清除本 App 里所有的网站数据？",
    "",
    "会清掉：所有站点的 cookie、localStorage、IndexedDB、表单与缓存。",
    keeps,
    "",
    "外部站点（Trae / WorkBuddy / 华为云等）的登录会全部失效，下次打开要重新登录。",
  ].join("\n");
}

/**
 * 清理结果的措辞（单一来源，前端只负责渲染）。
 *
 * 诚实原则：`ok === true` 才说"已清空"；有残留就点名残留站点，不许把
 * 「我调用了整店 API」当成「清干净了」。Android 的复核只能覆盖已知登录主机
 * （CookieManager 没有枚举 API），这一点也要如实标注，别让用户以为已经全查过。
 */
export function describeWipeOutcome(res, platform) {
  const r = res && typeof res === "object" ? res : {};
  // 后端字段可能缺失（旧内核）→ 按"未知"处理，绝不兜底成 0 枚残留。
  const remaining = Number.isFinite(Number(r.remaining)) ? Number(r.remaining) : null;
  const hosts = Array.isArray(r.remainingHosts) ? r.remainingHosts.filter((h) => typeof h === "string" && h) : [];

  if (r.ok === true && remaining === 0) {
    const parts = ["已清除全部站点数据"];
    if (hasReconnectCredential(r.pairing)) parts.push("配对信息已保留");
    return { text: parts.join("；"), tone: "ok" };
  }
  if (remaining === null) {
    // 复核数字没回来：既不能说成功，也不能编一个残留数。
    return { text: "清理已执行，但没有拿到复核结果；请点「重新扫描」确认", tone: "bad" };
  }
  const where = hosts.length ? `（${hosts.slice(0, 3).join("、")}）` : "";
  const scope = platform === "android"
    ? "已探测的登录站点里仍有"
    : "仍有";
  return { text: `清理后${scope} ${remaining} 项残留${where}，可点「重新扫描」再清一次`, tone: "bad" };
}
