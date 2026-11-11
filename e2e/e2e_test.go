// Package e2e exercises the full three-piece pipeline in one process:
// a fake dsh web server <- tunnel <- relay <- real HTTP client, matching
// the repository's REAL-composition testing strategy.
package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"dsh_mobile/internal/relay"
	"dsh_mobile/internal/tunnel"
)

// startFakeDsh runs a tiny stand-in for the dsh web server: one HTTP route,
// one POST echo route and a websocket echo endpoint, plus a hook capturing
// the headers it receives (for header-normalization assertions).
func startFakeDsh(t *testing.T) (*httptest.Server, *capture) {
	t.Helper()
	c := &capture{}
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		c.record(r)
		_, _ = w.Write([]byte("hello from dsh"))
	})
	mux.HandleFunc("/api/echo", func(w http.ResponseWriter, r *http.Request) {
		c.record(r)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c.record(r)
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(msgType, data); err != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, c
}

type capture struct {
	mu     sync.Mutex
	seen   []*http.Request
}

func (c *capture) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, r.Clone(context.Background()))
}

func (c *capture) first() *http.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) == 0 {
		return nil
	}
	return c.seen[0]
}

// startRelay launches the relay on an ephemeral port.
func startRelay(t *testing.T, pairings map[string]string, accessToken string) (baseURL string, wsURL string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // release the port; the relay re-listens on it below

	r := relay.New(relay.Config{
		ListenAddr:           addr,
		Pairings:             pairings,
		AccessToken:          accessToken,
		HeartbeatInterval:    time.Second,
		MaxStreamsPerDevice:  32,
		PerStreamBufferBytes: 256 * 1024,
		IdleTimeout:          30 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	base := "http://" + addr
	// Wait for the relay to accept connections.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/__dsh_tunnel__/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return base, "ws://" + addr
}

// startTunnel connects a tunnel to the relay pointing at the fake dsh.
func startTunnel(t *testing.T, relayWSURL, deviceID, token, localAddr string) *tunnel.Tunnel {
	t.Helper()
	tt := tunnel.New(tunnel.Config{
		RelayURL:             relayWSURL + relay.DevicePath,
		DeviceID:             deviceID,
		PairingToken:         token,
		LocalAddr:            localAddr,
		HeartbeatInterval:    time.Second,
		MaxStreams:           32,
		PerStreamBufferBytes: 256 * 1024,
		IdleTimeout:          30 * time.Second,
		ReconnectMin:         50 * time.Millisecond,
		ReconnectMax:         200 * time.Millisecond,
	})
	t.Cleanup(tt.Close)
	// Wait for pairing to complete.
	deadline := time.Now().Add(5 * time.Second)
	for !tt.Connected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !tt.Connected() {
		t.Fatal("tunnel did not pair with relay")
	}
	return tt
}

func TestHTTPRoundTrip(t *testing.T) {
	dsh, cap := startFakeDsh(t)
	relayHTTP, relayWS := startRelay(t, map[string]string{"pair-token": "dev-1"}, "")
	startTunnel(t, relayWS, "dev-1", "pair-token", strings.TrimPrefix(dsh.URL, "http://"))

	// Plain GET through the three-piece pipeline.
	resp, err := http.Get(relayHTTP + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if string(body) != "hello from dsh" {
		t.Fatalf("body = %q", body)
	}

	// POST body round trip.
	postResp, err := http.Post(relayHTTP+"/api/echo", "application/json", strings.NewReader(`{"k":"v"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer postResp.Body.Close()
	echoBody, _ := io.ReadAll(postResp.Body)
	if string(echoBody) != `{"k":"v"}` {
		t.Fatalf("echo body = %q", echoBody)
	}

	// Header normalization: the fake dsh must see Host=local and no
	// Origin/Sec-Fetch-Site.
	r0 := cap.first()
	if r0 == nil {
		t.Fatal("fake dsh saw no requests")
	}
	host := strings.TrimPrefix(dsh.URL, "http://")
	if r0.Host != host {
		t.Errorf("Host seen by dsh = %q, want %q", r0.Host, host)
	}
	for _, k := range []string{"Origin", "Sec-Fetch-Site"} {
		if _, ok := r0.Header[k]; ok {
			t.Errorf("header %q should have been stripped", k)
		}
	}
}

func TestWebSocketUpgradePassthrough(t *testing.T) {
	dsh, _ := startFakeDsh(t)
	relayHTTP, relayWS := startRelay(t, map[string]string{"pair-token": "dev-1"}, "")
	startTunnel(t, relayWS, "dev-1", "pair-token", strings.TrimPrefix(dsh.URL, "http://"))

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(relayHTTP, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	for i := 0; i < 5; i++ {
		msg := fmt.Sprintf("ping-%d", i)
		if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
			t.Fatal(err)
		}
		_, got, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != msg {
			t.Fatalf("echo = %q, want %q", got, msg)
		}
	}
}

func TestPairingRejected(t *testing.T) {
	_, relayWS := startRelay(t, map[string]string{"pair-token": "dev-1"}, "")

	// Wrong token: the relay must reject and the tunnel must not connect.
	bad := tunnel.New(tunnel.Config{
		RelayURL:      relayWS + relay.DevicePath,
		DeviceID:      "dev-1",
		PairingToken:  "wrong-token",
		LocalAddr:     "127.0.0.1:1",
		ReconnectMin:  100 * time.Millisecond,
		ReconnectMax:  200 * time.Millisecond,
	})
	defer bad.Close()
	time.Sleep(300 * time.Millisecond)
	if bad.Connected() {
		t.Fatal("tunnel with wrong token should not connect")
	}
}

func TestAccessTokenGate(t *testing.T) {
	dsh, _ := startFakeDsh(t)
	relayHTTP, relayWS := startRelay(t, map[string]string{"pair-token": "dev-1"}, "secret-token")
	startTunnel(t, relayWS, "dev-1", "pair-token", strings.TrimPrefix(dsh.URL, "http://"))

	// Without the token: 401.
	resp, err := http.Get(relayHTTP + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	// With the token: passes.
	req, _ := http.NewRequest(http.MethodGet, relayHTTP+"/hello", nil)
	req.Header.Set("X-DSH-Relay-Token", "secret-token")
	client := &http.Client{}
	okResp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer okResp.Body.Close()
	if okResp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", okResp.StatusCode)
	}
}

func TestDevicePathPrefixRouting(t *testing.T) {
	dsh, _ := startFakeDsh(t)
	relayHTTP, relayWS := startRelay(t, map[string]string{"pair-token": "dev-9"}, "")
	startTunnel(t, relayWS, "dev-9", "pair-token", strings.TrimPrefix(dsh.URL, "http://"))

	resp, err := http.Get(relayHTTP + "/d/dev-9/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "hello from dsh" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}
