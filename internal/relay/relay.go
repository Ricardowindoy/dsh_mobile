// Package relay implements the public relay service: it listens on a public
// port, pairs devices over wss (pairing token -> deviceId), gates phone
// requests behind an optional static access token, and routes each phone
// HTTP request to the paired device through a multiplexed stream.
//
// The relay is a byte pipe: it never parses dsh business traffic. TLS can be
// terminated here or by a fronting reverse proxy.
package relay

import (
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"dsh_mobile/internal/diag"
	"dsh_mobile/internal/mux"
	"dsh_mobile/internal/protocol"
	"dsh_mobile/internal/update"
)

// DevicePath is the wss endpoint devices dial.
const DevicePath = "/__dsh_tunnel__/device"

// Config configures the relay.
type Config struct {
	// ListenAddr is the public listen address, e.g. ":443".
	ListenAddr string
	// ExtraListenAddrs are additional listen addresses served with the exact
	// same handler. A fronting reverse proxy must speak plain HTTP to its
	// upstream, so the deployment binds the public TLS listener on the real
	// port (e.g. ":24000") and adds a loopback-only plain-HTTP listener
	// (e.g. "127.0.0.1:24001") for nginx to proxy to — otherwise nginx would
	// hand the TLS listener a cleartext request and every request would fail
	// with "Client sent an HTTP request to an HTTPS server" (measured).
	ExtraListenAddrs []string
	// TLSCertFile / TLSKeyFile enable TLS termination (optional; a fronting
	// reverse proxy may terminate TLS instead).
	TLSCertFile string
	TLSKeyFile  string

	// Pairings maps pairing tokens to device ids.
	Pairings map[string]string
	// DefaultNodeFile, when set, points at a file holding one device id: the
	// node root-path requests route to when several devices are paired. The
	// file is re-read on every unmatched-path request (cheap, no reload); an
	// empty/missing/unknown id falls back to the single-device heuristic.
	// Managed by the remote-access plugin's node switch.
	DefaultNodeFile string
	// AccessToken, when non-empty, gates phone requests: the client must
	// send X-DSH-Relay-Token or "Authorization: Bearer <token>". With
	// PhoneDevicesFile set it additionally acts as the MANAGEMENT credential
	// (pair mint/revoke/list); phone data-plane access then also accepts
	// per-device tokens from the roster.
	AccessToken string
	// PhoneDevicesFile, when non-empty, enables the per-device credential
	// surface: one-time pairing codes (login?pair=), a hash-only device
	// roster, and per-device revocation. Empty = legacy static-token
	// behaviour, byte for byte.
	PhoneDevicesFile string
	// NotifyEnabled turns on the approval/question notification plane
	// (SSE watch, push, answers). Requires PhoneDevicesFile (the phone
	// credential IS the subscription identity) and AccessToken (the
	// plugin's management credential). Default off.
	NotifyEnabled bool
	// NotifySeqFile persists the notification plane's monotonic sequence.
	// Empty = derive "<dir of PhoneDevicesFile>/notify-seq". The sequence must
	// survive restarts: an in-memory counter reset makes every reconnect
	// cursor invalid (and previously caused whole-ring replays).
	NotifySeqFile string

	// HeartbeatInterval, defaults to protocol.DefaultHeartbeatMs.
	HeartbeatInterval time.Duration
	// MaxStreamsPerDevice caps concurrent streams per device.
	MaxStreamsPerDevice int
	// PerStreamBufferBytes caps per-stream buffering before backpressure.
	PerStreamBufferBytes int
	// StreamRecvLimitBytes is the hard per-stream receive-queue cap (0 keeps
	// PerStreamBufferBytes*8). Public relays serving multi-MB bundles to slow
	// phones must raise this, otherwise a large response is reset mid-stream
	// and the browser reports a truncated asset.
	StreamRecvLimitBytes int
	// TotalRecvLimitBytes bounds the sum of all receive queues (0 = unlimited).
	TotalRecvLimitBytes int
	// IdleTimeout closes idle streams (0 disables).
	IdleTimeout time.Duration
	// BandwidthBytesPerSec caps device throughput per direction (0 = unlimited).
	BandwidthBytesPerSec int64
	// MaxConnDuration closes device connections after this long (0 = forever).
	// Devices reconnect automatically.
	MaxConnDuration time.Duration
	// RequestBodyLimit caps forwarded request bodies.
	RequestBodyLimit int64
	// UpdatesDir is the directory serving app update manifests and
	// packages (default "updates"). See internal/update.
	UpdatesDir string

	// Diagnostic, when non-nil, receives one line per device/connection
	// lifecycle event (paired, lost with reason, superseded by a newer
	// connection). Use diag.NewFileWriter for a file-backed sink; the
	// caller owns closing it.
	Diagnostic io.Writer

	// Logger, defaults to slog.Default().
	Logger *slog.Logger
}

// Relay is the running service.
type Relay struct {
	cfg    Config
	log    *slog.Logger
	diag   *diag.Logger
	server *http.Server
	// extraServers are the additional plain-HTTP listeners (reverse-proxy
	// upstreams), tracked so shutdown can close them too.
	extraServers []*http.Server

	mu      sync.Mutex
	devices map[string]*device

	// phones is the per-device credential roster (nil = surface disabled).
	phones *phoneRoster
	// pairing holds the single active one-time pairing code (memory only).
	pairing *pairingState

	// notify is the approval/question push hub (nil = plane disabled).
	notify *notifyHub

	// appLogHits rate-limits log pushes per IP (10/minute).
	appLogMu   sync.Mutex
	appLogHits map[string][]time.Time

	// nodeWatchers fan out default-node changes to SSE subscribers
	// (/__dsh_tunnel__/default-node/watch). Buffered channels; a slow
	// subscriber misses events rather than blocking the setter.
	nodeWatchMu  sync.Mutex
	nodeWatchers map[chan string]struct{}
}

