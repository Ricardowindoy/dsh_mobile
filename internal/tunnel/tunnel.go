// Package tunnel implements the dsh-side outbound connector: it dials the
// public relay over wss, pairs with a token, and forwards multiplexed
// streams to the local dsh web server (127.0.0.1:3080 by default).
//
// Security note: before replaying a forwarded request to the local server,
// headers are normalized — Host is rewritten to the loopback target and
// Origin / Sec-Fetch-Site are stripped, so the request passes dsh's existing
// browser trust gate as a "local user". Authentication is the relay's job;
// inside the tunnel everything is treated as trusted loopback (same trust
// model as SSH port forwarding).
package tunnel

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"dsh_mobile/internal/diag"
	"dsh_mobile/internal/mux"
	"dsh_mobile/internal/protocol"
)

// Config configures the outbound tunnel.
type Config struct {
	// RelayURL is the relay's device endpoint, e.g. wss://relay.example.com/device.
	RelayURL string
	// RelayURLs are alternative endpoints tried in order when a dial fails.
	// The public entry points are independent of each other (bare IP + pinned
	// private CA vs. a domain behind a reverse proxy), so if one of them stops
	// resolving or stops answering, the tunnel still comes up on the other
	// instead of retrying a dead host forever.
	RelayURLs []string
	// DeviceID identifies this machine to the relay.
	DeviceID string
	// PairingToken authenticates the device; supplied via the credentials
	// seam (env / file), never hard-coded.
	PairingToken string
	// LocalAddr is the dsh web server address to forward to.
	LocalAddr string
	// AllowLoopbackPorts enables the /__dsh_loopback__/<port>/... path that
	// carries RFC 8252 loopback OAuth callbacks. Default OFF: enabling it lets
	// any holder of the relay access token reach arbitrary loopback ports on
	// this machine (same trust model as `ssh -L`, but an explicit opt-in).
	// The tunnel only ever dials 127.0.0.1 — the phone can never name a host.
	AllowLoopbackPorts bool
	// GUICookieAuthority is the Host authority the local dsh web sees (the
	// --local target, e.g. "127.0.0.1:3082"); it keys the node's own auth
	// cookie name used by the GUI-token injection gate. Required whenever
	// GUIAuthTokenFile is set.
	GUICookieAuthority string
	// GUIAuthTokenFile optionally points at a file holding dsh web's current
	// launch token (the one dsh prints as `/?token=...`). When set, a root GET
	// gets that token appended once so the local GUI can mint its auth cookie;
	// the token is per-process, so the file must be refreshed on every dsh web
	// restart. Empty disables the injection.
	GUIAuthTokenFile string
	// GUITokenJournalUnit is the systemd unit whose journal dsh web's startup
	// token is scraped from (e.g. "dsh-web.service"). Empty disables the journal
	// source; the heap scan still runs when GUIAuthTokenFile is set.
	GUITokenJournalUnit string
	// GUITokenJournalUser selects journalctl --user for the unit above.
	GUITokenJournalUser bool
	// HeartbeatInterval, defaults to protocol.DefaultHeartbeatMs.
	HeartbeatInterval time.Duration
	// MaxStreams caps concurrent forwarded streams.
	MaxStreams int
	// PerStreamBufferBytes caps per-stream buffering before backpressure.
	PerStreamBufferBytes int
	// StreamRecvLimitBytes is the hard per-stream receive-queue cap (0 keeps
	// PerStreamBufferBytes*8). Raises the ceiling for large request bodies
	// (file uploads) so a slow consumer cannot reset them mid-transfer.
	StreamRecvLimitBytes int
	// TotalRecvLimitBytes bounds the sum of all receive queues (0 = unlimited).
	TotalRecvLimitBytes int
	// IdleTimeout closes idle streams (0 disables).
	IdleTimeout time.Duration
	// DiscoverInterval overrides the GUI-token discovery cadence
	// (0 = 5s default). Tests use a short interval.
	DiscoverInterval time.Duration
	// ReconnectMin / ReconnectMax bound the exponential backoff (1s..30s).
	ReconnectMin time.Duration
	ReconnectMax time.Duration
	// Diagnostic, when non-nil, receives one line per connection lifecycle
	// event: dial attempts, pairing, losses (with reason and uptime) and
	// recoveries (with total time from loss to re-paired). Use
	// diag.NewFileWriter for a file-backed sink; the caller owns closing it.
	Diagnostic io.Writer
	// Watchdog probes the public entrance on a cadence and escalates
	// (switch endpoint → exit for supervisor restart) when the link is dead
	// even though the mux is up. Zero value (no PublicProbeURL) = off.
	Watchdog WatchdogConfig
	// WatchdogExit overrides the process-exit seam (tests only).
	WatchdogExit func(code int)
	// Logger, defaults to slog.Default().
	Logger *slog.Logger
}

