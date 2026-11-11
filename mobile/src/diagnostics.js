// WebView 侧连接诊断：把"App 打开/切后台/联网变化/加载失败/重载次数"
// 这些只存在于设备端的事实记进 ring buffer，随「推送日志」或导航前
// sendBeacon 一起上报到 Relay。手机不在局域网时，这些是唯一能看到
// 客户端到底走到哪一步的证据。
//
// 本文件必须在 app 模块之前加载（index.html 里放在 type=module 之前），
// 这样 window.onerror 能覆盖模块加载期就发生的失败（例如 NSC 没生效、
// 证书被拒导致的资源加载失败）。

const BOOT_KEY = "dsh-mobile-boot-count";
const LAST_KEY = "dsh-mobile-last-boot";
const recent = new Set();

// bootCount：本次 document 是第几次加载。数目突然增长说明页面被反复重载
// （WebView 崩了、导航失败、连接断了触发重新进站）。
let bootCount = 0;
try {
  bootCount = (parseInt(localStorage.getItem(BOOT_KEY) || "0", 10) || 0) + 1;
  localStorage.setItem(BOOT_KEY, String(bootCount));
  localStorage.setItem(LAST_KEY, new Date().toISOString());
} catch {
  // 隐私模式/容量不足：忽略，不影响启动
}

const state = {
  bootCount,
  online: typeof navigator.onLine === "boolean" ? navigator.onLine : null,
  startedAt: Date.now(),
  hiddenAt: null,
};

function brief(v) {
  const s = typeof v === "string" ? v : (v && (v.message || v.name)) || String(v);
  const clean = String(s).replace(/\s+/g, " ").trim();
  return clean.length > 160 ? clean.slice(0, 157) + "..." : clean;
}

function record(msg) {
  if (recent.has(msg)) return;
  recent.add(msg);
  window.__dshDiag?.log?.(msg);
}

// 早期事件（app 模块加载完之前发生的）先攒着，logger 接上后一次性吐出去。
const pending = [];
let sink = null;

function emit(line) {
  const stamped = `${new Date().toISOString().slice(11, 23)} ${line}`;
  if (sink) sink(stamped);
  else if (pending.length < 50) pending.push(stamped);
}

function attach(fn) {
  if (sink) return;
  sink = fn;
  while (pending.length) fn(pending.shift());
}

window.__dshDiag = {
  state,
  record,
  log: emit,
  __attach: attach,
  snapshot() {
    return `bootCount=${bootCount} online=${state.online} hidden=${state.hiddenAt ? "yes" : "no"}`;
  },
};

window.addEventListener("online", () => {
  state.online = true;
  record(`net online after ${Math.round((Date.now() - state.startedAt) / 1000)}s`);
});
window.addEventListener("offline", () => {
  state.online = false;
  record("net offline");
});
window.addEventListener("error", (e) => {
  const where = e.target;
  if (where && where !== window && (where.src || where.href)) {
    record(`resource error ${where.tagName} ${brief(where.src || where.href)}`);
    return;
  }
  record(`window.error ${brief(e.message || e.error)}`);
});
window.addEventListener("unhandledrejection", (e) => {
  record(`unhandledrejection ${brief(e.reason)}`);
});
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "hidden") {
    state.hiddenAt = Date.now();
  } else {
    const gone = state.hiddenAt ? Math.round((Date.now() - state.hiddenAt) / 1000) : 0;
    state.hiddenAt = null;
    record(`visible again after ${gone}s hidden`);
  }
});
window.addEventListener("load", () => {
  const nav = performance.getEntriesByType?.("navigation")?.[0];
  if (nav) {
    record(
      `load nav type=${nav.type} dom=${Math.round(nav.domContentLoadedEventEnd)}ms` +
        ` load=${Math.round(nav.loadEventEnd || nav.duration)}ms`,
    );
  }
});
