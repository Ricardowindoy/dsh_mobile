// dsh mobile: a thin Tauri shell around the dsh Web GUI served through the
// public relay. The first screen collects the relay URL and access token;
// after that the WebView navigates to the relay login endpoint, which
// plants the auth cookie and redirects into the GUI.

// 诊断必须最早执行（同一次脚本求值内先跑 diagnostics 再跑其余逻辑），
// 这样 window.onerror/resource error 能覆盖后续模块的错误；也因此把它并进
// 主入口而不是 HTML 里再挂一个 <script>——附加 script 不会被 Vite 处理，
// dist 里会 404（实测），诊断会静默失效。
import "./diagnostics.js";
import { log, installDiagnostics, pushLogs, getLogs, persistLogs } from "./logger.js";
import {
  GITHUB_RELEASES_API,
  GITEE_RELEASES_API,
  githubUpdateFromRelease,
  giteeUpdateFromRelease,
  parseChecksum,
  normalizeSource,
} from "./update-source.js";
import { pctOf, progressLabel } from "./update-progress.js";

const setupEl = document.getElementById("setup");
const statusEl = document.getElementById("status");
const errorEl = document.getElementById("error");
const errorTextEl = document.getElementById("error-text");
const relayInput = document.getElementById("relay-url");
const tokenInput = document.getElementById("access-token");
const tokenToggleEl = document.getElementById("token-toggle");
const iconEyeEl = document.getElementById("icon-eye");
const iconEyeOffEl = document.getElementById("icon-eye-off");
const appVersionEl = document.getElementById("app-version");
const updateEl = document.getElementById("update");
const updateVersionEl = document.getElementById("update-version");
const updateMetaEl = document.getElementById("update-meta");
const updateNoteEl = document.getElementById("update-note");
const updateDownloadEl = document.getElementById("update-download");
const updateDownloadLabelEl = document.getElementById("update-download-label");
const updateLaterEl = document.getElementById("update-later");
const updateProgressEl = document.getElementById("update-progress");
const updateProgressBarEl = document.getElementById("update-progress-bar");
const updateProgressCountEl = document.getElementById("update-progress-count");
const updateSourceEl = document.getElementById("update-source");
const pushLogBtn = document.getElementById("push-log");
const pushLogStatus = document.getElementById("push-log-status");
const selfCheckBtn = document.getElementById("self-check");
const selfCheckStatusEl = document.getElementById("self-check-status");

installDiagnostics();
installPressFeedback();
installStripThemeSync();

const STORAGE_KEY = "dsh-mobile-config";
const FALLBACK_VERSION = "0.1.0";

// --- 状态栏条带 = 页面画布（单一真值来源） ------------------------------------
// 原生 decorView 的底色与页面 --set-bg 若各算各的（一个查 Android 资源的 -night
// 限定，一个查 WebView 的 prefers-color-scheme），两套判定一旦不一致就会出现
// "页面浅色、条带深色"的分层断裂，而且这种不一致只在真机上看得见、CI 抓不到。
// 所以让页面把自己**实际解析到的**画布色推给原生：不一致的可能性被消掉；退化方式
// 也是良性的——万一这台设备的 WebView 不跟随系统深色，页面与条带会一起留在浅色，
// 而不是错开。values(-night)/colors.xml 因此只当 JS 起来之前的初值。
function installStripThemeSync() {
  const channels = (rgb) => {
    const m = String(rgb).match(/(\d+)\s*,\s*(\d+)\s*,\s*(\d+)/);
    return m ? [+m[1], +m[2], +m[3]] : null;
  };
  const toHex = (rgb) => {
    const c = channels(rgb);
    return c ? "#" + c.map((v) => v.toString(16).padStart(2, "0")).join("") : null;
  };
  // 亮度判据与原生 isLightColor 同式（WCAG 线性化 + Rec.709），但**极性相反**，
  // 这是两个 API 的命名差异，不是笔误：
  //   · Kotlin 里 isAppearanceLightStatusBars=true → 深色图标（浅底用）；
  //   · set_status_theme 的 lightIcons=true → 浅色(白)图标（深底用）。
  // 参照物是既有 connect()：进白底 GUI 时传的是 lightIcons:false。搞反就会在浅
  // 色条带上画白色图标，肉眼不可见。
  const isLight = (rgb) => {
    const c = channels(rgb);
    if (!c) return true;
    const f = (v) => { const x = v / 255; return x <= 0.04045 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4; };
    return 0.2126 * f(c[0]) + 0.7152 * f(c[1]) + 0.0722 * f(c[2]) > 0.5;
  };
  const push = () => {
    // isLight 吃的是 computed style 的 rgb(...) 原文，不是转好的 #rrggbb：
    // channels() 靠逗号取三通道，喂它十六进制串会解析失败并静默返回"浅底"，
    // 结果深色模式下发出 lightIcons:false（= 黑底画黑图标，肉眼不可见）。
    const rgb = getComputedStyle(document.body).backgroundColor;
    const bg = toHex(rgb);
    if (!bg) return;
    tauriInvoke("set_status_theme", { bg, lightIcons: !isLight(rgb) }).catch(() => {
      // 非 Tauri 环境（浏览器预览）没有这条命令，忽略即可
    });
  };
  push();
  // 系统切明暗时 WebView 不重建 Activity（configChanges 含 uiMode），
  // 由这里重推一次；connect() 之后前端会再推 GUI 的底色，后写覆盖先写。
  window.matchMedia?.("(prefers-color-scheme: dark)")
    ?.addEventListener?.("change", push);
}

// --- 按压反馈（apple-design 第一条：杀掉延迟） --------------------------------
// 反馈必须出现在 pointerdown，不是 click/抬起之后；:active 在 WebView 上有
// 时序问题（首次触摸可能不触发、滚动后钉住不放），所以用显式 class 挂/摘，
// :active 只当兜底。只加类、绝不 preventDefault —— 不抢手势、不阻断输入，
// 动画从当前屏幕上的值继续（可打断）。
function installPressFeedback() {
  const SELECTOR = ".btn-primary, .btn-secondary, .icon-btn, .field-action";
  const release = (el) => el && el.classList.remove("is-pressed");

  document.addEventListener("pointerdown", (event) => {
    const el = event.target.closest?.(SELECTOR);
    if (!el || el.disabled) return;
    el.classList.add("is-pressed");
    const off = () => {
      release(el);
      document.removeEventListener("pointerup", off);
      document.removeEventListener("pointercancel", off);
    };
    document.addEventListener("pointerup", off);
    document.addEventListener("pointercancel", off);
  });

  // App 进后台时 pointerup 不会送达，否则会留下"一直按着"的视觉状态。
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "hidden") {
      document.querySelectorAll(".is-pressed").forEach(release);
    }
  });
}

// 状态文案：tone 把结果落成可见语义（绿=通过、红=失败），
// 不靠 tooltip 或"看起来差不多"。缺省 = 中性灰。
function setStatusText(el, text, tone) {
  el.textContent = text;
  el.hidden = false;
  el.classList.remove("status-pills");
  if (tone) el.dataset.tone = tone;
  else delete el.dataset.tone;
}

// 连接自检结果：一条事实一枚胶囊（2026-11-08 控件规范 .wb-badge）。
// 骨架 = 色点 currentColor + 12px/500 + --set-radius-sm + 3px 10px；
// 颜色只是冗余编码，词本身承载语义；每枚都可以独立换行，窄屏不横向溢出。
function setStatusPills(el, items) {
  el.textContent = "";
  el.hidden = false;
  el.classList.add("status-pills");
  delete el.dataset.tone;
  for (const item of items) {
    const pill = document.createElement("span");
    pill.className = "status-pill" + (item.tone ? ` status-pill--${item.tone}` : "");
    const dot = document.createElement("i");
    pill.appendChild(dot);
    const label = document.createElement("span");
    label.textContent = item.text;
    pill.appendChild(label);
    el.appendChild(pill);
  }
}

