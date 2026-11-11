package relay

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuthorize(t *testing.T) {
	r := &Relay{cfg: Config{AccessToken: "sekrit"}}

	// No token.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if r.authorize(req) {
		t.Error("request without token should be denied")
	}
	// Header token.
	req.Header.Set("X-DSH-Relay-Token", "sekrit")
	if !r.authorize(req) {
		t.Error("request with correct header token should pass")
	}
	// Bearer token.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Authorization", "Bearer sekrit")
	if !r.authorize(req2) {
		t.Error("request with correct bearer token should pass")
	}
	// Wrong token.
	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	req3.Header.Set("X-DSH-Relay-Token", "wrong")
	if r.authorize(req3) {
		t.Error("request with wrong token should be denied")
	}

	// Gate off: everything passes.
	open := &Relay{cfg: Config{AccessToken: ""}}
	if !open.authorize(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Error("open relay should accept unauthenticated requests")
	}
}

func TestIsHopByHop(t *testing.T) {
	hop := []string{"Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade", "TE", "Trailer", "Proxy-Authorization", "Proxy-Authenticate"}
	for _, k := range hop {
		if !isHopByHop(k) {
			t.Errorf("%q should be hop-by-hop", k)
		}
	}
	for _, k := range []string{"Content-Type", "Content-Length", "Sec-WebSocket-Accept", "Set-Cookie"} {
		if isHopByHop(k) {
			t.Errorf("%q should not be hop-by-hop", k)
		}
	}
}

func TestPickDevicePrefix(t *testing.T) {
	r := &Relay{devices: make(map[string]*device)}
	d := &device{id: "dev-1"}
	r.devices["dev-1"] = d

	// Explicit prefix.
	req := httptest.NewRequest(http.MethodGet, "http://relay/d/dev-1/api/x?q=1", nil)
	got, path := r.pickDevice(req)
	if got != d || path != "/api/x" {
		t.Errorf("got %v path %q", got, path)
	}
	// Prefix without trailing path.
	req2 := httptest.NewRequest(http.MethodGet, "http://relay/d/dev-1", nil)
	_, path2 := r.pickDevice(req2)
	if path2 != "/" {
		t.Errorf("path = %q, want /", path2)
	}
	// Unknown device.
	req3 := httptest.NewRequest(http.MethodGet, "http://relay/d/nope/x", nil)
	if got, _ := r.pickDevice(req3); got != nil {
		t.Error("unknown device should return nil")
	}
}

func TestPickDeviceSingle(t *testing.T) {
	r := &Relay{devices: make(map[string]*device)}
	d := &device{id: "solo"}
	r.devices["solo"] = d
	req := httptest.NewRequest(http.MethodGet, "http://relay/some/path", nil)
	got, path := r.pickDevice(req)
	if got != d || path != "/some/path" {
		t.Errorf("got %v path %q", got, path)
	}

	// Two devices without prefix: ambiguous, refuse.
	r.devices["second"] = &device{id: "second"}
	if got, _ := r.pickDevice(req); got != nil {
		t.Error("ambiguous route should return nil")
	}
}

