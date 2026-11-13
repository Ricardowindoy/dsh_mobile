package relay

import (
	"net/http"
	"strings"
)

// Multi-node shell (2026-11-10): when ≥2 devices are paired, the relay serves
// a bare stage at the root instead of proxying one node's GUI. The stage keeps
// one same-origin iframe per node (/d/<id>/) alive and switches visibility on
// default-node SSE frames — browser-tab semantics, no reload. Living on the
// relay origin (not injected into a node's GUI) is what makes the auth cookie
// first-party: the plugin-injected variant sent phones cross-site to the
// configured absolute relay URL and SameSite=Lax dropped the cookie (401).
//
// 节点切换 UI 全部由 remote-access 插件提供（会话头部标签行右端的下拉），
// 壳页本身不再渲染任何下拉/顶栏（2026-11-10 用户定：红圈内的壳页下拉不需要）。
// 壳页只保留 iframe 舞台 + __dshShell 桥（takeover/state/switchTo/心跳）。
//
// The page never guesses state: the node list comes from the authorized probe
// (devices[]), the active node from default-node, and every switch is a POST
// to default-node — the same single source of truth the plugin already uses.
const shellPage = `<!doctype html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover, interactive-widget=resizes-content">
<title>DSH 节点</title>
<style>
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  html, body { margin: 0; height: 100%; overflow: hidden; }
  body { background: #fff; }
  @media (prefers-color-scheme: dark) { body { background: #111; } }
  #stage { position: relative; width: 100%; height: 100%; }
  #stage iframe { position: absolute; inset: 0; width: 100%; height: 100%; border: 0; display: none; }
  #stage iframe.active { display: block; }
  #loading {
    position: absolute; inset: 0; display: none; place-items: center;
    background: color-mix(in srgb, Canvas 65%, transparent); color: CanvasText;
    font-size: 13px; z-index: 5; pointer-events: none;
  }
  #loading.show { display: grid; }
  /* 节点启动失败时的恢复卡（2026-11-13）：只在当前节点起不来时出现，
     不是常驻顶栏/下拉 —— 正常浏览仍由节点 GUI 里的插件下拉负责切换。 */
  #recover {
    position: absolute; inset: 0; display: none; place-items: center;
    padding: 20px; z-index: 9; background: color-mix(in srgb, Canvas 86%, transparent);
  }
  #recover.show { display: grid; }
  #recover .card {
    width: min(360px, 100%); background: Canvas; color: CanvasText;
    border: 1px solid color-mix(in srgb, CanvasText 14%, transparent);
    border-radius: 14px; padding: 16px;
    box-shadow: 0 12px 40px color-mix(in srgb, #000 22%, transparent);
    font: 13px/1.45 system-ui, -apple-system, "Segoe UI", sans-serif;
  }
  #recover-title { font-size: 15px; font-weight: 600; margin-bottom: 4px; }
  #recover-detail { color: color-mix(in srgb, CanvasText 62%, transparent); word-break: break-all; }
  #recover-list { display: grid; gap: 8px; margin-top: 12px; }
  #recover-list button {
    display: flex; align-items: center; gap: 8px; width: 100%; min-height: 42px;
    padding: 0 12px; border-radius: 10px; cursor: pointer; text-align: left;
    border: 1px solid color-mix(in srgb, CanvasText 14%, transparent);
    background: color-mix(in srgb, CanvasText 4%, Canvas); color: inherit;
    font: inherit;
  }
  #recover-list button:disabled { opacity: .55; cursor: default; }
  #recover-list .dot { width: 8px; height: 8px; border-radius: 50%; flex: none; background: color-mix(in srgb, CanvasText 30%, transparent); }
  #recover-list .dot.on { background: #1a7f37; }
  #recover-list .dot.off { background: #cf222e; }
  #recover-actions { display: flex; align-items: center; gap: 10px; margin-top: 12px; }
  #recover-retry {
    min-height: 36px; padding: 0 14px; border-radius: 9px; cursor: pointer;
    border: 1px solid color-mix(in srgb, CanvasText 18%, transparent);
    background: transparent; color: inherit; font: inherit;
  }
  #recover-error { color: #cf222e; flex: 1; min-width: 0; }
  #err { padding: 16px; color: #cf222e; white-space: pre-wrap; }
</style>
</head>
<body>
<div id="stage">
  <div id="loading">节点启动中…</div>
  <div id="recover" role="dialog" aria-modal="true" aria-labelledby="recover-title">
    <div class="card">
      <div id="recover-title">节点无法启动</div>
      <div id="recover-detail"></div>
      <div id="recover-list"></div>
      <div id="recover-actions">
        <button id="recover-retry" type="button">重试</button>
        <span id="recover-error" role="alert"></span>
      </div>
    </div>
  </div>
</div>
<script>
(() => {
  'use strict'
  const stage = document.getElementById('stage')
  const loading = document.getElementById('loading')
  const recover = document.getElementById('recover')
  const recoverDetail = document.getElementById('recover-detail')
  const recoverList = document.getElementById('recover-list')
  const recoverRetry = document.getElementById('recover-retry')
  const recoverError = document.getElementById('recover-error')
  const frames = new Map()   // deviceId -> {iframe, ready, failed}
  let devices = []           // [{id, online}]
  let current = null         // visible deviceId
  let defaultNode = null     // relay authority
  let recoverNode = null     // deviceId the recovery card is showing for
  let recoverReason = ''

  const enc = encodeURIComponent
  const nodeURL = (id) => '/d/' + enc(id) + '/'

  // relay 在设备不可达时回的是纯文本短句；iframe 同源，读正文即可判定失败，
  // 比干等 20s 超时更早给出恢复入口。只认白名单，不把正常 GUI 文本当错误。
  const RELAY_ERRORS = /^(device stream rejected|device pairing in progress|device connection lost|no device paired|unauthorized)$/i
  function relayError(rec) {
    try {
      const d = rec.iframe.contentDocument
      if (!d || !d.body) return ''
      const text = String(d.body.innerText || d.body.textContent || '').trim()
      return text && text.length <= 120 && RELAY_ERRORS.test(text) ? text : ''
    } catch { return '' } // 同源构造，理论上不会跨域
  }

  function hideRecover() {
    recoverNode = null
    recoverReason = ''
    recoverError.textContent = ''
    recover.classList.remove('show')
  }

  // 恢复卡只在「当前节点起不来」时出现：正常浏览没有任何壳页顶栏/下拉
  // （2026-11-10 用户定切换 UI 归节点 GUI 里的插件）。插件不可能在打不开
  // 的节点 GUI 里渲染，所以失败时必须由壳页给出逃生口，否则会死锁。
  function showRecover(id, reason) {
    if (current !== id) return
    // 失败页的原始短句已收进 recoverDetail，舞台上的错误 iframe 不再裸露。
    for (const [, r] of frames) r.iframe.classList.remove('active')
    recoverNode = id
    recoverReason = reason
    recoverDetail.textContent = id + ' · ' + reason
    recoverList.replaceChildren()
    const candidates = devices
      .filter((d) => d.id !== id)
      .sort((a, b) => (b.online === true ? 1 : 0) - (a.online === true ? 1 : 0))
    if (candidates.length === 0) {
      const p = document.createElement('div')
      p.textContent = '没有其他已配对节点可切换。'
      p.style.opacity = '.65'
      recoverList.appendChild(p)
    }
    for (const d of candidates) {
      const b = document.createElement('button')
      b.type = 'button'
      const dot = document.createElement('span')
      dot.className = 'dot ' + (d.online === true ? 'on' : d.online === false ? 'off' : '')
      const txt = document.createElement('span')
      txt.textContent = d.id + (d.online === false ? ' · 离线' : '')
      b.appendChild(dot)
      b.appendChild(txt)
      b.addEventListener('click', () => { void switchNode(d.id) })
      recoverList.appendChild(b)
    }
    loading.classList.remove('show')
    recover.classList.add('show')
  }

  async function switchNode(id) {
    const buttons = [...recoverList.querySelectorAll('button')]
    for (const b of buttons) b.disabled = true
    recoverError.textContent = '切换中…'
    try {
      const r = await fetch('/__dsh_tunnel__/default-node', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ deviceId: id }),
      })
      if (!r.ok) throw new Error('HTTP ' + r.status)
      hideRecover()
      show(id)
      await loadDevices().catch(() => {})
      void loadDefault()
    } catch (e) {
      recoverError.textContent = '切换失败：' + e.message
      for (const b of buttons) b.disabled = false
    }
  }

  recoverRetry.addEventListener('click', () => {
    const id = recoverNode || current
    if (!id) return
    const rec = frames.get(id)
    if (rec) { rec.iframe.remove(); frames.delete(id) }
    if (current === id) current = null
    hideRecover()
    show(id)
  })

  // ready 只在 load 事件里置位；303 token-exchange 的中间页也会触发 load，
  // 因此事件后再轮询几次直到真 GUI 出现（dsh 的 #root/frame，或任何有标题
  // 且有子元素的文档）。轮询有界，绝不猜测。
  function watchReady(id, rec) {
    let tries = 0
    const check = () => {
      if (rec.ready || rec.failed) return
      tries++
      const errText = relayError(rec)
      if (errText) {
        rec.failed = errText
        if (current === id) { loading.classList.remove('show'); showRecover(id, errText) }
        return
      }
      try {
        const d = rec.iframe.contentDocument
        if (d && d.body && (d.querySelector('#root, [class*="frame"]') || (d.title && d.body.children.length > 0))) {
          rec.ready = true
          rec.failed = ''
          if (current === id) {
            loading.classList.remove('show')
            if (recoverNode === id) { hideRecover(); show(id) }
          }
          return
        }
      } catch { /* same-origin by construction */ }
      if (tries < 20) setTimeout(check, 500)
      else {
        rec.failed = '启动超时'
        if (current === id) { loading.classList.remove('show'); showRecover(id, '启动超时') }
      }
    }
    setTimeout(check, 0)
  }

  function ensureFrame(id) {
    let rec = frames.get(id)
    if (rec) return rec
    const f = document.createElement('iframe')
    f.src = nodeURL(id)
    f.dataset.node = id
    rec = { iframe: f, ready: false, failed: '' }
    // load 事件也可能永远不来（请求挂起而不是快速报错）：独立看门狗兜底。
    const watchdog = setTimeout(() => {
      if (rec.ready || rec.failed) return
      rec.failed = '启动超时'
      if (current === id) { loading.classList.remove('show'); showRecover(id, '启动超时') }
    }, 20000)
    f.addEventListener('load', () => { clearTimeout(watchdog); watchReady(id, rec) })
    frames.set(id, rec)
    stage.appendChild(f)
    return rec
  }

  function show(id) {
    if (!id) return
    current = id
    if (recoverNode && recoverNode !== id) hideRecover()
    const rec = ensureFrame(id)
    for (const [dev, r] of frames) r.iframe.classList.toggle('active', dev === id)
    if (rec.failed) { loading.classList.remove('show'); showRecover(id, rec.failed) }
    else loading.classList.toggle('show', !rec.ready)
  }

  // 壳页**不写入** default-node：唯一写入在插件/host（见下）。壳页只做
  // 「跟随权威状态切换 iframe」：applyDefault 由 SSE 首帧/后续帧与 5s 轮询驱动。
  // 旧版壳页的 switchTo 会自己 POST default-node，与插件下拉形成双写，已删除
  // （2026-11-10 用户定「切换逻辑只留一套」）。唯一例外是恢复卡：当前节点
  // 打不开、插件无处渲染时，由壳页写同一个 default-node 端点把用户救回来。

  function applyDefault(id) {
    if (!id) return
    const changed = id !== defaultNode
    defaultNode = id
    if (changed || !current) { if (devices.some(d => d.id === id)) show(id) }
  }

  async function loadDevices() {
    const r = await fetch('/__dsh_tunnel__/probe', { cache: 'no-store' })
    if (!r.ok) throw new Error('probe HTTP ' + r.status)
    const b = await r.json()
    devices = Array.isArray(b.devices) ? b.devices : []
  }

  async function loadDefault() {
    const r = await fetch('/__dsh_tunnel__/default-node', { cache: 'no-store' })
    if (!r.ok) return
    const b = await r.json()
    applyDefault(typeof b.deviceId === 'string' ? b.deviceId : '')
  }

  // ── 插件桥（2026-11-10 用户定：下拉按钮由插件实现、放回节点 GUI 会话头部
  // 标签行右端；壳页自有的下拉/顶栏已删除）。壳页与节点 iframe 同源，插件
  // （跑在 iframe 里）经此对象：
  //   · state()      → {current, devices} 给插件菜单渲染在线态/选中态
  //   · subscribe(fn)→ current 变化通知（SSE/轮询驱动）
  //   · switchTo(id) → **兼容空实现**：旧插件会调，但唯一写入已移到插件直发
  //     relay default-node；壳页只跟随 SSE，不再自己 POST。
  // takeover/release/beat 同为兼容空实现（壳页已无顶栏）。
  const subs = new Set()
  const notify = () => { for (const fn of subs) { try { fn(current) } catch {} } }
  const origShow = show
  show = function (id) { origShow(id); notify() }
  window.__dshShell = {
    takeover() {}, release() {}, beat() {}, switchTo() {},
    state() { return { current, devices: devices.map(d => ({ id: d.id, online: d.online })), error: recoverNode ? { deviceId: recoverNode, message: recoverReason } : null } },
    subscribe(fn) { subs.add(fn); return () => subs.delete(fn) },
  }
  // 已加载的节点 iframe 里插件可能先于桥就绪：广播一次 ready 事件。
  window.dispatchEvent(new Event('dsh-shell-ready'))

  async function boot() {
    await loadDevices()
    await loadDefault()
    // 首帧后仍无 current（default-node 为空/未配对）时只显示第一个在线节点，
    // **不写入** default-node —— 写入权唯一归插件/host。
    if (!current && devices.length) {
      const on = devices.find(d => d.online) || devices[0]
      show(on.id)
    }
    // SSE 是唯一跟随通道；轮询仅作断流兜底（只读 GET，不写入）。
    try {
      const es = new EventSource('/__dsh_tunnel__/default-node/watch')
      es.onmessage = (ev) => applyDefault(String(ev.data || '').trim())
    } catch { /* poll below covers it */ }
    setInterval(() => { if (document.visibilityState === 'visible') void loadDefault() }, 5000)
    setInterval(() => { if (document.visibilityState === 'visible') void loadDevices().catch(() => {}) }, 30000)
  }
  boot().catch((e) => {
    const div = document.createElement('div')
    div.id = 'err'
    div.textContent = '节点列表加载失败：' + e.message
    stage.appendChild(div)
  })
})()
</script>
</body>
</html>
`