// Tunnel is a running outbound connector.
type Tunnel struct {
	cfg         Config
	guiNameOnce sync.Once
	guiName     string
	guiToken    guiTokenState
	// rejectUntil is the negative cache of token candidates that failed the
	// probe (see discoverGUIToken): without it the same heap garbage gets
	// re-probed every 5s for the process lifetime.
	rejectMu    sync.Mutex
	rejectUntil map[string]time.Time
	// discoverRuns / validTicks count discovery-loop behavior (diagnostics +
	// regression tests): discovery must NOT run while the cached token is
	// still valid — that unconditional path was the 5s heap-scan leak.
	discoverRuns    atomic.Int64
	validTicks      atomic.Int64
	discoverBackoff time.Duration // only touched by the discovery goroutine
	// Test seams for the heap-scan memory gate (nil/0 in production).
	heapPidOverride    int
	heapRegionOverride func(int) (int, int, error)
	log                *slog.Logger
	// upstream writes lifecycle lines (same sink as the mux diagnostics).
	upstream *diag.Logger
	// tlsCache lets WebSocket reconnects resume the TLS session (1×RTT
	// handshake instead of 2×RTT). Shared for the tunnel's lifetime; safe for
	// concurrent use by the reconnect loop.
	tlsCache tls.ClientSessionCache

	mu      sync.Mutex
	mux     *mux.Mux
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	closed  chan struct{}
	closeOn sync.Once

	// attempts counts dial attempts since start-up; disconnectedAt is when
	// the previous link went away (zero before the first dial), which is the
	// baseline for "how long did reconnecting actually take".
	attempts       int
	disconnectedAt time.Time
	lastReason     string
	// relayIdx selects which RelayURLs entry the next dial uses; it advances
	// on every failed dial so a dead host cannot pin the reconnect loop.
	relayIdx int
}