// --- 应用内自动更新 ----------------------------------------------------------
// 流程：启动预检发现新版本 → 后台下载（Rust 流式 + SHA-256 校验）→
// 经 JNI FileProvider intent 拉起系统安装器。用户可「跳过此版本」直接进
// GUI，跳过按版本号记在 localStorage；manifest force=true 不受跳过影响。
const SKIPPED_KEY = "dsh-mobile-skipped-version";
// 下载源偏好：用户在下拉里选的 gitee/github/relay。存 localStorage（同步可读，
// 预检时就要用）；Rust config 里也镜像一份 cfg.updateSource，双写兜底。
const UPDATE_SOURCE_KEY = "dsh-mobile-update-source";
let activeUpdate = null;   // checkUpdate 返回的更新信息
let downloadedApk = null;  // {path,size} 已下载且校验通过的本地 APK
let lastCfg = null;        // 最近一次连接配置（跳过版本后直接进 GUI 用）

// 读记用户选的下载源（已规整为合法值；无记录回落 github）。
function getUpdateSource() {
  try {
    return normalizeSource(localStorage.getItem(UPDATE_SOURCE_KEY));
  } catch {
    return normalizeSource(null);
  }
}

// 记住用户选的下载源：localStorage 同步写 + 镜像进 Rust config（异步）。
function setUpdateSource(source) {
  const s = normalizeSource(source);
  try { localStorage.setItem(UPDATE_SOURCE_KEY, s); } catch { /* ignore */ }
  try {
    const cfg = (lastCfg && typeof lastCfg === "object") ? lastCfg : loadConfig();
    if (cfg && typeof cfg === "object") {
      cfg.updateSource = s;
      lastCfg = cfg;
      saveConfig(cfg);
    }
  } catch { /* ignore */ }
  return s;
}

async function tauriInvoke(cmd, args) {
  const { invoke } = await import("@tauri-apps/api/core");
  return invoke(cmd, args);
}

// 进度通道：tauri::ipc::Channel 的 JS 对端。拿不到 Channel 类（纯浏览器
// 预览/旧 API）时返回 undefined——调用方据此降级为纯文字进度。
async function makeProgressChannel(onMessage) {
  try {
    const { Channel } = await import("@tauri-apps/api/core");
    if (typeof Channel !== "function") return undefined;
    const ch = new Channel();
    ch.onmessage = onMessage;
    return ch;
  } catch {
    return undefined;
  }
}

function errMsg(e) {
  return typeof e === "string" ? e : (e && e.message) || String(e);
}

function openExternal(url) {
  import("@tauri-apps/plugin-opener")
    .then((m) => m.openUrl(url))
    .catch(() => window.open(url, "_blank", "noopener,noreferrer"));
}

// 全局启动计时器：从脚本执行起点开始计时，boot/connect 各处打点，
// 用于定位"打开 App → 连上 dsh"这段里是 WebView 冷启动、控制面请求
// 还是登录→导航跳转在耗时。打点统一进 ring buffer，可点"推送日志"上报。
const bootT0 = performance.now();
function bootMark(label) {
  const ms = Math.round(performance.now() - bootT0);
  log(`boot-timing ${label} +${ms}ms`);
}

// 持久化优先走 Rust 侧文件（WebView localStorage 在 Android 上不保证
// 跨重启存活），localStorage 作为兜底与镜像。
async function invokeSave(cfg) {
  try {
    const { invoke } = await import("@tauri-apps/api/core");
    await invoke("save_config", { config: cfg });
  } catch {
    // 非 Tauri 环境（纯浏览器预览）忽略
  }
}

async function invokeLoad() {
  try {
    const { invoke } = await import("@tauri-apps/api/core");
    const cfg = await invoke("load_config");
    if (cfg && typeof cfg === "object") return cfg;
  } catch {
    // ignore
  }
  return null;
}

function loadConfig() {
  try {
    return JSON.parse(localStorage.getItem(STORAGE_KEY)) || {};
  } catch {
    return {};
  }
}

function saveConfig(cfg) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(cfg));
  } catch {
    // ignore
  }
  void invokeSave(cfg);
}

function normalizeRelay(url) {
  return (url || "").trim().replace(/\/+$/, "");
}

async function appVersion() {
  try {
    const { getVersion } = await import("@tauri-apps/api/app");
    return await getVersion();
  } catch {
    return FALLBACK_VERSION;
  }
}

// 版本胶囊只显示"确实拿到的版本号"；空/null/undefined 一律整块隐藏。
// 兜底成 "vnull" 会把一个缺失的事实渲染成看起来像事实的字符串。
function renderVersion(raw) {
  const v = typeof raw === "string" ? raw.trim() : "";
  if (!v || /^(null|undefined)$/i.test(v)) {
    appVersionEl.hidden = true;
    appVersionEl.textContent = "";
    return;
  }
  appVersionEl.hidden = false;
  appVersionEl.textContent = `v${v}`;
}

function platformArch() {
  const ua = navigator.userAgent || "";
  if (/(aarch64|arm64)/i.test(ua)) return { platform: "android", arch: "arm64" };
  if (/x86_64|amd64/i.test(ua)) return { platform: "android", arch: "x86_64" };
  return { platform: "android", arch: "arm64" };
}

function formatSize(size) {
  if (!size) return "";
  return `${(size / 1024 / 1024).toFixed(1)} MB`;
}

// 更新源（用户 2026-11-11 定）：下拉里三选一 gitee / github / relay，选择持久化。
//   每个源独立实现「取最新 release → 映射成 update 对象」；取不到更新返回 null，
//   连不上/载荷异常抛错。checkUpdate(source) 按用户所选源执行，不再隐式回退——
//   失败就如实报错并提示换源（隐式回退会让用户以为当前源能用）。
// ⚠️ 曾经的失败模式（设备日志 .diag/app8.log 实证）：选 github 时 release 下载
//   17s 后「连接更新服务器失败」，一个字节都没到 → 进度条永远不显示。根因是源
//   不可达，不是进度条代码。现在把源交到用户手里，relay 作可靠兜底。
const UPDATE_GH_TIMEOUT_MS = 6000;
const UPDATE_GITEE_TIMEOUT_MS = 8000;
const UPDATE_RELAY_TIMEOUT_MS = 8000;

// 展示名（报错/提示文案用）。
const SOURCE_LABEL = { gitee: "Gitee", github: "GitHub", relay: "Relay" };

// 取校验文件（可选）：拿不到就保留空 sha256，卡片如实说“未提供校验”。
async function fillChecksum(update, timeoutMs) {
  if (!update || !update.checksumUrl) return update;
  try {
    const cr = await fetchWithTimeout(update.checksumUrl, timeoutMs);
    if (cr.ok) update.sha256 = parseChecksum(await cr.text());
  } catch { /* 忽略：仍可下载，仅缺校验 */ }
  delete update.checksumUrl;
  return update;
}

async function checkGitHubUpdate() {
  const current = await appVersion();
  const resp = await fetchWithTimeout(GITHUB_RELEASES_API, UPDATE_GH_TIMEOUT_MS, {
    headers: { Accept: "application/vnd.github+json" },
  });
  if (!resp.ok) throw new Error(`GitHub HTTP ${resp.status}`);
  const update = githubUpdateFromRelease(await resp.json(), current);
  return update ? fillChecksum(update, UPDATE_GH_TIMEOUT_MS) : null;
}

