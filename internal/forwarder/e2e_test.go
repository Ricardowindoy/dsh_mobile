package forwarder

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// freePort asks the kernel for an unused TCP port, then releases it. Tests
// bind real sockets (no mocks) so accept/close/error paths are exercised.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

func waitDial(t *testing.T, addr string, timeout time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("cannot dial %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startEcho starts a local TCP echo server (the "hindsight / gateway" stand-in).
func startEcho(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// newTestServer builds a Server with a trivial relay handler and N free ports.
func newTestServer(t *testing.T, token string, names []string) (*Server, ServerConfig, []int) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctlPort := freePort(t)
	ports := make([]PortSpec, 0, len(names))
	portNums := make([]int, 0, len(names))
	for _, n := range names {
		p := freePort(t)
		ports = append(ports, PortSpec{Name: n, RemotePort: p})
		portNums = append(portNums, p)
	}
	cfg := ServerConfig{
		RelayTLS: TLSConfig{Listen: "127.0.0.1:0", CertFile: "unused", KeyFile: "unused"},
		Relay:    RelayConfig{PairingsFile: "unused"},
		PortForward: PortForwardServerConfig{
			ControlListen:   net.JoinHostPort("127.0.0.1", itoa(ctlPort)),
			Token:           token,
			Ports:           ports,
			ConnectDeadline: Duration(2 * time.Second),
		},
	}
	// Certificates are not needed for the port-forward-only harness: call
	// startPortForwardOnly instead of the full start().
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("relay")) })
	return NewServer(cfg, handler, logger), cfg, portNums
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// startPortForwardOnly starts just the control/data plane (skips TLS listeners)
// so the tests can run without generating certs.
func (s *Server) startPortForwardOnly() error {
	for _, p := range s.cfg.PortForward.Ports {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(p.RemotePort)))
		if err != nil {
			return err
		}
		s.listens = append(s.listens, ln)
		s.wg.Add(1)
		go s.acceptData(ln, p.Name)
	}
	ctl, err := net.Listen("tcp", s.cfg.PortForward.ControlListen)
	if err != nil {
		return err
	}
	s.listens = append(s.listens, ctl)
	s.wg.Add(1)
	go s.acceptShared(ctl)
	s.wg.Add(1)
	go s.gcPending()
	return nil
}

func TestPortForwardEndToEnd(t *testing.T) {
	local, stopLocal := startEcho(t)
	defer stopLocal()

	srv, cfg, ports := newTestServer(t, "sekrit", []string{"echo"})
	if err := srv.startPortForwardOnly(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	clientCfg := ClientConfig{
		ServerAddr:  cfg.PortForward.ControlListen,
		Token:       "sekrit",
		ClientID:    "home-1",
		Forwards:    []ForwardSpec{{Name: "echo", LocalAddr: local}},
		DialTimeout: Duration(2 * time.Second),
		RetryMin:    Duration(100 * time.Millisecond),
		RetryMax:    Duration(500 * time.Millisecond),
	}
	client := NewClient(clientCfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()
	defer client.Close()

	// Wait for the control session to register, then send real bytes.
	pubAddr := net.JoinHostPort("127.0.0.1", itoa(ports[0]))
	var pub net.Conn
	deadline := time.Now().Add(3 * time.Second)
	for {
		srv.mu.Lock()
		registered := false
		for _, cs := range srv.sessions {
			if _, ok := cs.names["echo"]; ok {
				registered = true
			}
		}
		srv.mu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control session did not register")
		}
		time.Sleep(20 * time.Millisecond)
	}
	pub = waitDial(t, pubAddr, 3*time.Second)
	defer pub.Close()

	payload := []byte("hello through the self-written forwarder\n")
	if _, err := pub.Write(payload); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(payload))
	_ = pub.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(pub, buf); err != nil {
		t.Fatalf("echo read: %v", err)
	}
	if string(buf) != string(payload) {
		t.Errorf("echo = %q, want %q", buf, payload)
	}
}

func TestPortForwardRejectsBadToken(t *testing.T) {
	srv, cfg, _ := newTestServer(t, "right-token", []string{"echo"})
	if err := srv.startPortForwardOnly(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	conn, err := net.DialTimeout("tcp", cfg.PortForward.ControlListen, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := WriteFrame(conn, Hello("wrong-token", "home")); err != nil {
		t.Fatal(err)
	}
	f, err := ReadFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != TypeErr {
		t.Errorf("bad token should get err, got %q", f.Type)
	}
}

func TestPortForwardRejectsWithoutControlSession(t *testing.T) {
	srv, _, ports := newTestServer(t, "sekrit", []string{"echo"})
	if err := srv.startPortForwardOnly(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	// No client connected: a public connection must be closed promptly, never
	// silently held open.
	pub := waitDial(t, net.JoinHostPort("127.0.0.1", itoa(ports[0])), time.Second)
	defer pub.Close()
	_ = pub.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := pub.Read(make([]byte, 1))
	if err == nil {
		t.Error("connection without control session should be closed by server")
	}
}

func TestPortForwardUnknownRegisterName(t *testing.T) {
	srv, cfg, _ := newTestServer(t, "sekrit", []string{"echo"})
	if err := srv.startPortForwardOnly(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	conn, err := net.DialTimeout("tcp", cfg.PortForward.ControlListen, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = WriteFrame(conn, Hello("sekrit", "home"))
	f, _ := ReadFrame(conn)
	if f.Type != TypeOK {
		t.Fatalf("hello rejected: %+v", f)
	}
	if err := WriteFrame(conn, Register("not-in-config", "127.0.0.1:1")); err != nil {
		t.Fatal(err)
	}
	// The err frame travels on the control connection; read with a deadline.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := ReadFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != TypeErr || !strings.Contains(got.Reason, "unknown port name") {
		t.Errorf("expected unknown-port err, got %+v", got)
	}
}

func TestStripReferer(t *testing.T) {
	var seen string
	h := stripReferer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Referer")
	}), []string{"/api/bili-duty"})
	// httptest.NewRequest has no Referer; set one explicitly below.
	req := httptest.NewRequest(http.MethodPost, "/api/bili-duty", nil)
	req.Header.Set("Referer", "https://dsh.relay.example.com/")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "" {
		t.Errorf("Referer must be stripped for configured path, got %q", seen)
	}
	// Other paths keep it.
	seen = "sentinel"
	req2 := httptest.NewRequest(http.MethodGet, "/other", nil)
	req2.Header.Set("Referer", "https://example.com/")
	h.ServeHTTP(httptest.NewRecorder(), req2)
	if seen != "https://example.com/" {
		t.Errorf("Referer must be kept for other paths, got %q", seen)
	}
}

func TestBindDataRejectsUnknownID(t *testing.T) {
	srv, _, _ := newTestServer(t, "sekrit", []string{"echo"})
	if _, err := srv.BindData("never-issued"); err == nil {
		t.Error("unknown pairing id must be rejected")
	}
}