// handleShell serves the multi-node shell page. Authorization matches the
// phone data plane: the page itself carries no secrets, but gating it keeps
// unauthenticated callers from learning device ids.
func (r *Relay) handleShell(w http.ResponseWriter, req *http.Request) {
	if r.cfg.AccessToken != "" && !r.authorize(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if req.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(shellPage))
}

// pairedCount reports how many devices currently hold a live mux.
func (r *Relay) pairedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, d := range r.devices {
		if d.mux.Load() != nil {
			n++
		}
	}
	return n
}

// scopeNodeCookies rewrites Set-Cookie headers on /d/<id>/ responses so each
// node's dsh-auth cookie is scoped to its own path prefix. Without this every
// node's GUI issues the SAME cookie name (hash of the relay Host) with Path=/
// and different secrets: two live iframes would overwrite each other's cookie
// and ping-pong 303 token exchanges forever. Path-scoping keeps them isolated;
// the browser sends the matching cookie on prefix requests and dsh reads its
// own exact name. Relay-issued cookies (login/pairing) are untouched.
//
// Deliberate string surgery instead of http.ParseSetCookie (go1.23+; module
// is go1.22): only the Path attribute of dsh-auth-* cookies is replaced, the
// rest of the header passes through byte for byte.
func scopeNodeCookies(headers http.Header, prefix string) {
	cookies := headers.Values("Set-Cookie")
	if len(cookies) == 0 {
		return
	}
	scoped := make([]string, 0, len(cookies))
	for _, raw := range cookies {
		name, _, _ := strings.Cut(raw, "=")
		if !strings.HasPrefix(strings.TrimSpace(name), "dsh-auth-") {
			scoped = append(scoped, raw)
			continue
		}
		parts := strings.Split(raw, ";")
		hasPath := false
		for i, part := range parts {
			attr := strings.TrimSpace(part)
			if strings.EqualFold(attr, "Path") || strings.HasPrefix(strings.ToLower(attr), "path=") {
				parts[i] = " Path=" + prefix
				hasPath = true
			}
		}
		out := strings.Join(parts, ";")
		if !hasPath {
			out += "; Path=" + prefix
		}
		scoped = append(scoped, out)
	}
	headers.Del("Set-Cookie")
	for _, v := range scoped {
		headers.Add("Set-Cookie", v)
	}
}