func defaultConfig(cfg *Config) Config {
	c := *cfg
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = protocol.DefaultHeartbeatMs * time.Millisecond
	}
	if c.MaxStreams <= 0 {
		c.MaxStreams = 64
	}
	if c.PerStreamBufferBytes <= 0 {
		c.PerStreamBufferBytes = 256 * 1024
	}
	if c.LocalAddr == "" {
		c.LocalAddr = "127.0.0.1:3080"
	}
	if c.ReconnectMin <= 0 {
		c.ReconnectMin = time.Second
	}
	if c.ReconnectMax <= 0 {
		c.ReconnectMax = 30 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// New creates a tunnel and starts its dial/reconnect loop.
func New(cfg Config) *Tunnel {
	c := defaultConfig(&cfg)
	t := &Tunnel{
		cfg:      c,
		log:      c.Logger,
		closed:   make(chan struct{}),
		tlsCache: tls.NewLRUClientSessionCache(0), // 0 = default capacity (64)
	}
	t.upstream = diag.NewWithOptions(c.Diagnostic, c.Logger, diag.Options{Header: "tunnel " + c.RelayURL + " -> " + c.LocalAddr})
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.wg.Add(1)
	go t.run(ctx)
	if c.GUIAuthTokenFile != "" {
		t.wg.Add(1)
		go t.discoverGUIToken(ctx)
	}
	if c.Watchdog.PublicProbeURL != "" {
		t.wg.Add(1)
		go t.runWatchdog(ctx)
	}
	return t
}

// relayHosts returns the configured endpoints in priority order (primary
// first, then the alternatives). Always non-empty for a valid config.
func (t *Tunnel) relayHosts() []string {
	hosts := make([]string, 0, 1+len(t.cfg.RelayURLs))
	for _, h := range append([]string{t.cfg.RelayURL}, t.cfg.RelayURLs...) {
		if h = strings.TrimSpace(h); h != "" && !containsString(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		hosts = append(hosts, t.cfg.RelayURL)
	}
	return hosts
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// nextRelayHost returns the host to dial now and how many are configured.
func (t *Tunnel) nextRelayHost() (string, int) {
	hosts := t.relayHosts()
	t.mu.Lock()
	idx := t.relayIdx % len(hosts)
	t.mu.Unlock()
	return hosts[idx], len(hosts)
}

// advanceRelayHost moves to the next configured endpoint (after a failed
// dial) and reports the switch when more than one is configured.
func (t *Tunnel) advanceRelayHost() (string, bool) {
	hosts := t.relayHosts()
	if len(hosts) < 2 {
		return "", false
	}
	t.mu.Lock()
	t.relayIdx = (t.relayIdx + 1) % len(hosts)
	idx := t.relayIdx
	t.mu.Unlock()
	return hosts[idx], true
}

// Close stops the tunnel and all its streams.
func (t *Tunnel) Close() {
	t.closeOn.Do(func() {
		t.cancel()
		close(t.closed)
		t.mu.Lock()
		if t.mux != nil {
			_ = t.mux.Close()
		}
		t.mu.Unlock()
	})
	t.wg.Wait()
}

// Connected reports whether a mux is currently established.
func (t *Tunnel) Connected() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mux != nil
}

// stableFor is how long a link must survive before the next attempt starts
// from ReconnectMin again. Without this, one brief network blip after hours of
// uptime would leave every later reconnect paying a 15-30s backoff.
const stableFor = 30 * time.Second

// run dials, pairs and reconnects with exponential backoff + jitter. Every
// step is reported to the lifecycle log, because "it keeps disconnecting and
// takes ages to come back" is unanswerable from per-request logs alone.
func (t *Tunnel) run(ctx context.Context) {
	defer t.wg.Done()
	backoff := t.cfg.ReconnectMin
	for {
		attempt := t.nextAttempt()
		started := time.Now()
		err := t.dialOnce(ctx, attempt)
		elapsed := time.Since(started)
		if ctx.Err() != nil {
			return
		}

		// 备用入口：一次失败就换下一个主机，避免把重连循环钉在死主机上。
		if next, switched := t.advanceRelayHost(); switched {
			t.log.Warn("tunnel: switching relay endpoint", "next", next)
			t.upstream.Records(diag.PrefixTunnelRetry, "switching relay endpoint",
				"next", next, "attempt", attempt)
		}

		// A link that lived long enough resets the backoff ladder.
		if elapsed >= stableFor {
			if backoff != t.cfg.ReconnectMin {
				t.log.Info("tunnel: link was stable, reconnect backoff reset",
					"uptime", elapsed.Truncate(time.Second).String())
			}
			backoff = t.cfg.ReconnectMin
		}

		t.log.Warn("tunnel: connection lost, reconnecting",
			"err", err, "attempt", attempt,
			"uptime", elapsed.Truncate(time.Second).String(),
			"backoff", backoff.String())

		// Report the planned wait *after* it happened, so the logged value is
		// the real one rather than a prediction.
		jitter := time.Duration(rand.Int63n(int64(backoff/2) + 1))
		waited := t.sleepCtx(ctx, backoff, jitter)
		if ctx.Err() != nil {
			return
		}
		t.upstream.Records(diag.PrefixTunnelRetry, "retrying",
			"attempt", attempt+1,
			"waited_ms", waited.Milliseconds(),
			"backoff_ms", backoff.Milliseconds(),
			"since_loss_ms", t.sinceLossMs())

		backoff *= 2
		if backoff > t.cfg.ReconnectMax {
			backoff = t.cfg.ReconnectMax
		}
	}
}

// sinceLossMs reports how long ago the last established link went away, or -1
// when no link has ever been established. The zero-value guard matters: with a
// naive time.Since() the "recovery cost" field would overflow into
// 9223372036854 ms (measured) whenever the tunnel had not yet paired once.
func (t *Tunnel) sinceLossMs() int64 {
	t.mu.Lock()
	at := t.disconnectedAt
	t.mu.Unlock()
	if at.IsZero() {
		return -1
	}
	return time.Since(at).Milliseconds()
}

// nextAttempt allocates the next dial attempt number.
func (t *Tunnel) nextAttempt() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.attempts++
	return t.attempts
}

// sleepCtx waits for d plus a random jitter, returning how long it actually
// waited. Returns early when ctx is cancelled.
func (t *Tunnel) sleepCtx(ctx context.Context, d, jitter time.Duration) time.Duration {
	start := time.Now()
	select {
	case <-ctx.Done():
		return time.Since(start)
	case <-time.After(d):
	}
	select {
	case <-ctx.Done():
		return time.Since(start)
	case <-time.After(jitter):
	}
	return time.Since(start)
}

// dialOnce establishes one connection and runs it until it dies. attempt is
// the 1-based dial attempt number, used for the lifecycle log and to tell a
// start-up dial apart from a reconnect.
func (t *Tunnel) dialOnce(ctx context.Context, attempt int) error {
	host, hostCount := t.nextRelayHost()
	t.upstream.Records(diag.PrefixTunnelAttempt, "dialing relay",
		"attempt", attempt,
		"relay", host,
		"hosts", hostCount,
		"local", t.cfg.LocalAddr)

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		// 12MB bundle = ~740 个 16KB 帧在途；WebSocket 层缓冲太小会让
		// WriteMessage 在内核 socket 缓冲之上再排队，帧间出现明显停顿
		// （表现为传输速率波动 3 倍）。给足内核侧缓冲。
		ReadBufferSize:  64 * 1024,
		WriteBufferSize: 64 * 1024,
		// 断线重连必须快：TLS session resumption 复用上次握手的密钥材料，
		// 把重连握手从 2×RTT 压到 1×RTT（弱网下省 100~300ms）。gorilla 的
		// dialer 每次新建 TLS config 不会自动持有 ClientSessionCache，这里
		// 显式挂上，跨重连共享。
		TLSClientConfig: t.tlsConfig(),
		NetDialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second, // NAT/防火墙 idle 兜底，应用层心跳之外的保险
		}).DialContext,
	}
	dialStart := time.Now()
	conn, resp, err := dialer.DialContext(ctx, host, nil)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		t.upstream.Records(diag.PrefixTunnelLost, "dial failed",
			"attempt", attempt,
			"stage", "dial",
			"endpoint", host,
			"dur_ms", time.Since(dialStart).Milliseconds(),
			"http_status", httpStatusOf(resp),
			"reason", "dial: "+err.Error())
		return fmt.Errorf("dial relay: %w", err)
	}
	dialMs := time.Since(dialStart).Milliseconds()

	// Pairing handshake: hello -> ok/err.
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	hello, err := protocol.Marshal(protocol.Hello(t.cfg.DeviceID, t.cfg.PairingToken))
	if err != nil {
		_ = conn.Close()
		return err
	}
	if err := conn.WriteMessage(websocket.TextMessage, hello); err != nil {
		_ = conn.Close()
		t.upstream.Records(diag.PrefixTunnelLost, "handshake failed",
			"attempt", attempt, "stage", "send-hello",
			"endpoint", host,
			"dur_ms", time.Since(dialStart).Milliseconds(),
			"reason", "send hello: "+err.Error())
		return fmt.Errorf("send hello: %w", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	msgType, data, err := conn.ReadMessage()
	if err != nil {
		_ = conn.Close()
		t.upstream.Records(diag.PrefixTunnelLost, "handshake failed",
			"attempt", attempt, "stage", "read-hello-reply",
			"endpoint", host,
			"dur_ms", time.Since(dialStart).Milliseconds(),
			"reason", "read hello reply: "+err.Error())
		return fmt.Errorf("read hello reply: %w", err)
	}
	if msgType != websocket.TextMessage {
		_ = conn.Close()
		return fmt.Errorf("unexpected hello reply type %d", msgType)
	}
	reply, err := protocol.Unmarshal(data)
	if err != nil {
		_ = conn.Close()
		return err
	}
	switch reply.Type {
	case protocol.TypeOK:
	case protocol.TypeErr:
		_ = conn.Close()
		t.upstream.Records(diag.PrefixTunnelLost, "relay rejected pairing",
			"attempt", attempt, "stage", "pair",
			"reason", reply.Reason,
			"dur_ms", time.Since(dialStart).Milliseconds())
		return fmt.Errorf("relay rejected pairing: %s", reply.Reason)
	default:
		_ = conn.Close()
		return fmt.Errorf("unexpected hello reply %q", reply.Type)
	}
	// Handshake deadlines must not linger into the long-lived mux: the
	// relay sends pings every heartbeat and the tunnel answers pongs.
	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Time{})

	// Recovery cost: from the moment the previous link died to the moment
	// this one finished pairing — the number the user actually experiences.
	sinceLoss := t.sinceLossMs()
	t.upstream.Records(diag.PrefixTunnelLinked, "paired with relay",
		"attempt", attempt,
		"dial_ms", dialMs,
		"handshake_ms", time.Since(dialStart).Milliseconds()-dialMs,
		"since_loss_ms", sinceLoss,
		"heartbeat_ms", reply.HeartbeatMs,
		"prev_reason", t.lastReason)
	if sinceLoss >= 0 {
		t.upstream.Records(diag.PrefixTunnelRecover, "recovered",
			"attempt", attempt,
			"since_loss_ms", sinceLoss)
	}
	t.log.Info("tunnel: paired with relay",
		"relay", host,
		"attempt", attempt,
		"dial_ms", dialMs,
		"since_loss_ms", sinceLoss)

	m := mux.New(conn, mux.Config{
		HeartbeatInterval:    t.cfg.HeartbeatInterval,
		MaxStreams:           t.cfg.MaxStreams,
		PerStreamBufferBytes: t.cfg.PerStreamBufferBytes,
		StreamRecvLimitBytes: t.cfg.StreamRecvLimitBytes,
		TotalRecvLimitBytes:  t.cfg.TotalRecvLimitBytes,
		IdleTimeout:          t.cfg.IdleTimeout,
		// The tunnel's producer (local dsh web on loopback) is far faster than
		// the uplink; a deeper write queue smooths bursts instead of stalling
		// the TCP window on every 64-frame refill. 256 frames ≈ 4MB.
		WriteQueueFrames:  256,
		FrameWriteTimeout: 30 * time.Second,
		OnStreamOpen:      t.handleStreamOpen,
		Diagnostic:        t.cfg.Diagnostic,
		Logger:            t.log,
	})
	m.SetPeerHeartbeat(reply.HeartbeatMs)

	t.mu.Lock()
	t.mux = m
	t.mu.Unlock()

	// 只有真正建立过连接才记录"断开时刻"：拨号/握手/配对失败发生在连接之前，
	// 若也写 disconnectedAt，后续 since_loss_ms 会算成负数（实测出现
	// 9223372036854 这种溢出值），把"恢复耗时"这一核心指标变成噪声。
	<-m.Done()
	reason, closeCode := m.Diagnose()
	t.mu.Lock()
	t.mux = nil
	t.disconnectedAt = time.Now()
	t.lastReason = reason
	t.mu.Unlock()
	t.upstream.Records(diag.PrefixTunnelLost, "link down",
		"reason", reason,
		"close_code", closeCode,
		"uptime_s", fmt.Sprintf("%.1f", m.Uptime().Seconds()),
		"streams", m.StreamCount())
	return ErrMuxClosed
}

