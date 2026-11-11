package tunnel

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

	"dsh_mobile/internal/relay"
)

// 端到端：App 把 loopback OAuth 回调重写成 /__dsh_loopback__/<port>/... 发到
// relay，relay 透传给设备，本机 tunnel 解析前缀后拨 127.0.0.1:<port> 并剥前缀。
// 断言三件可观测事实：① 回调服务确实收到请求；② 路径已还原（不含 relay 前缀）；
// ③ code/state query 一字不差。用真实 TCP socket + 真实 wss 配对，不 mock。
func TestLoopbackCallbackEndToEnd(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 1) 本机「OAuth 回调服务」：模拟 dsh web 上注册的 /oauth/callback 路由。
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "PATH="+r.URL.Path+"|QUERY="+r.URL.RawQuery+"|HOST="+r.Host)
	}))
	defer callback.Close()
	port := callback.Listener.Addr().(*net.TCPAddr).Port

	// 2) relay（无 access token，App 侧鉴权不在此测试范围）。
	r := relay.New(relay.Config{Pairings: map[string]string{"pair-token": "test-device"}, Logger: quiet})
	relaySrv := httptest.NewServer(r.Handler())
	defer relaySrv.Close()
	defer r.CloseDevices()
	wsURL := "ws" + strings.TrimPrefix(relaySrv.URL, "http") + "/__dsh_tunnel__/device"

	// 3) 本机 tunnel：--local 指向一个**故意不可用**的地址，用来证明 loopback
	// 回调不是靠 GUI 端口接住的，而是真的拨了回调服务那个端口。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guiPort := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	tun := New(Config{
		RelayURL:           wsURL,
		DeviceID:           "test-device",
		PairingToken:       "pair-token",
		LocalAddr:          net.JoinHostPort("127.0.0.1", itoa(guiPort)),
		AllowLoopbackPorts: true,
		Logger:             quiet,
	})
	defer tun.Close()

	// 等配对完成（成功判据是真实状态，不是「我调用了它」）。
	deadline := time.Now().Add(5 * time.Second)
	for !tun.Connected() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !tun.Connected() {
		t.Fatal("tunnel did not pair with relay")
	}

	// 4) 模拟 App 重写后的请求。
	url := relaySrv.URL + "/__dsh_loopback__/" + itoa(port) +
		"/oauth/callback?code=a%2Bb&state=s%3D1"
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("loopback callback request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%q", resp.StatusCode, body)
	}
	got := string(body)
	for _, want := range []string{
		"PATH=/oauth/callback",
		"QUERY=code=a%2Bb&state=s%3D1",
		"HOST=127.0.0.1:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("callback saw %q, missing %q", got, want)
		}
	}
}

// 反例门禁：AllowLoopbackPorts=false 时，loopback 回调必须被拒（403），
// 绝不能悄悄落到 GUI 端口把回调当普通路由处理。
func TestLoopbackCallbackRejectedWhenDisabled(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "SHOULD-NOT-BE-REACHED")
	}))
	defer callback.Close()
	port := callback.Listener.Addr().(*net.TCPAddr).Port

	r := relay.New(relay.Config{Pairings: map[string]string{"pair-token": "test-device"}, Logger: quiet})
	relaySrv := httptest.NewServer(r.Handler())
	defer relaySrv.Close()
	defer r.CloseDevices()
	wsURL := "ws" + strings.TrimPrefix(relaySrv.URL, "http") + "/__dsh_tunnel__/device"

	tun := New(Config{
		RelayURL:     wsURL,
		DeviceID:     "test-device",
		PairingToken: "pair-token",
		LocalAddr:    "127.0.0.1:1", // 一定连不上
		// AllowLoopbackPorts 故意不设置（默认 false）
		Logger: quiet,
	})
	defer tun.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !tun.Connected() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !tun.Connected() {
		t.Fatal("tunnel did not pair with relay")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		relaySrv.URL+"/__dsh_loopback__/"+itoa(port)+"/oauth/callback?code=x", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK || strings.Contains(string(body), "SHOULD-NOT-BE-REACHED") {
		t.Fatalf("disabled loopback must not reach callback: status=%d body=%q", resp.StatusCode, body)
	}
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