// Gitee：releases?per_page=1 取最新一条（Gitee 无 latest 端点）；空数组=无 release。
async function checkGiteeUpdate() {
  const current = await appVersion();
  const resp = await fetchWithTimeout(GITEE_RELEASES_API, UPDATE_GITEE_TIMEOUT_MS, {
    headers: { Accept: "application/json" },
  });
  if (!resp.ok) throw new Error(`Gitee HTTP ${resp.status}`);
  const arr = await resp.json();
  const release = Array.isArray(arr) ? arr[0] : null;
  if (!release) return null; // 该源暂无 release → 无更新
  const update = giteeUpdateFromRelease(release, current);
  return update ? fillChecksum(update, UPDATE_GITEE_TIMEOUT_MS) : null;
}

// Relay：GET /__dsh_tunnel__/update/check?platform=&arch=&appVersion=
async function checkRelayUpdate(relay) {
  const version = await appVersion();
  const { platform, arch } = platformArch();
  const url =
    `${relay}/__dsh_tunnel__/update/check` +
    `?platform=${platform}&arch=${arch}&appVersion=${encodeURIComponent(version)}`;
  const resp = await fetchWithTimeout(url, UPDATE_RELAY_TIMEOUT_MS);
  if (!resp.ok) throw new Error(`检查更新失败（HTTP ${resp.status}）`);
  const body = await resp.json();
  if (body.code === 204 || body.code !== 200 || !body.version || !body.url) {
    return null;
  }
  body.source = body.source || "relay";
  return body;
}

// 按用户所选源检查更新。source 缺省=用户持久化偏好。
// 返回 {update, detail}：detail 说明「查了哪个源、结果是怎样」，供卡片如实展示。
async function checkUpdate(relay, sourceOverride) {
  const source = normalizeSource(sourceOverride || getUpdateSource());
  try {
    let update = null;
    if (source === "gitee") update = await checkGiteeUpdate();
    else if (source === "relay") update = await checkRelayUpdate(relay);
    else update = await checkGitHubUpdate();
    if (update) {
      log(`update check: source=${source} v${update.version}`);
      return { update, detail: `已从 ${SOURCE_LABEL[source]} 发现 v${update.version}` };
    }
    log(`update check: source=${source}, already up to date`);
    return { update: null, detail: `当前已是最新（源：${SOURCE_LABEL[source]}）` };
  } catch (e) {
    log(`update check: source=${source} unavailable (${errMsg(e)})`);
    return {
      update: null,
      error: `${SOURCE_LABEL[source]} 不可达：${errMsg(e)}`,
      source,
    };
  }
}

/* 更新卡按钮已图形化（用户要求：不用文字按钮）：
 * 主按钮保留 label（状态语义必须可读：下载中/立即安装/重试下载），左侧图标随
 * data-state 切换（download / install / retry），图标只是冗余编码。
 * 「跳过此版本」是纯图标按钮（X），语义放 aria-label/title。 */
function setUpdateButtons(primaryLabel, primaryAction, laterLabel) {
  updateDownloadEl.hidden = primaryLabel === null;
  if (primaryLabel !== null) {
    updateDownloadLabelEl.textContent = primaryLabel;
    const state = primaryLabel.includes("安装") ? "install"
      : primaryLabel.includes("重试") ? "retry"
      : "download";
    updateDownloadEl.dataset.state = state;
    updateDownloadEl.setAttribute("aria-label", primaryLabel);
    updateDownloadEl.onclick = primaryAction;
  }
  updateLaterEl.setAttribute("aria-label", laterLabel);
  updateLaterEl.setAttribute("title", laterLabel);
}

// 拉起系统安装器；应用内失败时兜底改用系统浏览器下载。
async function launchInstaller() {
  if (!downloadedApk) return;
  updateNoteEl.textContent = "正在拉起系统安装器…";
  try {
    await tauriInvoke("install_update", { path: downloadedApk.path });
    log("installer launched");
    updateNoteEl.textContent =
      "已在系统安装器中打开。按提示确认安装；装完重新打开 App 即为新版本。";
  } catch (e) {
    const msg = errMsg(e);
    log(`installer failed: ${msg}`);
    // 不自动拉浏览器：旧版本安装器失效时，自动跳转会让用户以为 App 闪退。
    // 改成显式按钮，用户自己决定什么时候去浏览器下载。
    updateNoteEl.textContent =
      `应用内安装未成功：${msg}\n可点「用浏览器下载」手动安装（装好这一版后，后续版本可直接在应用内安装）。`;
    setUpdateButtons("用浏览器下载", () => { void openExternal(activeUpdate.url); }, "跳过此版本");
  }
}

// 进度条渲染：progress = {received, total?}。定态画 scaleX + 百分比；
// 不定态（total 缺失）走条纹动画，数字行只报已收字节——绝不编造百分比。
// aria-valuenow 只在定态时设置；不定态没有可声明的数值。
function renderUpdateProgress(progress) {
  const received = progress && progress.received;
  const total = progress && progress.total;
  const pct = pctOf(received, total);
  updateProgressEl.hidden = false;
  updateProgressEl.classList.toggle("indeterminate", pct === null);
  if (pct === null) {
    updateProgressEl.removeAttribute("aria-valuenow");
    updateProgressBarEl.style.transform = "";
  } else {
    updateProgressEl.setAttribute("aria-valuenow", String(pct));
    updateProgressBarEl.style.transform = `scaleX(${pct / 100})`;
  }
  updateProgressCountEl.hidden = false;
  updateProgressCountEl.textContent = progressLabel(pct, received, total);
}

function resetUpdateProgress() {
  updateProgressEl.hidden = true;
  updateProgressEl.classList.remove("indeterminate");
  updateProgressEl.removeAttribute("aria-valuenow");
  updateProgressBarEl.style.transform = "";
  updateProgressCountEl.hidden = true;
  updateProgressCountEl.textContent = "";
}

// 后台下载 → SHA-256 校验 → 自动拉起安装器；失败给出「重试下载」。
// 进度经 tauri::ipc::Channel 由 Rust 逐块推送（download_update 的 on_progress）。
// Channel 不可用（纯浏览器预览）时降级为旧体验：只有文字提示，不传通道参数。
async function startAutoUpdate(update) {
  downloadedApk = null;
  resetUpdateProgress();
  updateNoteEl.textContent = "正在后台下载更新包…";
  setUpdateButtons("下载中…", null, "跳过此版本");
  try {
    bootMark("updateDownloadStart");
    log(`update download start v${update.version}`);
    // 通道必须先挂 onmessage 再 invoke：下载起点的事件可能在 invoke 返回前
    // 就到达，后挂监听会丢首帧。
    const channel = await makeProgressChannel(renderUpdateProgress);
    const args = {
      url: update.url,
      expectedSha256: update.sha256 || "",
    };
    if (channel) args.onProgress = channel;
    downloadedApk = await tauriInvoke("download_update", args);
    // 兜底画满：Rust 末帧事件通常已到，但事件与 promise resolve 无顺序保证，
    // 用真实 size 作 received 显式落一帧 100%，保证"完成"这个事实可见。
    renderUpdateProgress({ received: downloadedApk.size, total: downloadedApk.size });
    bootMark("updateDownloadDone");
    log(`update download ok size=${downloadedApk.size}`);
    updateNoteEl.textContent = update.sha256
      ? "下载完成，完整性校验通过。"
      : "下载完成（该版本未提供校验文件）。";
    setUpdateButtons("立即安装", () => launchInstaller(), "跳过此版本");
    // 成功时保留已满的进度条（100% 是事实，不该一闪而过就消失）；失败时
    // 才清空，避免用一条停在半路的条子把"失败"画成"还在继续"。
    launchInstaller();
  } catch (e) {
    bootMark("updateDownloadFail");
    const src = (update && update.source) || getUpdateSource();
    log(`update download fail: source=${src} ${errMsg(e)}`);
    resetUpdateProgress();
    // 显式提示换源——这正是设备日志里 GitHub release 连不上（17s 超时、
    // 一个字节没到所以进度条不显示）时用户需要看的下一步动作。
    updateNoteEl.textContent =
      `下载失败（源：${SOURCE_LABEL[src] || src}）：${errMsg(e)}。可在上方切换下载源后重试。`;
    setUpdateButtons("重试下载", () => startAutoUpdate(update), "跳过此版本");
  }
}