// httpStatusOf reports the HTTP status of a failed websocket dial handshake
// (0 when the failure happened before any HTTP response).
func httpStatusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// ErrMuxClosed is returned when the connection drops.
var ErrMuxClosed = errors.New("tunnel: mux closed")

// tlsConfig returns the TLS configuration for relay dials. Roots stay at the
// Go default so SSL_CERT_FILE keeps injecting the private CA (the documented
// mechanism); the only addition is the session cache, which turns reconnect
// handshakes into 1×RTT session resumption.
func (t *Tunnel) tlsConfig() *tls.Config {
	return &tls.Config{
		ClientSessionCache: t.tlsCache,
		MinVersion:         tls.VersionTLS12,
	}
}

// handleStreamOpen dials the local dsh web server and forwards one stream.
func (t *Tunnel) handleStreamOpen(s *mux.Stream, msg protocol.Message) {
	start := time.Now()
	defer s.Close()

	// A loopback callback (RFC 8252) arrives at the relay as
	// /__dsh_loopback__/<port>/<original path>; the relay is a byte pipe and
	// forwards the path verbatim. Resolve it HERE, on the trusted device, so
	// the phone only ever names a port number — never a host. Any other path
	// keeps the configured --local target.
	dialAddr, path, ok := resolveLoopback(t.cfg, msg.Path)
	if !ok {
		_ = s.Reject(403)
		return
	}
	// Reject before dialing so a disabled/unparsable loopback path never
	// silently falls through to the GUI's port (which would send an OAuth
	// callback into dsh web as a bogus route).
	local, err := net.DialTimeout("tcp", dialAddr, 5*time.Second)
	if err != nil {
		t.log.Warn("tunnel: local dial failed", "err", err, "sid", s.SID(), "addr", dialAddr)
		_ = s.Reject(502)
		return
	}
	defer local.Close()
	t.log.Info("tunnel: stream", "sid", s.SID(), "path", path, "addr", dialAddr,
		"dial_ms", time.Since(start).Milliseconds())

	// Replay the request with normalized headers. Host is rewritten to the
	// dial target so the local server's authority-bound checks see loopback.
	headers := normalizeHeaders(msg.Headers, dialAddr)
	// GUI launch-token injection must only apply to the main GUI target, never
	// to a loopback callback port (which is a different local service).
	tokenFile := t.cfg.GUIAuthTokenFile
	if dialAddr != t.cfg.LocalAddr {
		tokenFile = ""
	}
	target := t.injectGUIToken(msg.Method, path, headers, tokenFile)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", msg.Method, target)
	for k, vs := range headers {
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	if _, err := local.Write([]byte(b.String())); err != nil {
		t.log.Warn("tunnel: write request head failed", "err", err, "sid", s.SID())
		_ = s.Reject(502)
		return
	}

	// Request body: relay half-closes after sending it. Only half-close the
	// local socket when the server actually needs EOF to detect the end of
	// the request body (no Content-Length, no chunked encoding). Node's HTTP
	// server (dsh) aborts connections that are half-closed on a bodyless
	// request, so never CloseWrite for GET/HEAD or sized bodies.
	// Upgrade streams are raw TCP pipes: the stream EOF (phone hung up) must
	// propagate as a FIN so the local server closes its side.
	needsEOF := msg.Upgrade || requestNeedsEOF(msg.Method, headers)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(local, s)
		if needsEOF {
			if tc, ok := local.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
			}
		}
	}()

	// Response head.
	// 读响应头加 60s 超时:本地服务挂起不响应时防协程/TCP 永久泄漏
	// (此前 ReadResponse 可无限阻塞——流即使被 idle sweep 清理,协程仍
	// 卡死,到 3082 的连接越积越多。2026-08-19 实测 305 条泄漏连接)。
	// 响应头到手后立即清除超时,响应体(SSE 等流式)传输不受限。
	_ = local.SetReadDeadline(time.Now().Add(60 * time.Second))
	br := bufio.NewReader(local)
	resp, err := http.ReadResponse(br, nil)
	_ = local.SetReadDeadline(time.Time{})
	if err != nil {
		t.log.Warn("tunnel: read local response head failed", "err", err, "sid", s.SID())
		_ = s.Reject(502)
		// 不等请求体 goroutine:函数返回后 defer local.Close() 会关闭
		// TCP,io.Copy 写失败自行退出,无需在此等待。
		return
	}
	// GUI 401 自愈：根 GET 带着本节点 cookie 名却收到 401（cookie 过期）时，
	// 用当前 token 重试一次（dsh 对带 token 的请求幂等重签 303）。只重试一次
	// 且仅当原请求未带 token——避免与注入路径互相踩。
	if resp.StatusCode == http.StatusUnauthorized && msg.Method == http.MethodGet && !msg.Upgrade {
		base, query, _ := strings.Cut(path, "?")
		if base == "/" && !strings.Contains(query, "token=") && t.cfg.GUIAuthTokenFile != "" {
			own := t.guiCookieName()
			hasOwn := false
			for _, v := range msg.Headers["Cookie"] {
				if strings.Contains(v, own+"=") {
					hasOwn = true
					break
				}
			}
			if hasOwn {
				retryPath := t.injectGUIToken(msg.Method, path, map[string][]string{}, t.cfg.GUIAuthTokenFile)
				retryHeaders := normalizeHeaders(msg.Headers, dialAddr)
				var rb strings.Builder
				fmt.Fprintf(&rb, "%s %s HTTP/1.1\r\n", msg.Method, retryPath)
				for k, vs := range retryHeaders {
					for _, v := range vs {
						rb.WriteString(k + ": " + v + "\r\n")
					}
				}
				rb.WriteString("\r\n")
				local2, err2 := net.DialTimeout("tcp", dialAddr, 5*time.Second)
				if err2 == nil {
					defer local2.Close()
					if _, werr := local2.Write([]byte(rb.String())); werr == nil {
						_ = local2.SetReadDeadline(time.Now().Add(60 * time.Second))
						br2 := bufio.NewReader(local2)
						if resp2, err3 := http.ReadResponse(br2, nil); err3 == nil {
							_ = local2.SetReadDeadline(time.Time{})
							t.log.Info("tunnel: gui 401 retried with token", "sid", s.SID(), "status", resp2.StatusCode)
							resp = resp2
						}
					}
				}
			}
		}
	}
	defer resp.Body.Close()

	// 设备侧透明 gzip：tunnel→relay 走的是家庭宽带上行（真瓶颈），
	// 大响应（session.history 几 MB）必须压缩后再上传。relay 看到
	// Content-Encoding 已存在会直接透传不重复压。
	respHeader := map[string][]string(resp.Header)
	relaxSameSite(respHeader)
	useGzip := !msg.Upgrade && len(respHeader["Content-Encoding"]) == 0 &&
		tunnelCanGzip(msg.Headers["Accept-Encoding"], respHeader["Content-Type"])
	if useGzip {
		respHeader["Content-Encoding"] = []string{"gzip"}
		delete(respHeader, "Content-Length")
	}
	if err := s.Accept(resp.StatusCode, respHeader); err != nil {
		<-done
		return
	}
	t.log.Info("tunnel: head", "sid", s.SID(), "path", msg.Path,
		"status", resp.StatusCode, "gzip", useGzip, "head_ms", time.Since(start).Milliseconds())

	// Response body: pipe back to the relay until the local server closes.
	// For upgrade (101) responses ReadResponse leaves the raw connection
	// remainder (websocket frames) in br, so pipe br itself.
	//
	// 响应体转发放在 goroutine 里并同时监听 s.Done()：relay 手机侧断开会
	// 发 stream.reset，mux 收到后关闭 s.closed 触发 s.Done()。此时必须
	// 主动关闭本地连接——否则 io.Copy 卡在「读 resp.Body」（SSE 空闲期本地
	// 无数据，Write 的 ErrClosed 分支根本走不到），本地 TCP 连接仍泄漏。
	type copyResult struct {
		n   int64
		err error
	}
	copyDone := make(chan copyResult, 1)
	go func() {
		var n int64
		var cerr error
		if msg.Upgrade {
			n, cerr = io.Copy(s, br)
		} else if useGzip {
			// BestSpeed：设备侧是 6×A55 小核，默认压缩级别（-6）在这种
			// ARM 核上会把 12MB JS bundle 压到 1~2 MB/s，反而把家庭宽带上行
			// 的空闲时间变成 CPU 瓶颈（实测 4.8MB 资源传输速率波动 3 倍正与
			// CPU 抢占相关）。BestSpeed 在 JS/JSON 上仍能拿到 ~3x 压缩比，
			// 上行收益远大于级别损失。
			gz, err := gzip.NewWriterLevel(s, gzip.BestSpeed)
			if err != nil { // unreachable for a valid level; keep the fallback honest
				gz = gzip.NewWriter(s)
			}
			n, cerr = io.Copy(gz, resp.Body)
			if gzErr := gz.Close(); cerr == nil {
				cerr = gzErr
			}
		} else {
			n, cerr = io.Copy(s, resp.Body)
		}
		copyDone <- copyResult{n: n, err: cerr}
	}()

	var res copyResult
	select {
	case res = <-copyDone:
	case <-s.Done():
		// 对端 reset/关闭了流：关本地连接让 io.Copy 尽快退出。
		_ = local.Close()
		res = <-copyDone
	}
	bodyBytes := res.n
	err = res.err
	if err != nil {
		t.log.Debug("tunnel: response copy ended", "err", err, "sid", s.SID())
	}
	_ = s.CloseWrite()
	// 等请求体方向转发完成;流被关闭(sweep/对端关闭/mux 断开都会触发
	// s.Done())时立即返回,防永久卡死——relay 中断请求只 s.Close() 不发
	// stream.end 时,io.Copy(local, s) 读不到 EOF 会无限阻塞,导致本地
	// TCP 连接泄漏。
	select {
	case <-done:
	case <-s.Done():
	}
	t.log.Info("tunnel: done", "sid", s.SID(), "path", msg.Path,
		"bytes", bodyBytes, "gzip", useGzip, "total_ms", time.Since(start).Milliseconds())
}