type device struct {
	id     string
	mux    atomic.Pointer[mux.Mux]
	conn   *websocket.Conn
	since  time.Time
	cancel context.CancelFunc
}

func defaultConfig(cfg *Config) Config {
	c := *cfg
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = protocol.DefaultHeartbeatMs * time.Millisecond
	}
	if c.MaxStreamsPerDevice <= 0 {
		c.MaxStreamsPerDevice = 64
	}
	if c.PerStreamBufferBytes <= 0 {
		c.PerStreamBufferBytes = 256 * 1024
	}
	if c.RequestBodyLimit <= 0 {
		c.RequestBodyLimit = 32 << 20
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// New builds the relay. Call ListenAndServe to start it.
func New(cfg Config) *Relay {
	c := defaultConfig(&cfg)
	r := &Relay{
		cfg:     c,
		log:     c.Logger,
		devices: make(map[string]*device),
	}
	r.diag = diag.NewWithOptions(c.Diagnostic, c.Logger, diag.Options{Header: "relay"})
	if c.PhoneDevicesFile != "" {
		pr, err := loadRoster(c.PhoneDevicesFile)
		if err != nil {
			// A roster that cannot even be read is a configuration error, not
			// an open gate: fail construction rather than silently dropping
			// every paired device.
			panic(fmt.Sprintf("relay: phone roster: %v", err))
		}
		r.phones = pr
		r.pairing = newPairingState()
		if c.NotifyEnabled && c.AccessToken != "" {
			r.notify = newNotifyHub(r.notifySeqFile())
		}
	}
	r.server = &http.Server{
		Addr:              c.ListenAddr,
		Handler:           r.routes(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
	}
	return r
}

// Handler returns the relay's HTTP handler. It lets another program mount the
// relay on its own listeners (e.g. a unified forwarder with several TLS
// entrances) without changing how ListenAndServe behaves. The returned handler
// is safe for concurrent use and shares the relay's device registry.
func (r *Relay) Handler() http.Handler { return r.routes() }

// CloseDevices tears down all paired device connections. A host embedding the
// relay calls this during graceful shutdown; ListenAndServe already does it
// on its own exit paths.
func (r *Relay) CloseDevices() { r.closeDevices() }

// ListenAndServe runs the relay until the context is cancelled. The primary
// address is served (with TLS when configured); ExtraListenAddrs are served
// plain HTTP with the same handler.
func (r *Relay) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", r.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("relay: listen: %w", err)
	}
	r.log.Info("relay: listening", "addr", ln.Addr().String(), "tls", r.cfg.TLSCertFile != "")

	errCh := make(chan error, 1+len(r.cfg.ExtraListenAddrs))
	go func() {
		var serveErr error
		if r.cfg.TLSCertFile != "" {
			serveErr = r.server.ServeTLS(ln, r.cfg.TLSCertFile, r.cfg.TLSKeyFile)
		} else {
			serveErr = r.server.Serve(ln)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- fmt.Errorf("relay: serve %s: %w", r.cfg.ListenAddr, serveErr)
			return
		}
		errCh <- nil
	}()

	for _, addr := range r.cfg.ExtraListenAddrs {
		extra, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("relay: listen %s: %w", addr, err)
		}
		// Plain HTTP, never TLS: this listener exists for a fronting reverse
		// proxy that terminates TLS itself (X-Forwarded-Proto carries the
		// original scheme through to the update handler).
		r.log.Info("relay: listening", "addr", extra.Addr().String(), "tls", false)
		server := &http.Server{
			Handler:           r.server.Handler,
			ReadHeaderTimeout: 30 * time.Second,
			IdleTimeout:       5 * time.Minute,
		}
		r.extraServers = append(r.extraServers, server)
		go func(srv *http.Server, l net.Listener) {
			if serveErr := srv.Serve(l); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				errCh <- fmt.Errorf("relay: serve %s: %w", l.Addr(), serveErr)
				return
			}
			errCh <- nil
		}(server, extra)
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.server.Shutdown(shutdownCtx)
		for _, srv := range r.extraServers {
			_ = srv.Shutdown(shutdownCtx)
		}
		r.closeDevices()
		return ctx.Err()
	case err := <-errCh:
		r.closeDevices()
		return err
	}
}