// 展示更新卡片并启动自动更新；用户跳过的版本静默放行直接进 GUI。
function presentUpdate(update, cfg) {
  activeUpdate = update;
  try {
    if (localStorage.getItem(SKIPPED_KEY) === update.version && !update.force) {
      log(`update v${update.version} skipped by user, entering GUI`);
      void connect(cfg, true);
      return;
    }
  } catch { /* ignore */ }
  updateVersionEl.textContent = `v${update.version}`;
  updateMetaEl.textContent = [update.releaseAt, formatSize(update.size)]
    .filter(Boolean)
    .join(" · ");
  updateLaterEl.onclick = () => {
    try { localStorage.setItem(SKIPPED_KEY, update.version); } catch { /* ignore */ }
    updateEl.hidden = true;
    void connect(lastCfg || cfg, true);
  };
  updateEl.hidden = false;
  // 启动时不自动下载：旧版本安装器不可用时，自动流程会反复下载再拉起
  // 浏览器，用户感知就是"打开就闪退"。改为显式点按「下载更新」。
  updateNoteEl.textContent = `发现新版本 v${update.version}，点下方按钮下载安装。`;
  setUpdateButtons("下载更新", () => { void startAutoUpdate(update); }, "跳过此版本");
}

// 用户在更新卡片里切换下载源：记住偏好并立即用新源重查。
// 有新版本 → 刷新卡片；无更新 → 收起卡片并进 GUI；源不可达 → 在卡片里如实报错。
async function recheckWithSource(source, cfg) {
  const s = setUpdateSource(source);
  activeUpdate = null;
  downloadedApk = null;
  resetUpdateProgress();
  updateEl.hidden = false;
  updateVersionEl.textContent = "";
  updateMetaEl.textContent = "";
  updateNoteEl.textContent = `正在检查更新（源：${SOURCE_LABEL[s]}）…`;
  setUpdateButtons(null, null, "跳过此版本");
  const relay = normalizeRelay((cfg || lastCfg || loadConfig()).relayUrl || "");
  const res = await checkUpdate(relay, s).catch((e) => ({ update: null, error: errMsg(e), source: s }));
  if (res && res.update) {
    presentUpdate(res.update, cfg || lastCfg || loadConfig());
    return;
  }
  if (res && res.error) {
    updateNoteEl.textContent = `${res.error}。可换一个源再试。`;
    setUpdateButtons(null, () => { void recheckWithSource(updateSourceEl.value, cfg); }, "跳过此版本");
    return;
  }
  // 该源暂无更新：收起卡片直接进 GUI。
  log(`update check: source=${s}, already up to date (from switch)`);
  updateEl.hidden = true;
  const base = cfg || lastCfg || loadConfig();
  if (base && base.relayUrl) void connect(base, true);
}

// 绑定下载源下拉：change 时记住偏好并按新源重查（仅在更新卡片可见时有意义）。
function bindUpdateSource() {
  if (!updateSourceEl) return;
  updateSourceEl.value = getUpdateSource();
  updateSourceEl.addEventListener("change", () => {
    const s = updateSourceEl.value;
    log(`update source switched to ${s}`);
    void recheckWithSource(s, lastCfg);
  });
}

// A dsh page may itself be served as a PWA with a service worker; if one is
// already controlling the WebView it could interfere with navigation, so
// unregister before hopping origins. Android WebViews can hang in
// getRegistrations(), so this is best-effort, timeout-capped, and must
// never block navigation.
function clearServiceWorkers() {
  const sweep = () => {
    if (!navigator.serviceWorker) return;
    const timer = setTimeout(() => {}, 0);
    navigator.serviceWorker
      .getRegistrations()
      .then((regs) => Promise.all(regs.map((r) => r.unregister().catch(() => {}))))
      .catch(() => {});
    void timer;
  };
  try {
    sweep();
  } catch {
    // ignore
  }
}

// fetch with a hard timeout (WebView fetch can hang on flaky networks).
function fetchWithTimeout(url, ms, opts = {}) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), ms);
  return fetch(url, { cache: "no-store", signal: ctrl.signal, ...opts }).finally(
    () => clearTimeout(timer),
  );
}

// 查询 Relay 是否需要访问鉴权；失败时按需要鉴权处理（宁可停在设置页）。
async function relayNeedsAuth(relay) {
  try {
    const resp = await fetchWithTimeout(`${relay}/__dsh_tunnel__/whoami`, 5000);
    if (!resp.ok) return true;
    const body = await resp.json();
    return body.authRequired === true;
  } catch {
    return true;
  }
}

// 预检设备 token（header 载体，长效凭据不进 URL）。null=通过。
async function probeDeviceToken(relay, token) {
  try {
    const resp = await fetchWithTimeout(`${relay}/__dsh_tunnel__/auth-check`, 8000, {
      headers: { "X-DSH-Relay-Token": token },
    });
    if (resp.status === 401) return "设备凭据已被撤销，请重新扫码配对";
    if (!resp.ok) return `Relay 响应异常（HTTP ${resp.status}），请检查地址`;
    return null;
  } catch {
    return "无法连接 Relay，请检查网络与地址";
  }
}

// 预检 token（返回错误文案，null=通过）。纯控制面请求，不经设备。
// 旧手输路径：token 仍走 query（兼容旧 relay），新 relay 会在日志标记弃用。
async function probeToken(relay, token) {
  try {
    const probe = `${relay}/__dsh_tunnel__/auth-check?token=${encodeURIComponent(token)}`;
    const resp = await fetchWithTimeout(probe, 8000);
    if (resp.status === 401) return "访问 token 不正确，请检查后重试";
    if (!resp.ok) return `Relay 响应异常（HTTP ${resp.status}），请检查地址`;
    return null;
  } catch {
    return "无法连接 Relay，请检查网络与地址";
  }
}