// loopbackPathPrefix is the path prefix the App rewrites RFC 8252 loopback
// OAuth callbacks into. The relay forwards paths verbatim (it is a byte
// pipe), so resolution happens here, on the trusted device.
//
// Why the port is in the PATH and not a new stream.open field: the relay runs
// on the public ECS and is deployed independently from this binary. Encoding
// the port in the path keeps the wire protocol and the ECS side unchanged —
// only the local tunnel and the App need to be rebuilt.
const loopbackPathPrefix = "/__dsh_loopback__/"

// resolveLoopback maps a forwarded request path to (dial address, rewritten
// path, allowed). A non-loopback path always maps to cfg.LocalAddr unchanged.
// A loopback path is accepted only when AllowLoopbackPorts is enabled and the
// port is a valid 1..65535 decimal; the dial target is ALWAYS 127.0.0.1 —
// the phone can choose a port but never a host.
func resolveLoopback(cfg Config, path string) (string, string, bool) {
	if !strings.HasPrefix(path, loopbackPathPrefix) {
		return cfg.LocalAddr, path, true
	}
	rest := strings.TrimPrefix(path, loopbackPathPrefix)
	portStr, tail, _ := strings.Cut(rest, "/")
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", "", false
	}
	if !cfg.AllowLoopbackPorts {
		return "", "", false
	}
	// Normalize the remainder to an absolute path; keep the query as-is.
	rewritten := "/" + tail
	return "127.0.0.1:" + strconv.Itoa(port), rewritten, true
}

