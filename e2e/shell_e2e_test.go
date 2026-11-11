package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dsh_mobile/internal/relay"
)

// startNodeDsh stands in for one node's dsh web: /hello answers with the node
// id, and / sets a per-node dsh-auth cookie with Path=/ (the real GUI's
// token-exchange shape) so cookie-scoping can be asserted end to end.
func startNodeDsh(t *testing.T, nodeID string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello from " + nodeID))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "dsh-auth-" + nodeID, Value: "secret-" + nodeID, Path: "/"})
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w, "<html><body>GUI of %s</body></html>", nodeID)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// startShellRelay is startRelay plus a DefaultNodeFile so the shell's POST
// switch has somewhere to persist.
func startShellRelay(t *testing.T, pairings map[string]string, accessToken string) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	defaultFile := filepath.Join(dir, "default-node")
	base, ws := startRelayWithDefault(t, pairings, accessToken, defaultFile)
	return base, ws, defaultFile
}

// startRelayWithDefault mirrors startRelay but wires DefaultNodeFile.
func startRelayWithDefault(t *testing.T, pairings map[string]string, accessToken, defaultFile string) (string, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	r := relay.New(relay.Config{
		ListenAddr:           addr,
		Pairings:             pairings,
		AccessToken:          accessToken,
		DefaultNodeFile:      defaultFile,
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

func getBody(t *testing.T, url string, header map[string]string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

// TestShellServedAtRootWithTwoNodes: root returns the shell page (not a node
// GUI) once two devices are paired; the page references the SSE watch and the
// per-node iframe URL scheme.
func TestShellServedAtRootWithTwoNodes(t *testing.T) {
	a := startNodeDsh(t, "node-a")
	b := startNodeDsh(t, "node-b")
	base, ws, _ := startShellRelay(t, map[string]string{"ta": "node-a", "tb": "node-b"}, "")
	startTunnel(t, ws, "node-a", "ta", strings.TrimPrefix(a.URL, "http://"))
	startTunnel(t, ws, "node-b", "tb", strings.TrimPrefix(b.URL, "http://"))

	status, body, hdr := getBody(t, base+"/", nil)
	if status != 200 {
		t.Fatalf("root status=%d body=%s", status, body[:min(200, len(body))])
	}
	if !strings.Contains(hdr.Get("Content-Type"), "text/html") {
		t.Fatalf("content-type=%q", hdr.Get("Content-Type"))
	}
	// 壳页只保留 iframe 舞台 + __dshShell 桥；节点切换 UI 全在插件里
	// （2026-11-10 用户定：红圈内的壳页自带下拉删除）。
	for _, marker := range []string{"default-node/watch", "/d/", "iframe", "__dshShell"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("shell page missing %q", marker)
		}
	}
	for _, removed := range []string{"aria-haspopup", `id="pick"`, `id="menu"`} {
		if strings.Contains(body, removed) {
			t.Fatalf("shell page still renders the removed built-in dropdown: %q", removed)
		}
	}
	if strings.Contains(body, "GUI of node-a") {
		t.Fatal("root served a node GUI instead of the shell")
	}
}

// TestShellSingleNodeFallsThrough: with one paired device the root keeps the
// legacy direct-GUI behaviour (no shell).
func TestShellSingleNodeFallsThrough(t *testing.T) {
	a := startNodeDsh(t, "solo")
	base, ws, _ := startShellRelay(t, map[string]string{"ta": "solo"}, "")
	startTunnel(t, ws, "solo", "ta", strings.TrimPrefix(a.URL, "http://"))

	status, body, _ := getBody(t, base+"/", nil)
	if status != 200 || !strings.Contains(body, "GUI of solo") {
		t.Fatalf("single-node root should proxy the GUI: status=%d body=%q", status, body)
	}
}

// TestShellAuthGate: a token-gated relay refuses the shell without credentials
// and serves it with the header token.
func TestShellAuthGate(t *testing.T) {
	a := startNodeDsh(t, "node-a")
	b := startNodeDsh(t, "node-b")
	base, ws, _ := startShellRelay(t, map[string]string{"ta": "node-a", "tb": "node-b"}, "sekrit")
	startTunnel(t, ws, "node-a", "ta", strings.TrimPrefix(a.URL, "http://"))
	startTunnel(t, ws, "node-b", "tb", strings.TrimPrefix(b.URL, "http://"))

	status, _, _ := getBody(t, base+"/", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated shell status=%d, want 401", status)
	}
	status, body, _ := getBody(t, base+"/", map[string]string{"X-DSH-Relay-Token": "sekrit"})
	if status != 200 || !strings.Contains(body, "iframe") {
		t.Fatalf("authenticated shell status=%d", status)
	}
}

// TestShellSwitchAndCookieScope: POST default-node persists + broadcasts; the
// node-prefixed GUI response gets its dsh-auth cookie scoped to /d/<id> so two
// iframes cannot clobber each other.
func TestShellSwitchAndCookieScope(t *testing.T) {
	a := startNodeDsh(t, "node-a")
	b := startNodeDsh(t, "node-b")
	base, ws, defaultFile := startShellRelay(t, map[string]string{"ta": "node-a", "tb": "node-b"}, "")
	startTunnel(t, ws, "node-a", "ta", strings.TrimPrefix(a.URL, "http://"))
	startTunnel(t, ws, "node-b", "tb", strings.TrimPrefix(b.URL, "http://"))

	// Switch to node-b via the same endpoint the shell page uses.
	resp, err := http.Post(base+"/__dsh_tunnel__/default-node", "application/json", strings.NewReader(`{"deviceId":"node-b"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("switch status=%d", resp.StatusCode)
	}
	persisted, err := os.ReadFile(defaultFile)
	if err != nil || strings.TrimSpace(string(persisted)) != "node-b" {
		t.Fatalf("default file=%q err=%v", persisted, err)
	}
	// GET reflects it.
	_, body, _ := getBody(t, base+"/__dsh_tunnel__/default-node", nil)
	var dn struct {
		DeviceID string `json:"deviceId"`
	}
	if err := json.Unmarshal([]byte(body), &dn); err != nil || dn.DeviceID != "node-b" {
		t.Fatalf("default GET=%q", body)
	}
	// Node-prefixed GUI: cookie must be scoped to the node path.
	status, _, hdr := getBody(t, base+"/d/node-a/", nil)
	if status != 200 {
		t.Fatalf("node-a GUI status=%d", status)
	}
	sc := hdr.Get("Set-Cookie")
	if !strings.Contains(sc, "dsh-auth-node-a") || !strings.Contains(sc, "Path=/d/node-a") {
		t.Fatalf("cookie not path-scoped: %q", sc)
	}
	// Both nodes' GUIs still route correctly under their prefixes.
	for _, id := range []string{"node-a", "node-b"} {
		st, bd, _ := getBody(t, base+"/d/"+id+"/hello", nil)
		if st != 200 || bd != "hello from "+id {
			t.Fatalf("prefix routing %s: status=%d body=%q", id, st, bd)
		}
	}
}

// TestShellWatchStreamsSwitch: the SSE watch uses the same endpoint the shell
// subscribes to; first frame is current, a POST pushes the new id promptly.
func TestShellWatchStreamsSwitch(t *testing.T) {
	a := startNodeDsh(t, "node-a")
	b := startNodeDsh(t, "node-b")
	base, ws, _ := startShellRelay(t, map[string]string{"ta": "node-a", "tb": "node-b"}, "")
	startTunnel(t, ws, "node-a", "ta", strings.TrimPrefix(a.URL, "http://"))
	startTunnel(t, ws, "node-b", "tb", strings.TrimPrefix(b.URL, "http://"))

	resp, err := http.Get(base + "/__dsh_tunnel__/default-node/watch")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("watch status=%d ct=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// Trigger a switch; read frames until node-b arrives (bounded).
	if _, err := http.Post(base+"/__dsh_tunnel__/default-node", "application/json", strings.NewReader(`{"deviceId":"node-b"}`)); err != nil {
		t.Fatal(err)
	}
	frames := make(chan string, 16)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				frames <- string(buf[:n])
			}
			if err != nil {
				close(frames)
				return
			}
		}
	}()
	deadline := time.After(5 * time.Second)
	got := ""
	for {
		select {
		case chunk, ok := <-frames:
			if !ok {
				t.Fatalf("watch stream ended before node-b: %q", got)
			}
			got += chunk
			if strings.Contains(got, "data: node-b") {
				return
			}
		case <-deadline:
			t.Fatalf("watch never streamed node-b: %q", got)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
