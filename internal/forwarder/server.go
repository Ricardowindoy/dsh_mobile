package forwarder

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Server is the ECS-side unified forwarder. It owns every listener; a failure
// in one listener is reported but never kills the process.
type Server struct {
	cfg   ServerConfig
	log   *slog.Logger
	relay http.Handler

	mu       sync.Mutex
	sessions map[string]*controlSession // clientID -> active control session
	pending  map[string]*pendingConn    // server-issued id -> public conn awaiting data conn
	listens  []net.Listener
	servers  []*http.Server
	certs    []*CertSource
	wg       sync.WaitGroup
	closed   chan struct{}
	closeOne sync.Once
}

type controlSession struct {
	clientID string
	conn     net.Conn
	send     chan Frame        // bounded; a slow client cannot grow memory unbounded
	names    map[string]string // registered name -> localAddr (diagnostics)
	done     chan struct{}
	once     sync.Once
}

type pendingConn struct {
	id       string
	name     string
	pub      net.Conn
	created  time.Time
	deadline time.Time
}

// NewServer builds the server. relayHandler is the existing relay's Handler();
// keeping it injected means this package never imports internal/relay and the
// relay's own CLI/tests stay untouched.
func NewServer(cfg ServerConfig, relayHandler http.Handler, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg:      cfg,
		log:      log,
		relay:    relayHandler,
		sessions: make(map[string]*controlSession),
		pending:  make(map[string]*pendingConn),
		closed:   make(chan struct{}),
	}
}

// ListenAndServe starts all listeners and blocks until ctx is cancelled or a
// fatal startup error occurs. Startup is all-or-nothing: if any listener fails
// to bind, already-bound listeners are closed and the error returned.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if err := s.start(); err != nil {
		s.Close()
		return err
	}
	select {
	case <-ctx.Done():
		s.Close()
		return ctx.Err()
	case <-s.closed:
		return nil
	}
}

func (s *Server) start() error {
	// 1. Relay TLS entrance (private CA). Existing behaviour, hot-reloaded too.
	relayCert, err := NewCertSource(s.cfg.RelayTLS.CertFile, s.cfg.RelayTLS.KeyFile, s.cfg.PortForward.ReloadInterval.D(), s.log)
	if err != nil {
		return err
	}
	s.certs = append(s.certs, relayCert)
	handler := s.relayHandler()
	if err := s.serveHTTP(s.cfg.RelayTLS.Listen, handler, relayCert.TLSConfig()); err != nil {
		return fmt.Errorf("relayTLS: %w", err)
	}

	// 2. Public LE entrance (optional, for staged rollout).
	if s.cfg.PublicTLS != nil {
		pubCert, err := NewCertSource(s.cfg.PublicTLS.CertFile, s.cfg.PublicTLS.KeyFile, s.cfg.PortForward.ReloadInterval.D(), s.log)
		if err != nil {
			return err
		}
		s.certs = append(s.certs, pubCert)
		h := s.relayHandler()
		if len(s.cfg.PublicTLS.StripRefererPaths) > 0 {
			h = stripReferer(h, s.cfg.PublicTLS.StripRefererPaths)
		}
		if err := s.serveHTTP(s.cfg.PublicTLS.Listen, h, pubCert.TLSConfig()); err != nil {
			return fmt.Errorf("publicTLS: %w", err)
		}
	}

	// 3. Optional loopback plain-HTTP listener (gradual nginx retirement).
	if s.cfg.PlainHTTPListen != "" {
		if err := s.serveHTTP(s.cfg.PlainHTTPListen, s.relayHandler(), nil); err != nil {
			return fmt.Errorf("plainHTTP: %w", err)
		}
	}

	// 4. Port-forward data listeners (one per configured remote port).
	for _, p := range s.cfg.PortForward.Ports {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p.RemotePort))
		if err != nil {
			return fmt.Errorf("portForward listen %d (%s): %w", p.RemotePort, p.Name, err)
		}
		s.listens = append(s.listens, ln)
		s.log.Info("forwarder: port forward listening", "name", p.Name, "remotePort", p.RemotePort)
		s.wg.Add(1)
		go s.acceptData(ln, p.Name)
	}

	// 5. Port-forward control + data listener. Both share one public port:
	// the first frame distinguishes them (rathole-style). This keeps the
	// security-group footprint to a single port.
	ctl, err := net.Listen("tcp", s.cfg.PortForward.ControlListen)
	if err != nil {
		return fmt.Errorf("portForward listen %s: %w", s.cfg.PortForward.ControlListen, err)
	}
	s.listens = append(s.listens, ctl)
	s.log.Info("forwarder: control/data listening", "addr", ctl.Addr().String())
	s.wg.Add(1)
	go s.acceptShared(ctl)

	// 6. Pending-connection GC (bore-style TTL: a client that never opens the
	// data connection must not leak a public socket forever).
	s.wg.Add(1)
	go s.gcPending()
	return nil
}