func TestStaticCacheControl(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/assets/vendor-Cjbwl5VI.js", "public, max-age=31536000, immutable"},
		{"/assets/index-DwcjgW8G.css", "public, max-age=31536000, immutable"},
		{"/plugins/@deepseek-ai/dsh-client-runtime/client.js", ""},
		{"/plugins/foo/style.css", ""},
		{"/", ""},
		{"/api/session.history", ""},
		{"/api/events.mux", ""},
		{"/plugins/events", ""},
		{"/favicon.svg", ""},
	}
	for _, c := range cases {
		if got := staticCacheControl(c.path); got != c.want {
			t.Errorf("staticCacheControl(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// The aggregate probe answer is public, but the per-device list (ids + online)
// is only returned to an authorized caller: device ids are routing keys.
func TestProbeDeviceListRequiresToken(t *testing.T) {
	r := &Relay{
		cfg:     Config{AccessToken: "sekrit"},
		devices: map[string]*device{"home-dsh": {id: "home-dsh"}},
	}

	read := func(req *http.Request) map[string]any {
		w := httptest.NewRecorder()
		r.handleProbe(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (body %q)", err, w.Body.String())
		}
		return body
	}

	anon := read(httptest.NewRequest(http.MethodGet, "/__dsh_tunnel__/probe", nil))
	if _, ok := anon["devices"]; ok {
		t.Error("anonymous probe must not expose the device list")
	}
	if anon["deviceCount"].(float64) != 0 {
		t.Errorf("deviceCount = %v, want 0 (no live mux)", anon["deviceCount"])
	}

	req := httptest.NewRequest(http.MethodGet, "/__dsh_tunnel__/probe", nil)
	req.Header.Set("X-DSH-Relay-Token", "sekrit")
	auth := read(req)
	devices, ok := auth["devices"].([]any)
	if !ok || len(devices) != 1 {
		t.Fatalf("authorized probe devices = %v", auth["devices"])
	}
	entry := devices[0].(map[string]any)
	if entry["id"] != "home-dsh" || entry["online"] != false {
		t.Errorf("device entry = %v, want id=home-dsh online=false", entry)
	}

	// Wrong token still gets the aggregate answer, never the list.
	req2 := httptest.NewRequest(http.MethodGet, "/__dsh_tunnel__/probe?token=wrong", nil)
	if _, ok := read(req2)["devices"]; ok {
		t.Error("wrong token must not expose the device list")
	}
}

// App 把 RFC 8252 loopback 回调重写成 /__dsh_loopback__/<port>/... 后发到
// relay。relay 是字节管道：这条路径必须落到 handlePhone 并原样透传给设备，
// 不能被任何已注册的控制面端点吞掉、也不能被当成 404。
// 判据：无配对设备时 handlePhone 返回 503 "no device paired"（若被别的
// handler 处理会返回 404/200，测试即红）。
func TestLoopbackPathReachesPhoneHandler(t *testing.T) {
	r := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	for _, path := range []string{
		"/__dsh_loopback__/8976/oauth/callback?code=abc&state=s",
		"/__dsh_loopback__/49152/",
		"/__dsh_loopback__/8080/cb#frag",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "no device paired") {
			t.Errorf("GET %s = %d %q, want 503 no device paired (path must reach handlePhone)", path, resp.StatusCode, body)
		}
	}
}

// App 的 Relay 地址可以指向单个节点（/d/<deviceId>/）。此形态下的
// /d/<id>/__dsh_tunnel__/* 是 relay 自己的控制面（whoami/auth-check/login/
// probe/app-log/update），必须剥掉 /d/<id> 前缀交给注册端点处理，绝不能落进
// handlePhone 转发给设备（设备上没有这些端点 → 404；未带 cookie → 401）。
// 判据：auth-check 在节点前缀下应答 200/401（来自 relay 的 handleAuthCheck，
// 而不是 handlePhone 的 503 "no device paired" 或透传 404）；
// login 的 302 Location 必须保留 /d/<id> 前缀，让 WebView 落回节点 GUI。
func TestNodePrefixedTunnelEndpoints(t *testing.T) {
	r := New(Config{
		AccessToken: "sekrit",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// 预置一台"已配对但未连接"的设备（mux=nil，probe 报 offline）。
	r.devices["home-dsh"] = &device{id: "home-dsh"}
	h := r.Handler()

	// 节点前缀 + 正确 token → relay 的 auth-check 200（若落到 handlePhone
	// 或透传到设备，测试即红）。
	req := httptest.NewRequest(http.MethodGet, "/d/home-dsh/__dsh_tunnel__/auth-check?token=sekrit", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("node-prefixed auth-check status = %d body %q, want 200", w.Code, w.Body.String())
	}

	// 错误 token → 401 来自 auth-check（不是 503 no device paired）。
	req2 := httptest.NewRequest(http.MethodGet, "/d/home-dsh/__dsh_tunnel__/auth-check?token=wrong", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("node-prefixed auth-check wrong token = %d, want 401", w2.Code)
	}
	if got := strings.TrimSpace(w2.Body.String()); got != `{"valid":false}` {
		t.Fatalf("body = %q, want relay auth-check body", got)
	}

	// login 成功后的 302 Location 保留节点前缀。
	req3 := httptest.NewRequest(http.MethodGet, "/d/home-dsh/__dsh_tunnel__/login?token=sekrit&next=%2F", nil)
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, req3)
	if w3.Code != http.StatusFound {
		t.Fatalf("node-prefixed login status = %d, want 302", w3.Code)
	}
	loc := w3.Header().Get("Location")
	if loc != "/d/home-dsh/" {
		t.Fatalf("login Location = %q, want /d/home-dsh/", loc)
	}

	// whoami 同样可达（App 连接前预检依赖它）。
	req4 := httptest.NewRequest(http.MethodGet, "/d/aliyun-ecs/__dsh_tunnel__/whoami", nil)
	w4 := httptest.NewRecorder()
	h.ServeHTTP(w4, req4)
	if w4.Code != http.StatusOK {
		t.Fatalf("node-prefixed whoami status = %d, want 200", w4.Code)
	}

	// 非 __dsh_tunnel__ 的 /d/ 路径不受影响，仍走 handlePhone（先鉴权）。
	// 无 cookie → 401 unauthorized（handlePhone 的既有语义，未被 rewrite 吞掉
	// 也没有 503/404，证明它没被当成控制面端点）。
	req5 := httptest.NewRequest(http.MethodGet, "/d/no-such-node/api/remote.mux", nil)
	w5 := httptest.NewRecorder()
	h.ServeHTTP(w5, req5)
	if w5.Code != http.StatusUnauthorized || w5.Body.String() != "unauthorized\n" {
		t.Fatalf("non-tunnel node path = %d %q, want 401 unauthorized from handlePhone", w5.Code, w5.Body.String())
	}
	// 带合法 token 的 GUI 路径：设备 map 里没有 no-such-node → 503 no device
	// paired（pickDevice 显式路由未命中）。
	req6 := httptest.NewRequest(http.MethodGet, "/d/no-such-node/api/remote.mux", nil)
	req6.Header.Set("X-DSH-Relay-Token", "sekrit")
	w6 := httptest.NewRecorder()
	h.ServeHTTP(w6, req6)
	if w6.Code != http.StatusServiceUnavailable || w6.Body.String() != "no device paired\n" {
		t.Fatalf("non-tunnel node path with token = %d %q, want 503 no device paired", w6.Code, w6.Body.String())
	}
}

// Default-node routing: with several paired devices, the root path routes to
// the device named by DefaultNodeFile; explicit /d/<id>/ prefixes always win;
// an empty/unknown default falls back to the single-device heuristic (503 here).
// 判据（变异检验钉住）：删掉 pickDevice 的 default-node 回退后本测试必须红。
func TestDefaultNodeRouting(t *testing.T) {
	dir := t.TempDir()
	nodeFile := filepath.Join(dir, "default-node")

	r := New(Config{
		AccessToken:     "sekrit",
		DefaultNodeFile: nodeFile,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r.devices["aliyun-ecs"] = &device{id: "aliyun-ecs"}
	r.devices["home-dsh"] = &device{id: "home-dsh"}
	h := r.Handler()

	// 无默认文件：两台设备 → 503（handlePhone 鉴权后 pickDevice 返回 nil）。
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.Header.Set("X-DSH-Relay-Token", "sekrit")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no default + two devices = %d, want 503", w.Code)
	}

	// 设默认节点：根路径路由到该设备（无 /d/ 前缀，path 原样透传）。
	if err := os.WriteFile(nodeFile, []byte("aliyun-ecs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/api/x", nil))
	// handlePhone 会因设备 mux 为 nil 在转发处空指针吗——不会：先 authorize
	// 再 pickDevice；pickDevice 命中默认设备后进入转发。这里用裸 device 无
	// mux 会 panic，所以改为断言 pickDevice 的选择而不打完整 handler。
	_ = w2
	got, gotPath := r.pickDevice(func() *http.Request {
		req, _ := http.NewRequest(http.MethodGet, "/api/x", nil)
		return req
	}())
	if got == nil || got.id != "aliyun-ecs" || gotPath != "/api/x" {
		t.Fatalf("pickDevice with default = %v path %q, want aliyun-ecs /api/x", got, gotPath)
	}

	// 显式前缀优先于默认节点。
	got2, path2 := r.pickDevice(func() *http.Request {
		req, _ := http.NewRequest(http.MethodGet, "/d/home-dsh/api/y", nil)
		return req
	}())
	if got2 == nil || got2.id != "home-dsh" || path2 != "/api/y" {
		t.Fatalf("explicit prefix = %v path %q, want home-dsh /api/y", got2, path2)
	}

	// 默认节点文件指向未接入设备 → 回退 503（不瞎猜）。
	if err := os.WriteFile(nodeFile, []byte("ghost"), 0o600); err != nil {
		t.Fatal(err)
	}
	req3 := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req3.Header.Set("X-DSH-Relay-Token", "sekrit")
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, req3)
	if w3.Code != http.StatusServiceUnavailable {
		t.Fatalf("unknown default device = %d, want 503", w3.Code)
	}

	// GET default-node 端点：带 token 返回当前默认；不带 → 401。
	w4 := httptest.NewRecorder()
	h.ServeHTTP(w4, httptest.NewRequest(http.MethodGet, "/__dsh_tunnel__/default-node", nil))
	if w4.Code != http.StatusUnauthorized {
		t.Fatalf("default-node GET without token = %d, want 401", w4.Code)
	}
	// ghost 未接入：端点按诚实语义回空串（不把未接入的 id 冒充默认）。
	req5 := httptest.NewRequest(http.MethodGet, "/__dsh_tunnel__/default-node", nil)
	req5.Header.Set("X-DSH-Relay-Token", "sekrit")
	w5 := httptest.NewRecorder()
	h.ServeHTTP(w5, req5)
	var body struct {
		DeviceID string `json:"deviceId"`
	}
	if err := json.Unmarshal(w5.Body.Bytes(), &body); err != nil || body.DeviceID != "" {
		t.Fatalf("default-node GET unpaired = %q (err %v), want empty", w5.Body.String(), err)
	}

	// POST 换默认节点：paired id → 200 并落盘；未接入 id → 400。
	req6 := httptest.NewRequest(http.MethodPost, "/__dsh_tunnel__/default-node",
		strings.NewReader(`{"deviceId":"home-dsh"}`))
	req6.Header.Set("X-DSH-Relay-Token", "sekrit")
	w6 := httptest.NewRecorder()
	h.ServeHTTP(w6, req6)
	if w6.Code != http.StatusOK {
		t.Fatalf("default-node POST = %d %q, want 200", w6.Code, w6.Body.String())
	}
	if b, _ := os.ReadFile(nodeFile); string(b) != "home-dsh\n" {
		t.Fatalf("default-node file = %q, want home-dsh", b)
	}
	// POST 后 GET 必须回显已接入的新默认（闭环验证持久化+回读）。
	req8 := httptest.NewRequest(http.MethodGet, "/__dsh_tunnel__/default-node", nil)
	req8.Header.Set("X-DSH-Relay-Token", "sekrit")
	w8 := httptest.NewRecorder()
	h.ServeHTTP(w8, req8)
	var body2 struct {
		DeviceID string `json:"deviceId"`
	}
	if err := json.Unmarshal(w8.Body.Bytes(), &body2); err != nil || body2.DeviceID != "home-dsh" {
		t.Fatalf("default-node GET after POST = %q (err %v), want home-dsh", w8.Body.String(), err)
	}

	// POST 未接入 id → 400。
	req7 := httptest.NewRequest(http.MethodPost, "/__dsh_tunnel__/default-node",
		strings.NewReader(`{"deviceId":"nope"}`))
	req7.Header.Set("X-DSH-Relay-Token", "sekrit")
	w7 := httptest.NewRecorder()
	h.ServeHTTP(w7, req7)
	if w7.Code != http.StatusBadRequest {
		t.Fatalf("default-node POST unknown id = %d, want 400", w7.Code)
	}
}

// SSE watch 守门：订阅即收当前值首帧；POST 切换后事件即时推送；无 token 401。
// 变异检验钉住：删掉 broadcastDefaultNode 调用 → 第二帧永远不到 → 测试红。
func TestDefaultNodeWatchSSE(t *testing.T) {
	dir := t.TempDir()
	nodeFile := filepath.Join(dir, "default-node")
	if err := os.WriteFile(nodeFile, []byte("home-dsh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(Config{
		AccessToken:     "sekrit",
		DefaultNodeFile: nodeFile,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r.devices["home-dsh"] = &device{id: "home-dsh"}
	r.devices["aliyun-ecs"] = &device{id: "aliyun-ecs"}

	// 无 token → 401
	w0 := httptest.NewRecorder()
	r.Handler().ServeHTTP(w0, httptest.NewRequest(http.MethodGet, "/__dsh_tunnel__/default-node/watch", nil))
	if w0.Code != http.StatusUnauthorized {
		t.Fatalf("watch without token = %d, want 401", w0.Code)
	}

	// 订阅（带 token）：用真实 server 才能流式读
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/__dsh_tunnel__/default-node/watch", nil)
	req.Header.Set("X-DSH-Relay-Token", "sekrit")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch status = %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	readFrame := func(timeout time.Duration) string {
		type result struct {
			line string
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					ch <- result{"", err}
					return
				}
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "data: ") {
					ch <- result{strings.TrimPrefix(line, "data: "), nil}
					return
				}
			}
		}()
		select {
		case res := <-ch:
			if res.err != nil {
				t.Fatalf("read frame: %v", res.err)
			}
			return res.line
		case <-time.After(timeout):
			t.Fatal("timed out waiting for SSE frame (broadcast missing?)")
			return ""
		}
	}
	if got := readFrame(3 * time.Second); got != "home-dsh" {
		t.Fatalf("first frame = %q, want home-dsh", got)
	}
	// POST 切换 → 即时推送
	preq, _ := http.NewRequest(http.MethodPost, srv.URL+"/__dsh_tunnel__/default-node", strings.NewReader(`{"deviceId":"aliyun-ecs"}`))
	preq.Header.Set("X-DSH-Relay-Token", "sekrit")
	presp, err := srv.Client().Do(preq)
	if err != nil {
		t.Fatal(err)
	}
	presp.Body.Close()
	if presp.StatusCode != http.StatusOK {
		t.Fatalf("POST = %d", presp.StatusCode)
	}
	if got := readFrame(3 * time.Second); got != "aliyun-ecs" {
		t.Fatalf("push frame = %q, want aliyun-ecs", got)
	}
}