// connect: skipProbe=true 时跳过 token 预检（boot 已并行预检过）。
// cfg.deviceToken（扫码配对产物）优先于 cfg.accessToken（旧手输路径）：
// 设备 token 走 pair/refresh 换一次性 ticket，WebView 只导航 login?ticket=，
// 长效凭据不进 URL。
async function connect(cfg, skipProbe = false) {
  lastCfg = cfg;
  bootMark("connectStart");
  const relay = normalizeRelay(cfg.relayUrl);
  let next = "/";
  if (cfg.deviceToken) {
    if (!skipProbe) {
      const err = await probeDeviceToken(relay, cfg.deviceToken);
      bootMark("probeDone");
      if (err) {
        setupEl.hidden = false;
        statusEl.hidden = true;
        errorEl.hidden = false;
        errorTextEl.textContent = err;
        return;
      }
    }
    if (cfg.ticket) {
      // 扫码刚兑换的一次性 ticket：直接用，不再多铸一张。
      next = `/__dsh_tunnel__/login?ticket=${encodeURIComponent(cfg.ticket)}&next=${encodeURIComponent("/")}`;
    } else {
      // 重连：设备 token 走 header 换新 ticket；失败（被撤销等）停在设置页。
      try {
        const { invoke } = await import("@tauri-apps/api/core");
        const refreshed = await invoke("pair_refresh", { relayUrl: relay, deviceToken: cfg.deviceToken });
        next = `/__dsh_tunnel__/login?ticket=${encodeURIComponent(refreshed.ticket)}&next=${encodeURIComponent("/")}`;
      } catch (e) {
        setupEl.hidden = false;
        statusEl.hidden = true;
        errorEl.hidden = false;
        errorTextEl.textContent = errMsg(e);
        return;
      }
    }
  } else if (cfg.accessToken) {
    if (!skipProbe) {
      const err = await probeToken(relay, cfg.accessToken);
      bootMark("probeDone");
      if (err) {
        setupEl.hidden = false;
        statusEl.hidden = true;
        errorEl.hidden = false;
        errorTextEl.textContent = err;
        return;
      }
    }
    next = `/__dsh_tunnel__/login?token=${encodeURIComponent(cfg.accessToken)}&next=${encodeURIComponent("/")}`;
  } else {
    // 没填 token：先问 Relay 要不要鉴权。要鉴权就停在设置页提示，
    // 否则直接进 GUI（避免 401 白屏）。
    statusEl.hidden = false;
    setupEl.hidden = true;
    const needsAuth = await relayNeedsAuth(relay);
    if (needsAuth) {
      setupEl.hidden = false;
      statusEl.hidden = true;
      errorEl.hidden = false;
      errorTextEl.textContent = "该 Relay 需要凭据：请扫码配对，或填写访问 token";
      return;
    }
  }
  // ticket 是一次性的，不落盘（JSON.stringify 会丢掉 undefined 键）。
  saveConfig({ ...cfg, ticket: undefined });
  setupEl.hidden = true;
  statusEl.hidden = false;

  // 导航前把 boot-timing 打点落盘并自动上报。sendBeacon 无法带自定义
  // header：有凭据时用 fetch keepalive（同样能在导航后送达），开放 relay
  // 保留 sendBeacon 旧路径。凭据走 header，不进 URL。
  persistLogs();
  bootMark("navigate");
  const logToken = cfg.deviceToken || cfg.accessToken || "";
  try {
    if (logToken) {
      void fetch(`${relay}/__dsh_tunnel__/app-log`, {
        method: "POST",
        headers: { "Content-Type": "text/plain", "X-DSH-Relay-Token": logToken },
        body: getLogs(),
        keepalive: true,
      }).catch(() => {});
    } else {
      navigator.sendBeacon(`${relay}/__dsh_tunnel__/app-log`, getLogs());
    }
  } catch {
    // 旧 WebView 无 sendBeacon 时静默跳过，不影响连接。
  }

  // 进入 GUI（白底浅色主题）：切状态栏条带为白色、图标为深色，与页面一致。
  // 失败不阻断导航（旧版本 App 或非 Tauri 环境没有该命令）。
  try {
    await tauriInvoke("set_status_theme", { bg: "#ffffff", lightIcons: false });
  } catch {
    // 非 Tauri 环境或命令不存在，保持当前配色。
  }

  // 审批推送订阅（Android + 设备凭据时）：Rust 侧维持 SSE，原生通知卡片。
  // 老配置可能没存 deviceId：pair/refresh 的应答顺带返回它（新 relay），
  // 拿不到就静默跳过 —— 推送是增益不是门槛。
  if (cfg.deviceToken && /android/i.test(navigator.userAgent)) {
    try {
      const { invoke } = await import("@tauri-apps/api/core");
      let deviceId = cfg.deviceId || "";
      if (!deviceId) {
        const refreshed = await invoke("pair_refresh", { relayUrl: relay, deviceToken: cfg.deviceToken }).catch(() => null);
        deviceId = refreshed?.deviceId || "";
        if (deviceId) saveConfig({ ...cfg, deviceId, ticket: undefined });
      }
      if (deviceId) {
        await invoke("notify_start", { relayUrl: relay, deviceId, deviceToken: cfg.deviceToken });
        log("notify subscription started");
      }
    } catch (e) {
      // notify_start 现在会在 JNI/渠道初始化失败时返回 Err；日志必须说清是
      // 「启动失败」而非「跳过」，否则排查会误判为功能没启用。
      log(`notify subscription FAILED: ${errMsg(e)}`);
    }
  }

  // Navigate immediately; the service-worker sweep runs in parallel and
  // never blocks entering the GUI.
  window.location.replace(relay + next);
  clearServiceWorkers();
}

// 连接自检：三项独立事实——① Relay 可达性（毫秒级）② Relay 是否要求 token
// ③ 当前是否已有设备在线（tunnel 是否连着）。手机不在局域网时，"连不上"的
// 三种原因（地址错/网络不通、token 错、隧道断）在这里就能一次分清。
async function runSelfCheck() {
  const relay = normalizeRelay(relayInput.value.trim());
  selfCheckStatusEl.hidden = false;
  if (!relay) {
    setStatusPills(selfCheckStatusEl, [{ text: "请先填写 Relay 地址", tone: "bad" }]);
    return;
  }
  setStatusPills(selfCheckStatusEl, [{ text: "自检中…" }]);
  log(`self-check start relay=${relay}`);
  const t0 = performance.now();
  try {
    const resp = await fetchWithTimeout(`${relay}/__dsh_tunnel__/probe`, 8000);
    const ms = Math.round(performance.now() - t0);
    if (!resp.ok) {
      setStatusPills(selfCheckStatusEl, [{ text: `Relay 响应异常：HTTP ${resp.status}（${ms}ms）`, tone: "bad" }]);
      log(`self-check fail http=${resp.status} ms=${ms}`);
      return;
    }
    const body = await resp.json();
    const auth = body.authRequired ? "需要 token" : "免 token";
    // 设备离线不是"自检失败"（网络与地址其实都通了），所以用 warn 而不是 danger：
    // 琥珀胶囊只提示"链路断"，红色仍只留给请求失败。
    setStatusPills(selfCheckStatusEl, [
      { text: `Relay 可达 ${ms}ms` },
      { text: auth },
      { text: body.deviceOnline ? "设备在线" : "设备离线（tunnel 没连上）", tone: body.deviceOnline ? "ok" : "warn" },
    ]);
    log(`self-check ok ms=${ms} authRequired=${body.authRequired} deviceOnline=${body.deviceOnline}`);
  } catch (e) {
    const ms = Math.round(performance.now() - t0);
    setStatusPills(selfCheckStatusEl, [{ text: `无法连接 Relay（${ms}ms）：检查网络与地址`, tone: "bad" }]);
    log(`self-check error ms=${ms} err=${errMsg(e)}`);
  }
}

// ── 扫码配对 ─────────────────────────────────────────────────────────
// 二维码载荷 = {v:1, relay, ca, code}（host 插件 pairing.mjs 生成）。
// 安全规则：陌生 relay host 必须用户确认（防钓鱼 QR 把凭据发给攻击者）；
// 长效 deviceToken 只经 Rust invoke 流动，绝不进 URL / 日志。
// KNOWN_RELAY_HOSTS 是部署时约定的可信入口白名单（环境特定，示例见 README），
// 与手机里已保存的 relay host 一起构成「免确认」集合。
const KNOWN_RELAY_HOSTS = JSON.parse(
  (() => {
    try {
      return window.__DSH_KNOWN_RELAY_HOSTS__ ?? '["relay.example.com"]'
    } catch { return '["relay.example.com"]' }
  })()
);
const SCAN_GUARD_KEY = "dsh-scan-guard";
const scanPairBtn = document.getElementById("scan-pair");
const scanPairLabel = document.getElementById("scan-pair-label");
const scanGalleryBtn = document.getElementById("scan-gallery");
const scanPasteBtn = document.getElementById("scan-paste");
let scanning = false;