// relayHandler applies the same handler to every HTTP entrance. It is a
// method (not a field) so a future per-entrance wrapper can be inserted.
func (s *Server) relayHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Preserve the browser-facing scheme for update download links.
		if r.Header.Get("X-Forwarded-Proto") == "" && r.TLS != nil {
			r.Header.Set("X-Forwarded-Proto", "https")
		}
		// Control-plane responses must never be cached (they carry pairing
		// codes and device facts) and must not be MIME-sniffed.
		if strings.HasPrefix(r.URL.Path, "/__dsh_tunnel__/") || strings.HasPrefix(r.URL.Path, "/d/") {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if strings.Contains(r.URL.Path, "/__dsh_tunnel__/") {
				w.Header().Set("Cache-Control", "no-store")
			}
		}
		s.relay.ServeHTTP(w, r)
	})
}

func (s *Server) serveHTTP(addr string, handler http.Handler, tlsCfg *tls.Config) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
	}
	if tlsCfg != nil {
		srv.TLSConfig = tlsCfg
		ln = tls.NewListener(ln, srv.TLSConfig)
	}
	s.listens = append(s.listens, ln)
	s.servers = append(s.servers, srv)
	s.log.Info("forwarder: HTTP listening", "addr", ln.Addr().String(), "tls", tlsCfg != nil)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("forwarder: HTTP serve error", "addr", addr, "err", err)
		}
	}()
	return nil
}

// stripReferer removes Referer for the configured paths. This replaces the
// nginx `Referer ""` rule for /api/bili-duty; without it the plugin's
// same-origin fallback rejects the loopback-rewritten request.
func stripReferer(next http.Handler, paths []string) http.Handler {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if set[r.URL.Path] {
			r.Header.Del("Referer")
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- control plane ----------

// acceptShared accepts both control and data connections on one port. The
// first frame decides: `hello` = control session, `data` = a data connection
// carrying a server-issued pairing id. Anything else is rejected.
func (s *Server) acceptShared(ln net.Listener) {
	defer s.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
				s.log.Warn("forwarder: shared accept error", "err", err)
				continue
			}
		}
		s.wg.Add(1)
		go s.classify(conn)
	}
}

