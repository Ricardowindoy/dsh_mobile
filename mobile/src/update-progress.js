// 更新下载进度的纯函数层（可单测，禁止碰 document）：
// DOM 状态机在 main.js 的 renderUpdateProgress / resetUpdateProgress。

// 百分比用 floor：只有 received >= total 才可能显示 100。
// 「四舍五入到 100% 但下载还没完」是把没发生的事画成事实。
export function pctOf(received, total) {
  const r = Number(received);
  const t = Number(total);
  if (!Number.isFinite(r) || r < 0) return null;
  if (!Number.isFinite(t) || t <= 0) return null; // 无 Content-Length → 不定态
  return Math.min(100, Math.max(0, Math.floor((r / t) * 100)));
}

export function formatBytes(n) {
  const v = Number(n);
  if (!Number.isFinite(v) || v < 0) return "";
  if (v < 1024 * 1024) return `${(v / 1024).toFixed(1)} KB`;
  return `${(v / 1024 / 1024).toFixed(1)} MB`;
}

// 进度条下的数字行：定态报「42% · 12.3 MB / 29.5 MB」；
// 不定态（服务器没给 Content-Length）只报已收字节，绝不编造百分比。
export function progressLabel(pct, received, total) {
  const got = formatBytes(received);
  if (pct === null) return got;
  return `${pct}% · ${got} / ${formatBytes(total)}`;
}
