// dsh-mobile 客户端日志：内存 ring buffer + 推送到 Relay。
// 用于诊断"软键盘不顶起输入框"等 WebView 环境问题。
// 日志同时镜像到 localStorage，这样跨导航（setup 页 → GUI 页是不同
// document，内存缓冲会丢）和跨启动都能保留，方便下次打开 setup 页时
// 用"推送日志"按钮上报。
const MAX_LOGS = 200;
const LOG_KEY = "dsh-mobile-logs";
const logs = [];

// 启动时恢复上一会话日志，便于跨启动保留 boot-timing 等关键打点。
try {
  const prev = localStorage.getItem(LOG_KEY);
  if (prev) {
    for (const l of prev.split("\n")) {
      if (l) logs.push(l);
    }
    while (logs.length > MAX_LOGS) logs.shift();
  }
} catch {
  // 隐私模式 / 容量不足时忽略
}

let persistTimer = null;

export function persistLogs() {
  try {
    localStorage.setItem(LOG_KEY, getLogs());
  } catch {
    // ignore
  }
}

// WebView 侧诊断（diagnostics.js）在 app 模块之前就装了全局钩子；这里把它
// 接进同一个 ring buffer / 同一个上报通道。只接一次。
let diagAttached = false;
function attachDiagnostics() {
  if (diagAttached) return;
  diagAttached = true;
  try {
    window.__dshDiag?.__attach?.((line) => {
      logs.push(line);
      if (logs.length > MAX_LOGS) logs.shift();
    });
  } catch {
    // 非 WebView 环境（浏览器预览）忽略
  }
}

export function log(msg) {
  attachDiagnostics()
  const line = `${new Date().toISOString().slice(11, 23)} ${msg}`;
  logs.push(line);
  if (logs.length > MAX_LOGS) logs.shift();
  console.log("[dsh]", line);
  // 防抖镜像，避免键盘动画期间 resize 高频触发 localStorage 写。
  if (!persistTimer) {
    persistTimer = setTimeout(() => {
      persistTimer = null;
      persistLogs();
    }, 500);
  }
}

export function getLogs() {
  return logs.join("\n");
}

// 安装全局诊断监听：视口变化（键盘弹起/收起的关键信号）、焦点、
// 窗口尺寸变化。
export function installDiagnostics(relayUrlGetter) {
  const report = (label) => {
    const vv = window.visualViewport;
    log(
      `${label} vv=${vv ? `${Math.round(vv.width)}x${Math.round(vv.height)}@${Math.round(vv.offsetTop)}` : "n/a"}` +
        ` inner=${window.innerWidth}x${window.innerHeight}` +
        ` outer=${window.outerWidth}x${window.outerHeight}` +
        ` dpr=${window.devicePixelRatio}`,
    );
  };
  report("boot");
  window.addEventListener("resize", () => report("resize"));
  if (window.visualViewport) {
    window.visualViewport.addEventListener("resize", () => report("visualViewport.resize"));
    window.visualViewport.addEventListener("scroll", () => report("visualViewport.scroll"));
  }
  document.addEventListener("focusin", (e) => {
    const t = e.target;
    log(`focusin tag=${t.tagName} id=${t.id || "?"} type=${t.type || "?"}`);
    report("focusin");
  });
  document.addEventListener("focusout", () => report("focusout"));
  window.addEventListener("error", (e) => log(`window.error ${e.message}`));
  window.addEventListener("unhandledrejection", (e) => {
    // e.reason 常见是 Error 对象，直接模板插值会得到 "[object Object]"。
    const r = e.reason;
    const text = typeof r === "string" ? r : (r && (r.message || r.name)) || String(r);
    log(`unhandledrejection ${text}`);
  });
  // 周期采样 2s，捕捉键盘动画中间态
  setInterval(() => {
    const active = document.activeElement;
    if (active && (active.tagName === "INPUT" || active.tagName === "TEXTAREA")) {
      report("sample(active-input)");
    }
  }, 2000);
  void relayUrlGetter;
}

// 推送日志到 Relay：POST /__dsh_tunnel__/app-log。
// relay 开了鉴权时该端点要求凭据（设备 token 或旧 AccessToken），token 由
// 调用方传入并走 header——不进 URL。未传 token 时保持旧行为（开放 relay）。
export async function pushLogs(relay, token) {
  const body = getLogs();
  try {
    const headers = { "Content-Type": "text/plain" };
    if (token) headers["X-DSH-Relay-Token"] = token;
    const resp = await fetch(`${relay}/__dsh_tunnel__/app-log`, {
      method: "POST",
      headers,
      body,
    });
    return resp.ok;
  } catch {
    return false;
  }
}