func (s *Server) classify(conn net.Conn) {
	defer s.wg.Done()
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	f, err := ReadFrame(conn)
	if err != nil {
		s.log.Warn("forwarder: shared hello read failed", "remote", conn.RemoteAddr(), "err", err)
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	switch f.Type {
	case TypeHello:
		s.serveControl(conn, f)
	case TypeData:
		s.serveData(conn, f.ID)
	default:
		_ = conn.Close()
	}
}

// serveControl runs an authenticated control session. It is called after the
// shared listener has already consumed the `hello` frame.
func (s *Server) serveControl(conn net.Conn, hello Frame) {
	defer conn.Close()
	if subtle.ConstantTimeCompare([]byte(hello.Token), []byte(s.cfg.PortForward.Token)) != 1 {
		_ = WriteFrame(conn, Err("invalid token"))
		return
	}
	if hello.ClientID == "" {
		_ = WriteFrame(conn, Err("clientId required"))
		return
	}
	sess := &controlSession{
		clientID: hello.ClientID, conn: conn,
		send: make(chan Frame, 64), names: make(map[string]string), done: make(chan struct{}),
	}
	s.mu.Lock()
	if old := s.sessions[hello.ClientID]; old != nil {
		s.log.Warn("forwarder: superseding previous control session", "clientId", hello.ClientID)
		old.once.Do(func() { close(old.done) })
		_ = old.conn.Close()
	}
	s.sessions[hello.ClientID] = sess
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.sessions[hello.ClientID] == sess {
			delete(s.sessions, hello.ClientID)
		}
		s.mu.Unlock()
		s.dropSessionPending(sess)
		sess.once.Do(func() { close(sess.done) })
	}()

	if err := WriteFrame(conn, OK()); err != nil {
		return
	}
	s.wg.Add(2)
	go s.controlWriter(sess)
	go s.controlReader(sess)
	<-sess.done
}

// serveData joins a data connection (from the in-house client) with its
// already-accepted public connection. The id must have been issued by this
// server and be unexpired; otherwise the connection is closed cleanly.
func (s *Server) serveData(conn net.Conn, id string) {
	pub, err := s.BindData(id)
	if err != nil {
		s.log.Warn("forwarder: data conn rejected", "id", id, "err", err)
		_ = conn.Close()
		return
	}
	joinHalfClose(conn, pub)
}

// joinHalfClose copies two net.Conn directions with correct half-close
// semantics: EOF in one direction only CloseWrite()s the peer so the reverse
// direction can drain its tail. (frp's golib.Join is the known anti-pattern:
// it fully Closes both ends and loses trailing data.)
func joinHalfClose(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	copyDir := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go copyDir(a, b)
	go copyDir(b, a)
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

func (s *Server) controlWriter(sess *controlSession) {
	defer s.wg.Done()
	for {
		select {
		case <-sess.done:
			return
		case <-s.closed:
			return
		case f := <-sess.send:
			_ = sess.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := WriteFrame(sess.conn, f); err != nil {
				s.log.Warn("forwarder: control write failed", "clientId", sess.clientID, "err", err)
				sess.once.Do(func() { close(sess.done) })
				return
			}
		}
	}
}

func (s *Server) controlReader(sess *controlSession) {
	defer s.wg.Done()
	// Read-side liveness: ANY inbound frame counts as activity (smux's
	// sessionIsActive pattern). 3x heartbeat with no frame => dead.
	const idle = 90 * time.Second
	for {
		_ = sess.conn.SetReadDeadline(time.Now().Add(idle))
		f, err := ReadFrame(sess.conn)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.log.Warn("forwarder: control read failed", "clientId", sess.clientID, "err", err)
			}
			sess.once.Do(func() { close(sess.done) })
			return
		}
		switch f.Type {
		case TypePing:
			select {
			case sess.send <- Pong():
			default:
			}
		case TypePong:
			// liveness observed; nothing to do
		case TypeRegister:
			s.handleRegister(sess, f)
		default:
			_ = WriteFrame(sess.conn, Err("unexpected frame"))
		}
	}
}

func (s *Server) handleRegister(sess *controlSession, f Frame) {
	// The port list is server-owned. A client may only register a name that
	// exists in the config; it cannot choose or add public ports.
	var found *PortSpec
	for i := range s.cfg.PortForward.Ports {
		if s.cfg.PortForward.Ports[i].Name == f.Name {
			found = &s.cfg.PortForward.Ports[i]
			break
		}
	}
	if found == nil {
		_ = WriteFrame(sess.conn, Err("unknown port name "+f.Name))
		return
	}
	s.mu.Lock()
	sess.names[f.Name] = f.LocalAddr
	s.mu.Unlock()
	select {
	case sess.send <- Registered(f.Name, found.RemotePort):
	default:
	}
	s.log.Info("forwarder: port registered", "clientId", sess.clientID, "name", f.Name, "localAddr", f.LocalAddr, "remotePort", found.RemotePort)
}