// tunnelCanGzip 判断响应是否适合设备侧压缩（与 relay 侧 canGzip 同规则：
// 客户端接受 gzip、可压缩类型、非 SSE）。
func tunnelCanGzip(acceptEncoding []string, contentType []string) bool {
	acceptOK := false
	for _, v := range acceptEncoding {
		if strings.Contains(strings.ToLower(v), "gzip") {
			acceptOK = true
			break
		}
	}
	if !acceptOK || len(contentType) == 0 {
		return false
	}
	ct := strings.ToLower(contentType[0])
	if strings.HasPrefix(ct, "text/event-stream") {
		return false
	}
	return strings.HasPrefix(ct, "text/") ||
		strings.Contains(ct, "json") ||
		strings.Contains(ct, "javascript")
}

// requestNeedsEOF reports whether the local server can only detect the end
// of the request body by a TCP half-close: no Content-Length and no
// chunked transfer encoding on a method that carries a body.
func requestNeedsEOF(method string, headers map[string][]string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	if _, ok := headers["Content-Length"]; ok {
		return false
	}
	for _, v := range headers["Transfer-Encoding"] {
		if strings.Contains(strings.ToLower(v), "chunked") {
			return false
		}
	}
	return true
}

// relaxSameSite rewrites `SameSite=Strict` to `SameSite=Lax` on Set-Cookie.
//
// Why: the phone's WebView starts the navigation from `tauri.localhost`, i.e.
// cross-site, and Chromium applies that origin to the whole redirect chain.
// dsh's GUI cookie is `SameSite=Strict`, so it is not sent on the redirect that
// follows the token exchange — the phone re-requests `/`, dsh answers 303
// again, and the WebView dies with `net::ERR_TOO_MANY_REDIRECTS` (measured on
// device). curl ignores SameSite, which is why the server-side flow looked
// fine in isolation. Lax still withholds the cookie from POST/iframe requests,
// so the CSRF protection that motivates Strict is preserved for what matters.
func relaxSameSite(h map[string][]string) {
	values, ok := h["Set-Cookie"]
	if !ok {
		return
	}
	for i, v := range values {
		if strings.Contains(v, "SameSite=Strict") {
			values[i] = strings.ReplaceAll(v, "SameSite=Strict", "SameSite=Lax")
		}
	}
}