function parsePairPayload(raw) {
  let doc;
  try { doc = JSON.parse(raw); } catch { throw new Error("这不是配对二维码（无法解析）"); }
  if (!doc || doc.v !== 1 || typeof doc.relay !== "string" || typeof doc.code !== "string" || doc.code.length < 16) {
    throw new Error("这不是配对二维码（格式不符）");
  }
  let u;
  try { u = new URL(doc.relay); } catch { throw new Error("二维码里的 Relay 地址无效"); }
  if (u.protocol !== "https:" && u.protocol !== "http:") throw new Error("二维码里的 Relay 地址无效");
  return { relay: doc.relay.replace(/\/+$/, ""), code: doc.code, host: u.hostname };
}

// 把用户给的“配对来源”统一收敛成 {kind, payload}：
// 文本 → 直接当二维码内容试解析；图片（Blob）→ jsQR 静态解码。
// 相册扫码与剪贴板粘贴共用这一条入口，失败语义也一致。
async function decodePairSource(source) {
  const jsQR = (await import("jsqr")).default;
  if (source.kind === "text") {
    return { payload: parsePairPayload(source.text), raw: source.text, via: "text" };
  }
  // 图片：file input（相册）/ 粘贴的 image blob，都走同一条解码路径。
  const blob = source.blob;
  log(`pair decode image size=${blob.size} type=${blob.type}`);
  const bitmap = await createImageBitmap(blob);
  const scale = Math.min(1, 1024 / Math.max(bitmap.width, bitmap.height));
  const w = Math.max(1, Math.round(bitmap.width * scale));
  const h = Math.max(1, Math.round(bitmap.height * scale));
  const canvas = new OffscreenCanvas(w, h);
  canvas.getContext("2d").drawImage(bitmap, 0, 0, w, h);
  const { data, width, height } = canvas.getContext("2d").getImageData(0, 0, w, h);
  const code = jsQR(data, width, height, { inversionAttempts: "attemptBoth" });
  bitmap.close();
  if (!code?.data) throw new Error("图片里没有识别到二维码");
  return { payload: parsePairPayload(code.data), raw: code.data, via: "image" };
}

// 相册扫码 / 剪贴板粘贴成功后的公共后半段：host 确认 → pair_redeem → connect。
// 与相机 scanAndPair 保持同一套安全规则（陌生 host 确认）。
async function redeemPairPayload(payload, via) {
  log(`pair ${via} host=${payload.host}`); // 只记 host，不记 code
  const savedHost = (() => { try { return new URL(normalizeRelay(relayInput.value.trim())).hostname; } catch { return ""; } })();
  if (!KNOWN_RELAY_HOSTS.includes(payload.host) && payload.host !== savedHost) {
    const ok = window.confirm(`二维码指向陌生服务器：\n${payload.host}\n\n确认要向它配对吗？`);
    if (!ok) return;
  }
  scanPairLabel.textContent = "配对中…";
  const { invoke } = await import("@tauri-apps/api/core");
  const name = `Android ${await appVersion()}`;
  const redeemed = await invoke("pair_redeem", { relayUrl: payload.relay, code: payload.code, name });
  if (!redeemed?.deviceToken || !redeemed?.ticket) throw new Error("relay 配对响应缺字段");
  relayInput.value = payload.relay;
  tokenInput.value = "";
  log(`pair redeemed device=${redeemed.deviceId}`);
  await connect({ relayUrl: payload.relay, deviceToken: redeemed.deviceToken, deviceId: redeemed.deviceId, ticket: redeemed.ticket }, true);
}

// 相册选图扫码：用 WebView 文件选择器（RustWebChromeClient.onShowFileChooser
// 已支持，无需原生改动），选完交给 decodePairSource 的 jsQR 路径解码。
function scanFromGallery() {
  if (scanning) return;
  const input = document.createElement("input");
  input.type = "file";
  input.accept = "image/*";
  input.onchange = async () => {
    const file = input.files?.[0];
    input.remove();
    if (!file) return;
    scanning = true;
    scanPairBtn.disabled = true;
    if (scanGalleryBtn) scanGalleryBtn.disabled = true;
    if (scanPasteBtn) scanPasteBtn.disabled = true;
    errorEl.hidden = true;
    scanPairLabel.textContent = "识别中…";
    try {
      const { payload, via } = await decodePairSource({ kind: "image", blob: file });
      await redeemPairPayload(payload, via);
    } catch (e) {
      const msg = errMsg(e);
      if (!/cancel/i.test(msg)) {
        errorEl.hidden = false;
        errorTextEl.textContent = msg;
        log(`pair fail: ${msg}`);
      }
    } finally {
      scanning = false;
      scanPairBtn.disabled = false;
      if (scanGalleryBtn) scanGalleryBtn.disabled = false;
      if (scanPasteBtn) scanPasteBtn.disabled = false;
      scanPairLabel.textContent = "扫码配对";
    }
  };
  input.click();
}

// 剪贴板粘贴：Android 走原生 ClipboardBridge —— WebView 的
// navigator.clipboard.read() 在本设备直接 "Read permission denied"，
// readText() 又拿不到截图（图片在剪贴板里没有文本表示），所以必须原生读。
// 非 Android / 原生读失败时才退回 WebView clipboard API。
function isTauriAndroid() {
  return "__TAURI_INTERNALS__" in window && /android/i.test(navigator.userAgent);
}

function base64ToBlob(b64, mime) {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return new Blob([bytes], { type: mime || "image/png" });
}

async function readClipboardSource() {
  if (isTauriAndroid()) {
    try {
      const { invoke } = await import("@tauri-apps/api/core");
      const raw = await invoke("read_clipboard");
      const doc = typeof raw === "string" ? JSON.parse(raw) : raw;
      if (doc?.type === "image" && doc.base64) {
        log(`pair clipboard image mime=${doc.mime || "?"} b64=${doc.base64.length}`);
        return { kind: "image", blob: base64ToBlob(doc.base64, doc.mime) };
      }
      if (doc?.type === "text" && doc.text) return { kind: "text", text: String(doc.text).trim() };
      if (doc?.type === "error") throw new Error(`读取剪贴板失败：${doc.message}`);
      return null;
    } catch (e) {
      log(`pair native clipboard failed: ${errMsg(e)}`);
    }
  }
  try {
    const items = await navigator.clipboard.read();
    for (const item of items) {
      const type = item.types.find((t) => t === "image/png" || t === "image/jpeg" || t === "image/webp");
      if (type) return { kind: "image", blob: await item.getType(type) };
      if (item.types.includes("text/plain")) {
        const text = (await (await item.getType("text/plain")).text()).trim();
        if (text) return { kind: "text", text };
      }
    }
  } catch (e) {
    log(`pair clipboard read fallback: ${errMsg(e)}`);
    const text = (await navigator.clipboard.readText().catch(() => "")).trim();
    if (text) return { kind: "text", text };
  }
  return null;
}

