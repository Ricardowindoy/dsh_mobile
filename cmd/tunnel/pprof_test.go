package main

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// pprof 门禁：显式开启才监听；拒绝非 loopback；端点真实可用；
// 端口占用不得影响主程序（只记日志）。
func TestPprofOptInLoopbackOnly(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 空地址 = 完全不监听
	startPprofServer("", quiet)

	// 非 loopback 拒绝：找一个本机非 loopback IP；没有就跳过该分支
	if ip := outboundIP(); ip != "" {
		ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
		if err == nil {
			addr := ln.Addr().String()
			_ = ln.Close()
			startPprofServer(addr, quiet) // 必须拒绝且不 panic
			c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
			if err == nil {
				_ = c.Close()
				t.Fatalf("pprof must refuse non-loopback bind %s", addr)
			}
		}
	}

	// loopback 显式开启：真实抓取 heap 与 goroutine 端点
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	startPprofServer(addr, quiet)
	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + addr + "/debug/pprof/heap?debug=1")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("pprof endpoint unreachable: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "heap profile") {
		t.Fatalf("heap endpoint: status=%d body-head=%q", resp.StatusCode, string(body[:min(80, len(body))]))
	}
	resp2, err := http.Get("http://" + addr + "/debug/pprof/goroutine?debug=1")
	if err != nil {
		t.Fatalf("goroutine endpoint: %v", err)
	}
	b2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != 200 || !strings.Contains(string(b2), "goroutine profile") {
		t.Fatalf("goroutine endpoint: status=%d", resp2.StatusCode)
	}

	// 端口被占：重复绑定同一地址必须安全返回（主程序不受影响）
	startPprofServer(addr, quiet)
}

func outboundIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
				return ipn.IP.String()
			}
		}
	}
	return ""
}