// relayAccessCookie is the relay's own session cookie. It must never reach the
// local server (the relay credential is not the GUI's business).
const relayAccessCookie = "dsh_relay_token"

// guiAuthCookiePrefix is the prefix dsh web uses for its authority-bound
// browser session cookie (`dsh-auth-<hash-of-authority>`).
const guiAuthCookiePrefix = "dsh-auth-"

// injectGUIToken appends dsh web's launch token to a root GET so the GUI can
// mint its auth cookie through the tunnel.
//
// Why this is needed: dsh web gates its index with a per-process launch token
// (`/?token=<tok>` → 303 + signed cookie bound to the request authority), and
// the tunnel speaks to it as 127.0.0.1. Without the token the phone would see
// `dsh web authentication required`. The gate accepts exactly one `token`
// query parameter on `GET /`; every other request is left untouched.
//
// Injection is keyed to the node's OWN cookie name, not "any dsh-auth cookie".
// The name is `dsh-auth-<base64url(sha256(authority))>` and the authority is
// the very Host the tunnel rewrites to (its --local target, http host
// spelling). Two failure modes must both recover:
//   - cross-node switch: the WebView holds a cookie minted by the OTHER node.
//     Both tunnels speak loopback, but on DIFFERENT ports, so the names
//     differ — the stale cookie does not suppress injection, and the node's
//     token remints a fresh cookie (measured 2026-11-08).
//   - same-node expired cookie: the name matches, so injection is skipped —
//     but the stream handler detects the 401 and retries once with the token
//     (see retryGUITokenAfterUnauthorized), so recovery still happens.
//
// Always injecting instead loops forever: dsh's token branch runs before its
// cookie check, so a valid session would 303 → `./` → inject → 303 → ∞
// (ERR_TOO_MANY_REDIRECTS, measured).
func (t *Tunnel) injectGUIToken(method, path string, headers map[string][]string, tokenFile string) string {
	if tokenFile == "" || method != http.MethodGet {
		return path
	}
	base, query, _ := strings.Cut(path, "?")
	if base != "/" || strings.Contains(query, "token=") {
		return path
	}
	for _, v := range headers["Cookie"] {
		if strings.Contains(v, t.guiCookieName()+"=") {
			return path
		}
	}
	token := t.guiToken.get()
	if token == "" {
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			return path
		}
		token = strings.TrimSpace(string(data))
	}
	if token == "" {
		return path
	}
	if query == "" {
		return "/?token=" + url.QueryEscape(token)
	}
	return path + "&token=" + url.QueryEscape(token)
}