async function scanFromPaste() {
  if (scanning) return;
  scanning = true;
  scanPairBtn.disabled = true;
  if (scanGalleryBtn) scanGalleryBtn.disabled = true;
  if (scanPasteBtn) scanPasteBtn.disabled = true;
  errorEl.hidden = true;
  scanPairLabel.textContent = "识别中…";
  try {
    const source = await readClipboardSource();
    if (!source) throw new Error("剪贴板里没有二维码图片或配对码文本");
    const { payload, via } = await decodePairSource(source);
    await redeemPairPayload(payload, via);
  } catch (e) {
    const msg = errMsg(e);
    if (!/cancel/i.test(msg)) {
      errorEl.hidden = false;
      errorTextEl.textContent = msg;
      log(`pair fail: ${msg}`);
    }
  } finally {
    scanning = false;
    scanPairBtn.disabled = false;
    if (scanGalleryBtn) scanGalleryBtn.disabled = false;
    if (scanPasteBtn) scanPasteBtn.disabled = false;
    scanPairLabel.textContent = "扫码配对";
  }
}

// ── 相机扫码（WebView getUserMedia + jsQR）──────────────────────────────
// 不再使用 barcode-scanner 插件的 scan()：插件走 MLKit/CameraX native 路径，
// 1.0.4 日志实证在 vivo/Android 16 上扫到码后 ~10s 整个进程闪退（release 包
// 拿不到 native 栈）。这里改用 WebView 自带的相机流：<video> 直接画在页面上
// （取景框可见），逐帧 drawImage 到 canvas 后交给已在相册路径验证过的 jsQR。
// 全 JS、零 native 依赖，从根上消除该崩溃。
let camSession = null;

function stopCamera() {
  const s = camSession;
  if (!s) return;
  camSession = null;
  cancelAnimationFrame(s.raf);
  clearTimeout(s.timer);
  try { s.stream.getTracks().forEach((t) => t.stop()); } catch { /* ignore */ }
  if (s.cancelBtn && s.onCancel) s.cancelBtn.removeEventListener("click", s.onCancel);
  s.wrap?.remove();
}

function openCameraOverlay(stream) {
  const wrap = document.createElement("div");
  wrap.className = "cam-overlay";
  wrap.innerHTML =
    '<video class="cam-video" playsinline muted></video>' +
    '<div class="cam-frame"></div>' +
    '<p class="cam-hint">把二维码对准框内</p>' +
    '<button class="cam-cancel" type="button">取消</button>';
  document.body.appendChild(wrap);
  const video = wrap.querySelector(".cam-video");
  video.srcObject = stream;
  video.play().catch(() => {});
  return { wrap, video, cancelBtn: wrap.querySelector(".cam-cancel") };
}

async function scanWithCamera() {
  if (!navigator.mediaDevices?.getUserMedia) {
    throw new Error("此设备 WebView 不支持网页相机，请用「相册识别」或「粘贴配对」");
  }
  const jsQR = (await import("jsqr")).default;
  log("pair camera start");
  const stream = await navigator.mediaDevices.getUserMedia({
    video: { facingMode: { ideal: "environment" }, width: { ideal: 1280 }, height: { ideal: 720 } },
    audio: false,
  });
  const { wrap, video, cancelBtn } = openCameraOverlay(stream);
  const canvas = document.createElement("canvas");
  const ctx = canvas.getContext("2d", { willReadFrequently: true });

  return await new Promise((resolve, reject) => {
    const session = { stream, wrap, cancelBtn, raf: 0, timer: 0, onCancel: null };
    camSession = session;
    const fail = (err) => { stopCamera(); reject(err); };
    session.onCancel = () => fail(new Error("cancelled"));
    cancelBtn.addEventListener("click", session.onCancel);
    session.timer = setTimeout(() => fail(new Error("scan timeout: 30 秒未识别到二维码")), 30_000);

    const tick = () => {
      if (!camSession) return; // 已被 stopCamera 清掉
      if (video.readyState >= 2) {
        const vw = video.videoWidth;
        const vh = video.videoHeight;
        if (vw && vh) {
          const scale = Math.min(1, 640 / Math.max(vw, vh));
          const w = Math.max(1, Math.round(vw * scale));
          const h = Math.max(1, Math.round(vh * scale));
          if (canvas.width !== w || canvas.height !== h) {
            canvas.width = w;
            canvas.height = h;
          }
          ctx.drawImage(video, 0, 0, w, h);
          const img = ctx.getImageData(0, 0, w, h);
          const code = jsQR(img.data, w, h, { inversionAttempts: "dontInvert" });
          if (code?.data) {
            stopCamera();
            resolve(code.data);
            return;
          }
        }
      }
      session.raf = requestAnimationFrame(tick);
    };
    session.raf = requestAnimationFrame(tick);
  });
}

async function scanAndPair() {
  if (scanning) return;
  // 崩溃哨兵（历史防线，保留）：1.0.4 的 native 相机路径曾导致闪退；
  // 现在相机改为 WebView 流 + jsQR，不再走 native，哨兵只作兜底观测。
  try { localStorage.setItem(SCAN_GUARD_KEY, String(Date.now())); } catch { /* ignore */ }
  scanning = true;
  scanPairBtn.disabled = true;
  scanPairLabel.textContent = "扫描中…";
  errorEl.hidden = true;
  try {
    const raw = await scanWithCamera();
    log("pair camera decoded");
    const payload = parsePairPayload(raw);
    await redeemPairPayload(payload, "scan");
  } catch (e) {
    const msg = errMsg(e);
    // 用户取消扫码不是错误。
    if (!/cancel/i.test(msg)) {
      errorEl.hidden = false;
      errorTextEl.textContent = msg;
      log(`pair fail: ${msg}`);
    }
  } finally {
    stopCamera();
    try { localStorage.removeItem(SCAN_GUARD_KEY); } catch { /* ignore */ }
    scanning = false;
    scanPairBtn.disabled = false;
    scanPairLabel.textContent = "扫码配对";
  }
}

// boot 时检查崩溃哨兵：残留 = 上次相机扫码没走完任何正常退出路径就死了。
// 禁用相机按钮（title 说明原因），相册/粘贴不受影响；日志带计数方便上报统计。
function installScanCrashGuard() {
  let crashed = 0;
  try { crashed = parseInt(localStorage.getItem(SCAN_GUARD_KEY) || "0", 10) || 0; } catch { /* ignore */ }
  if (!crashed) return;
  const label = new Date(crashed).toISOString();
  log(`pair scan guard: 上次相机扫码 ${label} 未正常结束（判定闪退），本次会话禁用相机扫码`);
  try { localStorage.removeItem(SCAN_GUARD_KEY); } catch { /* ignore */ }
  if (scanPairBtn) {
    scanPairBtn.disabled = true;
    scanPairBtn.title = "相机扫码在上次使用中闪退，已暂时禁用；请用「相册识别」或「粘贴配对」";
  }
  if (scanPairLabel) scanPairLabel.textContent = "相机不可用，用相册/粘贴";
}

scanPairBtn?.addEventListener("click", () => { void scanAndPair(); });
scanGalleryBtn?.addEventListener("click", scanFromGallery);
scanPasteBtn?.addEventListener("click", () => { void scanFromPaste(); });
// 哨兵检查必须在 scanPairBtn 等 const 初始化之后（函数体引用它们）。
installScanCrashGuard();
// 非 Android Tauri 环境（浏览器预览/桌面）没有相机插件：隐藏扫码区而不是点了报错。
// 桌面形态的文案与分区标题也按设计 02 收起来（桌面没有"手动连接（救急）"这一说）。
const isAndroidRuntime = /android/i.test(navigator.userAgent);
if (!("__TAURI_INTERNALS__" in window) || !isAndroidRuntime) {
  scanPairBtn?.closest("section")?.setAttribute("hidden", "");
}
if (!isAndroidRuntime) {
  const pageSub = document.getElementById("page-sub");
  if (pageSub) pageSub.textContent = "填公网 Relay 地址与访问 token，即可打开 dsh 界面。";
  document.querySelector(".group-h")?.setAttribute("hidden", "");
}