// routes builds the HTTP routes.
func (r *Relay) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(DevicePath, r.handleDevice)
	mux.HandleFunc("/__dsh_tunnel__/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/__dsh_tunnel__/login", r.handleLogin)
	// auth-check 是 App 预检 token 的端点：200=token 有效，401=无效。
	// 返回普通 JSON（无重定向），配合 CORS 头可被 WebView 正常读取
	// （login 的 302 在跨域 redirect:manual 下会变 opaque，读不到状态码）。
	mux.HandleFunc("/__dsh_tunnel__/auth-check", r.handleAuthCheck)
	// whoami 告诉 App 这个 Relay 是否开了访问鉴权，App 据此决定是否
	// 要求用户填写 token（未鉴权时直接进 GUI）。
	mux.HandleFunc("/__dsh_tunnel__/whoami", r.handleWhoami)
	mux.HandleFunc("/__dsh_tunnel__/probe", r.handleProbe)
	mux.HandleFunc("/__dsh_tunnel__/default-node", r.handleDefaultNode)
	mux.HandleFunc("/__dsh_tunnel__/default-node/watch", r.handleDefaultNodeWatch)
	// app-log 接收 App 推送的客户端日志（调试用），追加写入文件。
	mux.HandleFunc("/__dsh_tunnel__/app-log", r.handleAppLog)
	// Per-device pairing surface (only answers when PhoneDevicesFile is set).
	mux.HandleFunc("/__dsh_tunnel__/pair/mint", r.handlePairMint)
	mux.HandleFunc("/__dsh_tunnel__/pair/status", r.handlePairStatus)
	mux.HandleFunc("/__dsh_tunnel__/pair/devices", r.handlePairDevices)
	mux.HandleFunc("/__dsh_tunnel__/pair/revoke", r.handlePairRevoke)
	mux.HandleFunc("/__dsh_tunnel__/pair/self", r.handlePairSelf)
	mux.HandleFunc("/__dsh_tunnel__/pair/redeem", r.handlePairRedeem)
	mux.HandleFunc("/__dsh_tunnel__/pair/refresh", r.handlePairRefresh)
	// Notification plane (only answers when NotifyEnabled + roster + admin).
	mux.HandleFunc("/__dsh_tunnel__/notify/push", r.handleNotifyPush)
	mux.HandleFunc("/__dsh_tunnel__/notify/watch", r.handleNotifyWatch)
	mux.HandleFunc("/__dsh_tunnel__/notify/answer", r.handleNotifyAnswer)
	mux.HandleFunc("/__dsh_tunnel__/notify/answers", r.handleNotifyAnswers)
	mux.HandleFunc("/__dsh_tunnel__/notify/online", r.handleNotifyOnline)
	mux.HandleFunc("/__dsh_tunnel__/notify/ack", r.handleNotifyAck)
	updates := update.NewHandler(r.cfg.UpdatesDir, r.log)
	mux.HandleFunc("/__dsh_tunnel__/update/check", updates.Check)
	mux.HandleFunc("/__dsh_tunnel__/update/download", updates.Download)
	// Control-plane seal: an unregistered /__dsh_tunnel__/* path is a 404 on
	// the relay itself, never forwarded to a device (which would waste a mux
	// stream and answer with the GUI's 404 page instead of a control error).

	mux.HandleFunc("/__dsh_tunnel__/", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not-found", "path": req.URL.Path})
	})
	mux.HandleFunc("/", r.handlePhone)
	// 包装顺序：rewrite 在外、CORS 在内——rewrite 先把 /d/<id>/__dsh_tunnel__/* 剥成
	// /__dsh_tunnel__/*，CORS 检查才能命中并加上跨域头；顺序反了节点前缀请求拿不到
	// CORS 头，App WebView 的 fetch 会被浏览器拦截（读不到 200 响应）报无法连接。
	// /__dsh_tunnel__/* 端点要被 App WebView（tauri.localhost origin）
	// 跨域 fetch，必须带 CORS 头；/ 主路径走导航不受 CORS 限制，不动。
	return withNodePrefixRewrite(withTunnelCORS(mux))
}

// withNodePrefixRewrite lets the mobile App point its Relay address at one
// node explicitly: /d/<deviceId>/__dsh_tunnel__/<rest> is the App control
// plane (whoami/auth-check/login/probe/app-log/update) addressed to a node,
// NOT a GUI path for handlePhone. The control endpoints live on the Relay
// itself, so strip the /d/<deviceId> prefix in place. login's redirect then
// gets the node prefix re-applied (via nodePrefixWriter) so the GUI lands on
// the right node. Without this, a node-prefixed control request falls through
// to handlePhone and dies on the device (no such endpoint there).
func withNodePrefixRewrite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		if strings.HasPrefix(path, "/d/") {
			trimmed := strings.TrimPrefix(path, "/d/")
			id, rest, _ := strings.Cut(trimmed, "/")
			if id != "" && strings.HasPrefix(rest, "__dsh_tunnel__/") {
				req.URL.Path = "/" + rest
				next.ServeHTTP(&nodePrefixWriter{ResponseWriter: w, nodePrefix: "/d/" + id}, req)
				return
			}
		}
		next.ServeHTTP(w, req)
	})
}

// nodePrefixWriter re-applies the /d/<id> prefix to any root-relative
// Location header the wrapped handler emits (login's 302), so the App's
// WebView stays node-addressed after the token exchange redirect.
type nodePrefixWriter struct {
	http.ResponseWriter
	nodePrefix string
	wrote      bool
}

func (w *nodePrefixWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		if w.nodePrefix != "" {
			loc := w.Header().Get("Location")
			if loc != "" && strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") {
				w.Header().Set("Location", w.nodePrefix+loc)
			}
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

// withTunnelCORS adds permissive CORS headers to the tunnel control-plane
// endpoints only (whoami/update/login 无敏感数据)。OPTIONS 预检直接 204。
func withTunnelCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/__dsh_tunnel__/") {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "*")
			if req.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, req)
	})
}

// notifySeqFile resolves where the notification sequence is persisted.
func (r *Relay) notifySeqFile() string {
	if r.cfg.NotifySeqFile != "" {
		return r.cfg.NotifySeqFile
	}
	if r.cfg.PhoneDevicesFile != "" {
		return filepath.Join(filepath.Dir(r.cfg.PhoneDevicesFile), "notify-seq")
	}
	return ""
}