// guiCookieName derives the cookie name THIS tunnel's local dsh web expects:
// `dsh-auth-` + base64url(sha256(authority)). The authority is the Host value
// the tunnel rewrites requests to — its --local target, in http host spelling
// (`host:port`), matching dsh's requestAudience for non-TLS listeners.
// Cached per Tunnel: the --local target is fixed for the process lifetime.
func (t *Tunnel) guiCookieName() string {
	t.guiNameOnce.Do(func() {
		sum := sha256.Sum256([]byte(t.cfg.GUICookieAuthority))
		t.guiName = "dsh-auth-" + base64.RawURLEncoding.EncodeToString(sum[:])
	})
	return t.guiName
}

// normalizeHeaders rewrites Host to the loopback target and strips
// Origin / Sec-Fetch-Site so the request passes dsh's trust gate as a
// local user. Relay-level auth headers are stripped, and the relay's own
// cookie is dropped from Cookie — the GUI's auth cookie is forwarded, because
// dsh web binds it to the request authority and needs it on every request.
func normalizeHeaders(h map[string][]string, localAddr string) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, vs := range h {
		switch strings.ToLower(k) {
		case "host":
			continue // rewritten below
		case "origin", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest",
			"sec-fetch-user", "authorization", "proxy-authorization",
			"x-dsh-relay-token":
			continue // stripped
		case "cookie":
			if kept := stripRelayCookies(vs); kept != "" {
				out[k] = []string{kept}
			}
			continue
		}
		out[k] = append([]string(nil), vs...)
	}
	out["Host"] = []string{localAddr}
	return out
}

// stripRelayCookies removes the relay's cookie from Cookie header values and
// returns the remaining pairs ("" when nothing is left).
func stripRelayCookies(values []string) string {
	var kept []string
	for _, v := range values {
		for _, pair := range strings.Split(v, ";") {
			pair = strings.TrimSpace(pair)
			name, _, _ := strings.Cut(pair, "=")
			if pair == "" || name == relayAccessCookie {
				continue
			}
			kept = append(kept, pair)
		}
	}
	return strings.Join(kept, "; ")
}