// 提交入口统一挂在 form 上：按钮（type=submit）与软键盘回车键走同一条路径，
// 既不会"回车没反应"，也不会按钮 + 回车双重提交。
const connectForm = document.getElementById("connect-form");

function submitConnect() {
  const relayUrl = relayInput.value.trim();
  if (!relayUrl) {
    errorEl.hidden = false;
    errorTextEl.textContent = "请填写 Relay 地址";
    relayInput.focus();
    return;
  }
  errorEl.hidden = true;
  log(`connect submit relay=${relayUrl} token=${tokenInput.value ? "set" : "empty"}`);
  connect({ relayUrl, accessToken: tokenInput.value.trim() });
}

connectForm.addEventListener("submit", (event) => {
  event.preventDefault();
  submitConnect();
});

// 下载源下拉（gitee/github/relay）：绑定并回填已保存偏好。
bindUpdateSource();

// token 明文/密文切换
tokenToggleEl.addEventListener("click", () => {
  const show = tokenInput.type === "password";
  tokenInput.type = show ? "text" : "password";
  iconEyeEl.hidden = show;
  iconEyeOffEl.hidden = !show;
});

// 推送日志按钮：把本地 ring buffer 日志发到 Relay 的 /__dsh_tunnel__/app-log
selfCheckBtn.addEventListener("click", () => {
  void runSelfCheck();
});

pushLogBtn.addEventListener("click", async () => {
  const relay = normalizeRelay(relayInput.value.trim() || (await invokeLoad())?.relayUrl || loadConfig().relayUrl);
  if (!relay) {
    setStatusText(pushLogStatus, "请先填写 Relay 地址", "bad");
    return;
  }
  setStatusText(pushLogStatus, "推送中…", undefined);
  const pushCfg = await invokeLoad().catch(() => null) || loadConfig();
  const ok = await pushLogs(relay, pushCfg?.deviceToken || pushCfg?.accessToken || "");
  setStatusText(pushLogStatus, ok ? "日志已推送到服务器" : "推送失败", ok ? "ok" : "bad");
  log(`push logs to ${relay} -> ${ok ? "ok" : "fail"}`);
});

// Boot: 已配置则先检查更新——有新版本时停在设置页展示更新横幅
// （下载走系统浏览器，之后点"连接 dsh"继续进入 GUI），无更新直接连入。
async function boot() {
  bootMark("start");
  // WebView 侧事实（第几次加载、是否在线、是否刚从后台回来）：手机换网络
  // 或 WebView 被系统回收后重载时，只有这里能说明"上一次为什么断了"。
  log(`webview ${window.__dshDiag?.snapshot?.() || "diagnostics unavailable"}`);
  // 顶部显示当前 App 版本（非 Tauri 环境取 FALLBACK_VERSION）。
  // 拿不到可用版本号时**整块隐藏**，而不是渲染成 "vnull"/"v" —— 版本胶囊是
  // 一个"事实"位，空值兜底成看起来像事实的字符串会误导（后端诚实≠前端诚实）。
  const bootVersion = await appVersion();
  renderVersion(bootVersion);
  // 版本打进 ring buffer：跨版本排查（"用户到底装的哪版"）只能靠这行，
  // install/update 失败时尤其关键。
  log(`app version ${bootVersion || "unknown"}`);
  // 上次进程若 panic 过，这里把落盘记录带进日志（随"推送日志"上传）。
  try {
    const { invoke } = await import("@tauri-apps/api/core");
    const crash = await invoke("read_crash_log");
    const text = typeof crash === "string" ? crash.trim() : "";
    if (text) {
      for (const line of text.split("\n").slice(0, 24)) log(`prev-run: ${line.slice(0, 300)}`);
    }
  } catch { /* 桌面/旧内核无此命令时忽略 */ }
  // 冷启动：从 Rust run() 到此刻（JS 就绪）已耗时多少，即 WebView 初始化成本，
  // 这是 boot-timing 在 JS 侧无法覆盖的部分，单独打点。
  try {
    const { invoke } = await import("@tauri-apps/api/core");
    const rustMs = await invoke("boot_elapsed_ms");
    log(`boot-timing rustColdStart +${rustMs}ms`);
  } catch {
    // 非 Tauri 环境（纯浏览器预览）忽略
  }
  // Rust 文件持久化优先（跨重启可靠），localStorage 兜底。
  let cfg = await invokeLoad();
  bootMark("configLoaded");
  if (!cfg || !cfg.relayUrl) {
    cfg = loadConfig();
  }
  if (cfg.relayUrl) {
    setupEl.hidden = false;
    // 回填已保存的地址与 token，避免每次启动都要重输。
    relayInput.value = cfg.relayUrl || "";
    tokenInput.value = cfg.accessToken || "";
    // 同步到两处，防止旧版本只写了 localStorage。
    if (cfg.relayUrl) saveConfig(cfg);
    // 并行执行更新检查与 token 预检（两个独立控制面请求，串行会多
    // 一个 RTT；移动网络每 RTT 几百 ms）。
    const relay = normalizeRelay(cfg.relayUrl);
    // 下拉回填用户上次选的源（cfg.updateSource 优先，其次 localStorage）。
    if (updateSourceEl) updateSourceEl.value = normalizeSource(cfg.updateSource || getUpdateSource());
    const updateP = checkUpdate(relay).catch((e) => ({ update: null, error: errMsg(e) }));
    const probeP = cfg.deviceToken
      ? probeDeviceToken(relay, cfg.deviceToken)
      : cfg.accessToken ? probeToken(relay, cfg.accessToken) : Promise.resolve(null);
    Promise.all([updateP, probeP]).then(([check, probeErr]) => {
      bootMark("preflightDone");
      if (probeErr) {
        // token 有问题：优先报错，停在设置页。
        errorEl.hidden = false;
        errorTextEl.textContent = probeErr;
        return;
      }
      const update = check && check.update;
      if (!update) {
        // 源不可达时如实提示（不给用户「一切正常」的假象），但照常进 GUI；
        // 用户可在更新卡片里换源重试。无更新/正常则静默进 GUI。
        if (check && check.error) log(`preflight: update source error — ${check.error}`);
        log("preflight ok, auto connect");
        connect(cfg, true);
        return;
      }
      presentUpdate(update, cfg); // 自动更新：下载→校验→拉起安装器
    });
  } else {
    setupEl.hidden = false;
  }
}
// 前后台切换与刷新前上报：手机把 App 切后台/切回来、系统重载 WebView
// 都会走这里，日志随之落到 Relay 的 app-logs/app.log，事后可直接下载。
// 导航 away 时的日志上报：有凭据走 fetch keepalive + header（sendBeacon 带
// 不了自定义头），开放 relay 保留 sendBeacon。凭据从内存 lastCfg 或持久化
// 配置取，绝不进 URL。
function beaconLogs() {
  persistLogs();
  const cfg = lastCfg || loadConfig();
  const relay = normalizeRelay(cfg?.relayUrl || "");
  if (!relay) return;
  const token = cfg?.deviceToken || cfg?.accessToken || "";
  try {
    if (token) {
      void fetch(`${relay}/__dsh_tunnel__/app-log`, {
        method: "POST",
        headers: { "Content-Type": "text/plain", "X-DSH-Relay-Token": token },
        body: getLogs(),
        keepalive: true,
      }).catch(() => {});
    } else {
      navigator.sendBeacon(`${relay}/__dsh_tunnel__/app-log`, getLogs());
    }
  } catch {
    // 不支持时静默跳过
  }
}
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "hidden") {
    beaconLogs();
  } else {
    log("app visible");
  }
});
window.addEventListener("pagehide", () => {
  beaconLogs();
});

void boot();