// ---------- data plane ----------

func (s *Server) acceptData(ln net.Listener, name string) {
	defer s.wg.Done()
	for {
		pub, err := ln.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
				s.log.Warn("forwarder: data accept error", "name", name, "err", err)
				continue
			}
		}
		// A port with no live control session must fail closed, not silently
		// hold the connection open.
		s.mu.Lock()
		var sess *controlSession
		for _, cs := range s.sessions {
			if _, ok := cs.names[name]; ok {
				sess = cs
				break
			}
		}
		if sess == nil {
			s.mu.Unlock()
			s.log.Warn("forwarder: rejecting connection, no control session for port", "name", name, "remote", pub.RemoteAddr())
			_ = pub.Close()
			continue
		}
		id, err := newID()
		if err != nil {
			s.mu.Unlock()
			_ = pub.Close()
			continue
		}
		deadlineMs := int64(s.connectDeadline().Milliseconds())
		p := &pendingConn{id: id, name: name, pub: pub, created: time.Now(), deadline: time.Now().Add(s.connectDeadline())}
		s.pending[id] = p
		select {
		case sess.send <- Connect(id, name, deadlineMs):
			s.mu.Unlock()
		default:
			delete(s.pending, id)
			s.mu.Unlock()
			s.log.Warn("forwarder: control send queue full, rejecting", "name", name)
			_ = pub.Close()
			continue
		}
	}
}

func (s *Server) connectDeadline() time.Duration {
	if d := s.cfg.PortForward.ConnectDeadline.D(); d > 0 {
		return d
	}
	return 10 * time.Second
}

// BindData pairs a newly opened data connection with its pending public conn.
// Returns the public conn; the caller then joins the two with half-close.
func (s *Server) BindData(id string) (net.Conn, error) {
	s.mu.Lock()
	p := s.pending[id]
	if p != nil {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if p == nil {
		return nil, fmt.Errorf("unknown or expired connection id %q", id)
	}
	if time.Now().After(p.deadline) {
		_ = p.pub.Close()
		return nil, fmt.Errorf("connection id %q expired", id)
	}
	return p.pub, nil
}

func (s *Server) gcPending() {
	defer s.wg.Done()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
			now := time.Now()
			s.mu.Lock()
			for id, p := range s.pending {
				if now.After(p.deadline) {
					delete(s.pending, id)
					_ = p.pub.Close()
					s.log.Warn("forwarder: GC stale pending connection", "id", id, "name", p.name)
				}
			}
			s.mu.Unlock()
		}
	}
}

func (s *Server) dropSessionPending(sess *controlSession) {
	// Pending conns are keyed by id, not session, but closing the control
	// session should not leave public sockets hanging until the GC tick.
	s.mu.Lock()
	for id, p := range s.pending {
		_ = p.pub.Close()
		delete(s.pending, id)
	}
	s.mu.Unlock()
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Close stops every listener, shuts down HTTP servers, drops pending public
// connections and waits (with a bound) for goroutines to drain.
func (s *Server) Close() {
	s.closeOne.Do(func() {
		close(s.closed)
		for _, srv := range s.servers {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = srv.Shutdown(ctx)
			cancel()
		}
		for _, ln := range s.listens {
			_ = ln.Close()
		}
		s.mu.Lock()
		for _, cs := range s.sessions {
			cs.once.Do(func() { close(cs.done) })
			_ = cs.conn.Close()
		}
		for id, p := range s.pending {
			_ = p.pub.Close()
			delete(s.pending, id)
		}
		s.mu.Unlock()
		for _, c := range s.certs {
			c.Close()
		}
	})
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		s.log.Warn("forwarder: shutdown timed out waiting for connections to drain")
	}
}