// handleAppLog appends client-side logs pushed from the mobile app.
// POST /__dsh_tunnel__/app-log {body: lines...}
// Gated by the same credential as the data plane (a log pusher is a phone),
// rate-limited per credential, and bounded well below the old 4MB anonymous
// write surface.
func (r *Relay) handleAppLog(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if (r.cfg.AccessToken != "" || r.phones != nil) && !r.authorize(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !r.appLogAllowed(clientIP(req)) {
		http.Error(w, "too many log pushes", http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 256<<10))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	// 日志文件放 updates 目录外，避免与更新包混淆。
	logDir := filepath.Join(filepath.Dir(r.cfg.UpdatesDir), "app-logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		http.Error(w, "mkdir failed", http.StatusInternalServerError)
		return
	}
	f, err := os.OpenFile(filepath.Join(logDir, "app.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		http.Error(w, "open log failed", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	ts := time.Now().Format("2006-01-02 15:04:05")
	// 头部带上来源与平台：多台手机/多个入口的日志混在一个文件里时，
	// 没有这些信息就无法判断"哪次是哪台设备、从哪个网络来的"。
	if _, err := fmt.Fprintf(f, "\n===== %s remote=%s path=%s ua=%q =====\n%s",
		ts, req.RemoteAddr, req.URL.Path, req.UserAgent(), body); err != nil {
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleAuthCheck validates a credential without redirecting.
// GET /__dsh_tunnel__/auth-check (header/bearer/cookie, or legacy ?token=)
// -> 200 {"valid":true} / 401 {"valid":false}
func (r *Relay) handleAuthCheck(w http.ResponseWriter, req *http.Request) {
	if r.cfg.AccessToken == "" && r.phones == nil {
		// 未开鉴权：任何 token（包括空）都算有效。
		writeJSON(w, http.StatusOK, map[string]any{"valid": true})
		return
	}
	if q := req.URL.Query().Get("token"); q != "" {
		r.log.Warn("relay: deprecation=query-token auth-check; use header", "remote", req.RemoteAddr)
		if r.cfg.AccessToken != "" && subtle.ConstantTimeCompare([]byte(q), []byte(r.cfg.AccessToken)) == 1 {
			writeJSON(w, http.StatusOK, map[string]any{"valid": true})
			return
		}
		// A presented-but-wrong query token is an explicit failure: do not
		// fall through to authorize(), which would pass on an open gate.
		writeJSON(w, http.StatusUnauthorized, map[string]any{"valid": false})
		return
	}
	if r.authorize(req) {
		writeJSON(w, http.StatusOK, map[string]any{"valid": true})
		return
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{"valid": false})
}

// handleWhoami reports whether the relay enforces an access token.
// GET /__dsh_tunnel__/whoami -> {"authRequired":true|false}
func (r *Relay) handleWhoami(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"authRequired":%v}`, r.cfg.AccessToken != "")
}

// handleProbe answers "can this client reach the relay, and is the device
// online?" without touching the device. The mobile app uses it as a
// connection self-test: a phone on a foreign network needs one request that
// separates "my network/URL is wrong" from "the tunnel is down", and
// /api/*-style probes cannot do that because they hang or 401 without a GUI
// cookie.
//
// The aggregate answer is public. The per-device list (id + online) is only
// returned to a caller that presents the access token, because device ids are
// routing keys and the probe is deliberately unauthenticated.
//
// GET /__dsh_tunnel__/probe[?token=...]
//
//	-> {"ok":true,"authRequired":bool,"deviceOnline":bool,"deviceCount":n}
//	-> + "devices":[{"id":"home-dsh","online":true}] when authorized
func (r *Relay) handleProbe(w http.ResponseWriter, req *http.Request) {
	type deviceView struct {
		ID     string `json:"id"`
		Online bool   `json:"online"`
	}
	r.mu.Lock()
	devices := make([]deviceView, 0, len(r.devices))
	count := 0
	for id, d := range r.devices {
		online := d.mux.Load() != nil
		if online {
			count++
		}
		devices = append(devices, deviceView{ID: id, Online: online})
	}
	r.mu.Unlock()
	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })

	payload := map[string]any{
		"ok":           true,
		"authRequired": r.cfg.AccessToken != "",
		"deviceOnline": count > 0,
		"deviceCount":  count,
	}
	if r.authorize(req) {
		payload["devices"] = devices
	}
	w.Header().Set("Content-Type", "application/json")
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(body)
}

// handleLogin exchanges a credential for a cookie and redirects to the dsh
// GUI. Mobile WebViews cannot send custom headers on navigation, so the app
// first opens this endpoint, which plants the cookie, then redirects to the
// plain GUI URL.
//
// Two carriers:
//   - ticket=<one-time voucher>: the App scanned a QR, redeemed the code via
//     POST pair/redeem (code in the body, never a URL), and got this voucher.
//     Burns it, plants the device cookie. The long-lived device token never
//     enters a URL.
//   - token=<AccessToken>: legacy static-token path, kept for installed apps.
//     Deprecated: the token rides the query string into access logs.
func (r *Relay) handleLogin(w http.ResponseWriter, req *http.Request) {
	next := loginRedirectTarget(req.URL.Query().Get("next"))
	if ticket := req.URL.Query().Get("ticket"); ticket != "" || req.URL.Query().Has("ticket") {
		if !r.pairingEnabled(w) {
			return
		}
		token, ok := r.pairing.consumeTicket(ticket)
		if !ok {
			if !r.pairing.allowFailure(clientIP(req)) {
				http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
				return
			}
			http.Error(w, "ticket expired or already used", http.StatusNotFound)
			return
		}
		r.setDeviceCookie(w, req, token)
		http.Redirect(w, req, next, http.StatusFound)
		return
	}
	if r.cfg.AccessToken == "" {
		http.Redirect(w, req, next, http.StatusFound)
		return
	}
	token := req.URL.Query().Get("token")
	if subtle.ConstantTimeCompare([]byte(token), []byte(r.cfg.AccessToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if req.URL.Query().Has("token") {
		r.log.Warn("relay: deprecation=query-token login; use QR pairing", "remote", req.RemoteAddr)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     accessCookie,
		Value:    r.cfg.AccessToken,
		Path:     "/",
		MaxAge:   30 * 24 * 3600,
		HttpOnly: true,
		Secure:   r.cfg.TLSCertFile != "",
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, req, next, http.StatusFound)
}

const accessCookie = "dsh_relay_token"

// authorize checks the phone data-plane gate. Order: per-device roster
// token (when enabled), then the legacy static AccessToken (header, bearer,
// or the legacy cookie). Gate fully off (no AccessToken, no roster) = open.
func (r *Relay) authorize(req *http.Request) bool {
	if r.authorizeDevice(req) {
		return true
	}
	if r.cfg.AccessToken == "" {
		return true
	}
	token := req.Header.Get("X-DSH-Relay-Token")
	if token == "" {
		token = strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	}
	if token == "" {
		if c, err := req.Cookie(accessCookie); err == nil {
			token = c.Value
		}
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(r.cfg.AccessToken)) == 1
}

// handleDevice accepts a device wss connection and pairs it.
func (r *Relay) handleDevice(w http.ResponseWriter, req *http.Request) {
	if !websocket.IsWebSocketUpgrade(req) {
		http.Error(w, "websocket required", http.StatusBadRequest)
		return
	}
	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true }, // pairing token is the auth
	}
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		r.log.Warn("relay: device upgrade failed", "err", err)
		return
	}

	// Pairing handshake: hello -> ok/err.
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	msgType, data, err := conn.ReadMessage()
	if err != nil {
		r.log.Warn("relay: read hello failed", "err", err, "remote", req.RemoteAddr)
		r.diag.Records(diag.PrefixDeviceLost, "device handshake failed",
			"remote", req.RemoteAddr,
			"reason", "read hello: "+err.Error(),
			"uptime_s", "0.0")
		_ = conn.Close()
		return
	}
	if msgType != websocket.TextMessage {
		r.log.Warn("relay: hello was not a text frame", "type", msgType, "remote", req.RemoteAddr)
		_ = conn.Close()
		return
	}
	hello, err := protocol.Unmarshal(data)
	if err != nil || hello.Type != protocol.TypeHello {
		r.log.Warn("relay: bad hello frame", "err", err, "remote", req.RemoteAddr)
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	deviceID, ok := r.cfg.Pairings[hello.Token]
	if !ok || deviceID != hello.DeviceID {
		r.log.Warn("relay: pairing rejected", "deviceId", hello.DeviceID, "remote", req.RemoteAddr)
		r.diag.Records(diag.PrefixDeviceLost, "pairing rejected",
			"device", hello.DeviceID,
			"remote", req.RemoteAddr,
			"reason", "invalid pairing token")
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		reply, _ := protocol.Marshal(protocol.Err("invalid pairing token"))
		_ = conn.WriteMessage(websocket.TextMessage, reply)
		_ = conn.Close()
		return
	}

	r.log.Info("relay: device paired", "deviceId", deviceID, "remote", req.RemoteAddr)

	devCtx, cancel := context.WithCancel(context.Background())
	d := &device{id: deviceID, conn: conn, since: time.Now(), cancel: cancel}

	// Kick any previous connection for the same device. This is normal after
	// a device-side restart, but it is also what a flapping tunnel looks like
	// from here, so it gets its own lifecycle record.
	r.mu.Lock()
	if old, exists := r.devices[deviceID]; exists {
		live := time.Since(old.since)
		r.log.Warn("relay: superseding previous device connection",
			"deviceId", deviceID, "previousUptime", live.Truncate(time.Second).String())
		r.diag.Records(diag.PrefixDeviceKick, "superseded by a newer device connection",
			"device", deviceID,
			"previous_uptime_s", fmt.Sprintf("%.1f", live.Seconds()),
			"remote", req.RemoteAddr)
		old.cancel()
		if oldMux := old.mux.Load(); oldMux != nil {
			oldMux.SetReason("superseded by a newer device connection")
			_ = oldMux.Close()
		}
	}
	r.devices[deviceID] = d
	r.mu.Unlock()

	reply, _ := protocol.Marshal(protocol.OK(int(r.cfg.HeartbeatInterval / time.Millisecond)))
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, reply); err != nil {
		_ = conn.Close()
	}
	_ = conn.SetWriteDeadline(time.Time{})

	m := mux.New(conn, mux.Config{
		HeartbeatInterval:    r.cfg.HeartbeatInterval,
		MaxStreams:           r.cfg.MaxStreamsPerDevice,
		PerStreamBufferBytes: r.cfg.PerStreamBufferBytes,
		StreamRecvLimitBytes: r.cfg.StreamRecvLimitBytes,
		TotalRecvLimitBytes:  r.cfg.TotalRecvLimitBytes,
		IdleTimeout:          r.cfg.IdleTimeout,
		FrameWriteTimeout:    30 * time.Second,
		ReadRateLimit:        r.cfg.BandwidthBytesPerSec,
		WriteRateLimit:       r.cfg.BandwidthBytesPerSec,
		SendPings:            true,
		Diagnostic:           r.cfg.Diagnostic,
		Logger:               r.log,
	})
	m.SetPeerHeartbeat(int(r.cfg.HeartbeatInterval / time.Millisecond))
	d.mux.Store(m)
	r.diag.Records(diag.PrefixDeviceLinked, "device paired with mux",
		"device", deviceID,
		"remote", req.RemoteAddr,
		"heartbeat_ms", r.cfg.HeartbeatInterval.Milliseconds(),
		"idle_timeout", r.cfg.IdleTimeout.String(),
		"max_streams", r.cfg.MaxStreamsPerDevice)

	if r.cfg.MaxConnDuration > 0 {
		stop := time.AfterFunc(r.cfg.MaxConnDuration, func() {
			r.log.Info("relay: max connection duration reached, kicking device", "deviceId", deviceID)
			_ = m.Close()
		})
		defer stop.Stop()
	}

	select {
	case <-m.Done():
	case <-devCtx.Done():
	}

	r.mu.Lock()
	stillCurrent := false
	if cur, exists := r.devices[deviceID]; exists && cur == d {
		delete(r.devices, deviceID)
		stillCurrent = true
	}
	r.mu.Unlock()
	reason, closeCode := m.Diagnose()
	r.log.Info("relay: device disconnected",
		"deviceId", deviceID,
		"uptime", m.Uptime().Truncate(time.Second).String(),
		"reason", reason,
		"closeCode", closeCode,
		"streamsAtClose", m.StreamCount())
	r.diag.Records(diag.PrefixDeviceLost, "device connection ended",
		"device", deviceID,
		"remote", req.RemoteAddr,
		"uptime_s", fmt.Sprintf("%.1f", m.Uptime().Seconds()),
		"reason", reason,
		"close_code", closeCode,
		"streams", m.StreamCount(),
		"current", stillCurrent)
	_ = m.Close()
}

// pickDevice resolves the target device for a phone request. v1 supports a
// single device; the optional /d/<deviceId>/ path prefix routes explicitly.
func (r *Relay) pickDevice(req *http.Request) (*device, string) {
	path := req.URL.Path
	if strings.HasPrefix(path, "/d/") {
		trimmed := strings.TrimPrefix(path, "/d/")
		id, rest, _ := strings.Cut(trimmed, "/")
		if id == "" {
			return nil, ""
		}
		r.mu.Lock()
		d, exists := r.devices[id]
		r.mu.Unlock()
		if !exists {
			return nil, ""
		}
		return d, "/" + rest
	}
	// Root path with several devices: honor the default-node file (the
	// remote-access plugin's node switch) when it names a paired device.
	// Re-read per request: the file is tiny and this keeps switching live
	// without any reload handshake.
	if id := r.readDefaultNode(); id != "" {
		r.mu.Lock()
		d, exists := r.devices[id]
		r.mu.Unlock()
		if exists {
			return d, path
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.devices) == 1 {
		for _, d := range r.devices {
			return d, path
		}
	}
	return nil, ""
}

// readDefaultNode returns the trimmed device id from DefaultNodeFile, or "".
// Never invents state: unreadable/empty file means no default.
func (r *Relay) readDefaultNode() string {
	if r.cfg.DefaultNodeFile == "" {
		return ""
	}
	b, err := os.ReadFile(r.cfg.DefaultNodeFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// handleDefaultNode serves GET (current default) and POST (set it) on
// /__dsh_tunnel__/default-node. POST requires relay authorization and a
// JSON body {"deviceId": "..."}; the id must be a paired device. GET also
// requires authorization when the relay is token-gated (it echoes a device
// id, which the public probe deliberately hides).
func (r *Relay) handleDefaultNode(w http.ResponseWriter, req *http.Request) {
	if r.cfg.AccessToken != "" && !r.authorize(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch req.Method {
	case http.MethodGet:
		id := r.readDefaultNode()
		r.mu.Lock()
		_, paired := r.devices[id]
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if id == "" || !paired {
			_, _ = w.Write([]byte(`{"deviceId":""}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"deviceId": id})
	case http.MethodPost:
		if r.cfg.DefaultNodeFile == "" {
			http.Error(w, "default node not configured on this relay", http.StatusNotImplemented)
			return
		}
		var body struct {
			DeviceID string `json:"deviceId"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, "bad json body", http.StatusBadRequest)
			return
		}
		id := strings.TrimSpace(body.DeviceID)
		r.mu.Lock()
		_, paired := r.devices[id]
		r.mu.Unlock()
		if id == "" || !paired {
			http.Error(w, "unknown deviceId", http.StatusBadRequest)
			return
		}
		if err := os.WriteFile(r.cfg.DefaultNodeFile, []byte(id+"\n"), 0o600); err != nil {
			http.Error(w, "persist failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		r.log.Info("relay: default node set", "deviceId", id)
		r.broadcastDefaultNode(id)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"deviceId": id})
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// broadcastDefaultNode pushes the new id to every SSE watcher (non-blocking).
func (r *Relay) broadcastDefaultNode(id string) {
	r.nodeWatchMu.Lock()
	defer r.nodeWatchMu.Unlock()
	for ch := range r.nodeWatchers {
		select {
		case ch <- id:
		default: // slow subscriber: drop, it will resync on reconnect
		}
	}
}

// handleDefaultNodeWatch streams default-node changes as SSE. The first frame
// is the current value so a fresh page can sync immediately; subsequent frames
// arrive the instant a POST lands (no polling latency). Heartbeat comments
// every 20s keep proxies from idling the stream out.
func (r *Relay) handleDefaultNodeWatch(w http.ResponseWriter, req *http.Request) {
	if r.cfg.AccessToken != "" && !r.authorize(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan string, 8)
	r.nodeWatchMu.Lock()
	if r.nodeWatchers == nil {
		r.nodeWatchers = make(map[chan string]struct{})
	}
	r.nodeWatchers[ch] = struct{}{}
	r.nodeWatchMu.Unlock()
	defer func() {
		r.nodeWatchMu.Lock()
		delete(r.nodeWatchers, ch)
		r.nodeWatchMu.Unlock()
	}()

	write := func(id string) bool {
		_, err := fmt.Fprintf(w, "data: %s\n\n", id)
		if err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !write(r.readDefaultNode()) {
		return
	}
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-req.Context().Done():
			return
		case id := <-ch:
			if !write(id) {
				return
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": hb\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handlePhone routes a phone HTTP request to the paired device.
func (r *Relay) handlePhone(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	// 请求耗时埋点：分阶段计时，定位"手机连接慢"的卡点。
	stage := func(name string, t0 time.Time) time.Time {
		now := time.Now()
		if d := now.Sub(t0); d > 100*time.Millisecond {
			r.log.Info("relay: slow-stage", "path", req.URL.Path, "stage", name, "dur_ms", d.Milliseconds())
		}
		return now
	}
	defer func() {
		r.log.Info("relay: request", "method", req.Method, "path", req.URL.Path,
			"total_ms", time.Since(start).Milliseconds())
	}()
	t := start

	if !r.authorize(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Multi-node shell: the bare root serves the relay-owned switcher page
	// (same-origin iframes per node) instead of one node's GUI. Only the
	// exact root HTML navigation — assets/APIs and /d/<id>/ paths proxy as
	// before, and a single paired device keeps the legacy direct-GUI root.
	if req.URL.Path == "/" && (req.Method == http.MethodGet || req.Method == http.MethodHead) && r.pairedCount() >= 2 {
		r.handleShell(w, req)
		return
	}
	if !websocket.IsWebSocketUpgrade(req) && req.Body != nil {
		req.Body = http.MaxBytesReader(w, req.Body, r.cfg.RequestBodyLimit)
	}
	d, path := r.pickDevice(req)
	if d == nil {
		http.Error(w, "no device paired", http.StatusServiceUnavailable)
		return
	}
	t = stage("pickDevice", t)

	headers := make(map[string][]string, len(req.Header))
	for k, vs := range req.Header {
		headers[k] = append([]string(nil), vs...)
	}

	// Rewrite the target path (drops the /d/<deviceId>/ routing prefix).
	target := path
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}

	upgrade := websocket.IsWebSocketUpgrade(req)
	dm := d.mux.Load()
	if dm == nil {
		// device 刚配对、mux 尚未就绪的短暂窗口：返回 503 让客户端重试。
		http.Error(w, "device pairing in progress", http.StatusServiceUnavailable)
		return
	}
	s, err := dm.OpenStream(req.Method, target, headers, upgrade)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	// 兜底释放:任何 return 路径都不留流在 mux map 里(幂等)。此前
	// WaitAccept 失败等分支不 Close,流只能等 5m idle sweep,高峰期
	// 堆积加速连接耗尽(2026-08-19 实测)。
	defer s.Close()
	t = stage("openStream", t)

	if !upgrade {
		if req.Body != nil {
			if _, err := io.Copy(s, req.Body); err != nil {
				// 必须先发 stream.end:只 s.Close() 时对端(tunnel)的
				// io.Copy 读不到 EOF 会永久阻塞,本地 TCP 连接泄漏。
				_ = s.CloseWrite()
				return
			}
		}
		// Half-close: the request is complete. Upgrade streams keep the
		// pipe open; pipeUpgrade drives the phone->stream direction.
		if err := s.CloseWrite(); err != nil {
			return
		}
	}
	t = stage("bodySent", t)

	info, err := s.WaitAccept()
	if err != nil {
		var rej *mux.RejectedError
		if errors.As(err, &rej) {
			http.Error(w, "device stream rejected", rej.Status)
			return
		}
		http.Error(w, "device connection lost", http.StatusBadGateway)
		return
	}
	t = stage("waitAccept", t)

	if upgrade {
		r.pipeUpgrade(w, req, s, info)
		return
	}

	// Copy the response head, dropping hop-by-hop headers that Go's
	// ResponseWriter must not see.
	for k, vs := range info.Headers {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	// Node-prefixed responses: scope each node's dsh-auth cookie to its own
	// /d/<id>/ path so coexisting node iframes cannot overwrite each other's
	// session cookie (same cookie name, different per-node secrets).
	if strings.HasPrefix(req.URL.Path, "/d/") {
		if id, _, ok := strings.Cut(strings.TrimPrefix(req.URL.Path, "/d/"), "/"); ok && id != "" {
			scopeNodeCookies(w.Header(), "/d/"+id)
		}
	}
	status := info.Status
	if status == 0 {
		status = http.StatusOK
	}
	// 静态资源缓存：dsh 前端对 /assets/*（文件名带内容哈希）不返回
	// Cache-Control/ETag，手机每次启动都重新下载主 bundle（约 300ms）。
	// relay 作为手机侧边缘注入永久缓存；插件 client 包 dsh 已返回
	// no-cache，且 URL 无哈希，不覆盖。只对 2xx 且上游无 Cache-Control
	// 的响应生效，API/SSE 不受影响。
	if cc := staticCacheControl(req.URL.Path); cc != "" && status >= 200 && status < 300 {
		if _, exists := info.Headers["Cache-Control"]; !exists {
			w.Header().Set("Cache-Control", cc)
		}
	}
	// 透明 gzip：text/json 类大响应（如 session.history 几 MB）在公网
	// 传输前压缩，显著缩短手机加载时间。SSE（text/event-stream）不压缩
	// （压缩缓冲会憋死心跳流）；客户端不接受 gzip 或响应已编码时不压。
	ct := ""
	if vs := info.Headers["Content-Type"]; len(vs) > 0 {
		ct = strings.ToLower(vs[0])
	}
	useGzip := canGzip(req.Header.Get("Accept-Encoding"), ct) && len(info.Headers["Content-Encoding"]) == 0
	if useGzip {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(status)
	// 流式转发必须逐块 flush：dsh 的 SSE（/plugins/events 等）靠
	// keep-alive 小包维持心跳，Go ResponseWriter 默认缓冲 2KB 会导致
	// 事件流长时间无数据，客户端判死触发 HMR reload，打断 bundle 加载
	// （"Failed to load plugins"）。
	var sink io.Writer = w
	var gz *gzip.Writer
	if useGzip {
		gz = gzip.NewWriter(w)
		sink = gz
	}
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var bytesOut int64
	firstByte := true
	aborted := false
	for {
		n, err := s.Read(buf)
		if n > 0 {
			// 首字节（TTFB）单独计时：慢在设备侧还是传输侧一目了然。
			if firstByte {
				firstByte = false
				r.log.Info("relay: first-byte", "path", req.URL.Path,
					"ttfb_ms", time.Since(start).Milliseconds())
			}
			if _, werr := sink.Write(buf[:n]); werr != nil {
				// 手机侧断开：必须发 stream.reset 通知 tunnel 立即关闭
				// 本地连接——否则 tunnel 端 io.Copy 持续写活跃 SSE 流、
				// idle sweep 判定「活跃」永不清理，本地 3082 连接泄漏。
				aborted = true
				break
			}
			bytesOut += int64(n)
			if gz != nil {
				_ = gz.Flush()
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	if gz != nil {
		_ = gz.Close()
	}
	if aborted {
		_ = s.Reset()
	} else {
		_ = s.Close()
	}
	r.log.Info("relay: response-done", "path", req.URL.Path,
		"bytes", bytesOut, "total_ms", time.Since(start).Milliseconds())
}

// canGzip reports whether the response should be transparently compressed:
// client accepts gzip, content type is compressible, and not SSE.
func canGzip(acceptEncoding, contentType string) bool {
	if !strings.Contains(strings.ToLower(acceptEncoding), "gzip") {
		return false
	}
	if strings.HasPrefix(contentType, "text/event-stream") {
		return false
	}
	if strings.HasPrefix(contentType, "text/") ||
		strings.Contains(contentType, "json") ||
		strings.Contains(contentType, "javascript") {
		return true
	}
	return false
}

// staticCacheControl returns the Cache-Control value to inject for a static
// asset path, or "" when the path should not be cached. dsh 前端对 /assets/*
// （文件名带内容哈希、天然不可变）不返回任何缓存头，手机 WebView 每次都
// 重新下载主 bundle。relay 为这类路径注入永久缓存。插件 client 包 URL 无
// 哈希且 dsh 已返回 no-cache，不动；API/SSE 也不动。
func staticCacheControl(path string) string {
	if strings.HasPrefix(path, "/assets/") {
		return "public, max-age=31536000, immutable"
	}
	return ""
}

// pipeUpgrade bridges an upgraded (websocket) connection: hijack the phone
// connection, write the 101 response head, then pipe raw bytes both ways.
func (r *Relay) pipeUpgrade(w http.ResponseWriter, _ *http.Request, s *mux.Stream, info mux.AcceptInfo) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "upgrade unsupported", http.StatusInternalServerError)
		_ = s.Close()
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		http.Error(w, "hijack failed", http.StatusInternalServerError)
		_ = s.Close()
		return
	}
	defer conn.Close()

	status := info.Status
	if status == 0 {
		status = http.StatusSwitchingProtocols
	}
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	for k, vs := range info.Headers {
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	if _, err := brw.WriteString(b.String()); err != nil {
		_ = s.Close()
		return
	}
	_ = brw.Flush()

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		// Drain any buffered bytes then raw-pipe from the phone.
		_, _ = io.Copy(s, brw)
		_ = s.CloseWrite()
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		_, _ = io.Copy(conn, s)
		_ = conn.Close()
	}()
	<-done
	// 手机断开或流结束时显式 reset，通知 tunnel 关闭本地连接，防
	// upgrade 流的本地 TCP 泄漏（同 handlePhone 的 SSE 泄漏根因）。
	_ = s.Reset()
}

func isHopByHop(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

func (r *Relay) closeDevices() {
	r.mu.Lock()
	devices := make([]*device, 0, len(r.devices))
	for _, d := range r.devices {
		devices = append(devices, d)
	}
	r.devices = make(map[string]*device)
	r.mu.Unlock()
	for _, d := range devices {
		d.cancel()
		if dm := d.mux.Load(); dm != nil {
			_ = dm.Close()
		}
	}
}

// DeviceCount reports how many devices are paired (for tests/metrics).
func (r *Relay) DeviceCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.devices)
}
