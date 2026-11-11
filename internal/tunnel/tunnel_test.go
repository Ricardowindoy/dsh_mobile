package tunnel

import (
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestNormalizeHeaders(t *testing.T) {
	in := map[string][]string{
		"Host":              {"relay.example.com"},
		"Origin":            {"https://relay.example.com"},
		"Sec-Fetch-Site":    {"cross-site"},
		"Sec-Fetch-Mode":    {"cors"},
		"Authorization":     {"Bearer secret"},
		"X-Dsh-Relay-Token": {"tok123"},
		"X-Custom":          {"keep"},
		"Accept":            {"text/html"},
	}
	out := normalizeHeaders(in, "127.0.0.1:3080")

	if got := out["Host"]; len(got) != 1 || got[0] != "127.0.0.1:3080" {
		t.Errorf("Host = %v, want [127.0.0.1:3080]", got)
	}
	for _, stripped := range []string{"Origin", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Authorization", "X-Dsh-Relay-Token"} {
		if _, ok := out[stripped]; ok {
			t.Errorf("header %q should be stripped", stripped)
		}
	}
	if got := out["X-Custom"]; len(got) != 1 || got[0] != "keep" {
		t.Errorf("X-Custom = %v, want [keep]", got)
	}
	if got := out["Accept"]; len(got) != 1 || got[0] != "text/html" {
		t.Errorf("Accept = %v, want [text/html]", got)
	}
}

func TestNormalizeHeadersEmpty(t *testing.T) {
	out := normalizeHeaders(nil, "127.0.0.1:3080")
	if got := out["Host"]; len(got) != 1 || got[0] != "127.0.0.1:3080" {
		t.Errorf("Host = %v, want [127.0.0.1:3080]", got)
	}
}

// 隧道要把 GUI 的鉴权 cookie 带过去（dsh web 把它绑在请求 authority 上，
// 每次请求都要校验），但**绝不能**把 relay 自己的 cookie 带进本地服务。
func TestNormalizeHeadersKeepsGUICookieDropsRelayCookie(t *testing.T) {
	in := map[string][]string{
		"Cookie": {"dsh_relay_token=secret; dsh_web_auth=abc123", "theme=dark"},
	}
	out := normalizeHeaders(in, "127.0.0.1:3082")
	got := out["Cookie"]
	if len(got) != 1 {
		t.Fatalf("Cookie = %v, want exactly one header value", got)
	}
	if strings.Contains(got[0], "dsh_relay_token") {
		t.Errorf("relay cookie must not reach the local server: %q", got[0])
	}
	for _, want := range []string{"dsh_web_auth=abc123", "theme=dark"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("Cookie %q should keep %q", got[0], want)
		}
	}
}

// 只有 relay 的 cookie 时，Cookie 头应整个消失（不要发出空头）。
func TestNormalizeHeadersDropsEmptyCookieHeader(t *testing.T) {
	out := normalizeHeaders(map[string][]string{"Cookie": {"dsh_relay_token=x"}}, "127.0.0.1:3082")
	if _, ok := out["Cookie"]; ok {
		t.Errorf("Cookie header should be dropped when nothing is left, got %v", out["Cookie"])
	}
}

// 注入规则：仅 GET /、仅一次、无既有 token、且调用方还没拿到 GUI cookie。
// 其它请求一律不动（负例同样重要）。
func TestInjectGUIToken(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	if err := os.WriteFile(file, []byte("LAUNCH-TOKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := &Tunnel{cfg: Config{GUICookieAuthority: "127.0.0.1:3082"}}
	empty := map[string][]string{}
	cases := []struct {
		name    string
		method  string
		path    string
		file    string
		headers map[string][]string
		want    string
	}{
		{name: "根路径注入", method: "GET", path: "/", file: file, headers: empty, want: "/?token=LAUNCH-TOKEN"},
		{name: "带查询串追加", method: "GET", path: "/?x=1", file: file, headers: empty, want: "/?x=1&token=LAUNCH-TOKEN"},
		{name: "已有 token 不重复注入", method: "GET", path: "/?token=other", file: file, headers: empty, want: "/?token=other"},
		{name: "非根路径不动", method: "GET", path: "/assets/index.js", file: file, headers: empty, want: "/assets/index.js"},
		{name: "非 GET 不动", method: "POST", path: "/", file: file, headers: empty, want: "/"},
		{name: "未配置文件不动", method: "GET", path: "/", file: "", headers: empty, want: "/"},
		{name: "文件缺失不动", method: "GET", path: "/", file: filepath.Join(dir, "missing"), headers: empty, want: "/"},
		// 多节点语义：异节点同名旧 cookie 不再挡注入（authority 相同→cookie 名相同，
		// 挡注入会让跨节点切换永远 401）。总是注入，token 对时 dsh 幂等重签。
		{name: "已带 GUI cookie 仍注入（跨节点旧 cookie 必须可恢复）", method: "GET", path: "/", file: file,
			headers: map[string][]string{"Cookie": {"dsh-auth-abc=v1.payload"}}, want: "/?token=LAUNCH-TOKEN"},
		{name: "只带 relay cookie 仍注入", method: "GET", path: "/", file: file,
			headers: map[string][]string{"Cookie": {"theme=dark"}}, want: "/?token=LAUNCH-TOKEN"},
		// 本节点自己的 cookie 名（sha256("127.0.0.1:3082") 的 base64url）：存在 → 不注入
		// （否则 303 循环）；异节点/失效的兜底由 stream 层 401 重试负责。
		{name: "本节点自己的 cookie 名不注入（防 303 循环）", method: "GET", path: "/", file: file,
			headers: map[string][]string{"Cookie": {"dsh-auth-" + ownCookieNameFor("127.0.0.1:3082") + "=v1.sig"}}, want: "/"},
	}
	for _, c := range cases {
		if got := tr.injectGUIToken(c.method, c.path, c.headers, c.file); got != c.want {
			t.Errorf("%s: injectGUIToken(%q,%q) = %q, want %q", c.name, c.method, c.path, got, c.want)
		}
	}
}

// 历史教训与新前提：Strict 时代持 cookie 再注入会 303 死循环（真机打不开），
// 该前提已被 relaxSameSite（Strict→Lax）消除——303 跟随请求会带上新 cookie，
// 200 收敛（2026-11-08 于 aliyun-ecs 真机验证：stale cookie + /?token → 303 → 200）。
// 多节点下 cookie 名跨节点同名，跳过注入会让切换后的首屏永远 401，因此本测试
// 现在钉住相反的契约：即便带着 GUI cookie 也必须注入，否则跨节点不可恢复。
func TestInjectGUITokenAvoidsRedirectLoop(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("LAUNCH-TOKEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := &Tunnel{cfg: Config{GUICookieAuthority: "127.0.0.1:3082"}}
	cookieHeaders := map[string][]string{"Cookie": {"a=1; dsh-auth-deadbeef=v1.sig; b=2"}}
	if got := tr.injectGUIToken("GET", "/", cookieHeaders, file); got != "/?token=LAUNCH-TOKEN" {
		t.Fatalf("带 GUI cookie 也必须注入（跨节点恢复依赖它，303 循环已由 relaxSameSite 消除），got %q", got)
	}
}

// 空 token 文件（dsh web 还没起来时）不得注入空参数。
func TestInjectGUITokenEmptyFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := &Tunnel{cfg: Config{GUICookieAuthority: "127.0.0.1:3082"}}
	if got := tr.injectGUIToken("GET", "/", nil, file); got != "/" {
		t.Errorf("empty token file should not inject, got %q", got)
	}
}

// 回程必须把 SameSite=Strict 降为 Lax，否则手机 WebView（从 tauri.localhost
// 跨站发起导航）不会回送鉴权 cookie，表现为 ERR_TOO_MANY_REDIRECTS。
func TestRelaxSameSite(t *testing.T) {
	h := map[string][]string{
		"Set-Cookie": {
			"dsh-auth-x=v1.a.b; Max-Age=1; Path=/; HttpOnly; SameSite=Strict",
			"other=1; SameSite=Lax",
			"plain=2",
		},
	}
	relaxSameSite(h)
	got := h["Set-Cookie"]
	if strings.Contains(got[0], "SameSite=Strict") || !strings.Contains(got[0], "SameSite=Lax") {
		t.Errorf("Strict 应改为 Lax，实际 %q", got[0])
	}
	if got[1] != "other=1; SameSite=Lax" {
		t.Errorf("已是 Lax 的不得改动，实际 %q", got[1])
	}
	if got[2] != "plain=2" {
		t.Errorf("无 SameSite 的不得改动，实际 %q", got[2])
	}
}

// 没有 Set-Cookie 头时不得凭空创建。
func TestRelaxSameSiteNoHeader(t *testing.T) {
	h := map[string][]string{"Content-Type": {"text/html"}}
	relaxSameSite(h)
	if _, ok := h["Set-Cookie"]; ok {
		t.Error("无 Set-Cookie 时不得创建该头")
	}
	if h["Content-Type"][0] != "text/html" {
		t.Error("其它响应头不得被改动")
	}
}

func TestRequestNeedsEOF(t *testing.T) {
	cases := []struct {
		method  string
		headers map[string][]string
		want    bool
	}{
		{"GET", nil, false},
		{"HEAD", nil, false},
		{"OPTIONS", nil, false},
		// 有 Content-Length：服务器按长度判定 body 结束，无需半关闭。
		{"POST", map[string][]string{"Content-Length": {"5"}}, false},
		// chunked：服务器按终止块判定，无需半关闭。
		{"POST", map[string][]string{"Transfer-Encoding": {"chunked"}}, false},
		// 无长度声明：只能靠 EOF 半关闭通知服务器。
		{"POST", nil, true},
		{"PUT", nil, true},
	}
	for _, c := range cases {
		if got := requestNeedsEOF(c.method, c.headers); got != c.want {
			t.Errorf("requestNeedsEOF(%q, %v) = %v, want %v", c.method, c.headers, got, c.want)
		}
	}
}

// RFC 8252 loopback OAuth 回调：App 把 http://127.0.0.1:<port>/callback 重写成
// relay 上的 /__dsh_loopback__/<port>/callback，由本机 tunnel 解析并拨对应端口。
// 这些用例钉死三条红线：① 普通路径绝不受影响；② 拨号目标恒为 127.0.0.1
// （手机只能选端口，永远不能指定主机）；③ 功能默认关，非法端口必须硬拒。
func TestResolveLoopback(t *testing.T) {
	const local = "127.0.0.1:3082"
	cases := []struct {
		name    string
		allow   bool
		path    string
		addr    string
		rewrite string
		ok      bool
	}{
		// 普通路径：无论开关如何，一律走 --local，路径原样。
		{"普通 GUI 路径-关", false, "/", local, "/", true},
		{"普通 GUI 路径-开", true, "/api/session/list", local, "/api/session/list", true},
		{"带查询串的普通路径", true, "/api/x?q=1&n=2", local, "/api/x?q=1&n=2", true},
		// 近似前缀但不属于 loopback 路由的路径，不能被误判。
		{"不含前缀的路径片段", true, "/assets/__dsh_loopback__/8080/x", local, "/assets/__dsh_loopback__/8080/x", true},

		// 开关关闭：即使端口合法也必须拒绝（不能静默落到 GUI 端口）。
		{"loopback-默认关", false, "/__dsh_loopback__/8976/oauth/callback?code=abc&state=s", "", "", false},
		// 开关打开：拨 127.0.0.1:8976，路径剥掉前缀还原，query 完整保留。
		{"loopback-开启并重写", true, "/__dsh_loopback__/8976/oauth/callback?code=abc&state=s",
			"127.0.0.1:8976", "/oauth/callback?code=abc&state=s", true},
		{"loopback-根回调", true, "/__dsh_loopback__/49152/", "127.0.0.1:49152", "/", true},
		{"loopback-无尾斜杠", true, "/__dsh_loopback__/8080", "127.0.0.1:8080", "/", true},

		// 非法 / 越界端口：必须拒，且绝不回落到 --local。
		{"端口非数字", true, "/__dsh_loopback__/abc/cb", "", "", false},
		{"端口 0", true, "/__dsh_loopback__/0/cb", "", "", false},
		{"端口越界 65536", true, "/__dsh_loopback__/65536/cb", "", "", false},
		{"端口负数", true, "/__dsh_loopback__/-1/cb", "", "", false},
		{"端口带路径穿越", true, "/__dsh_loopback__/8080/../admin", "127.0.0.1:8080", "/../admin", true},
		// 关键安全断言：手机无法把拨号目标改写成其它主机（无 host 字段可传）。
		{"无法指定主机", true, "/__dsh_loopback__/192.168.1.5/cb", "", "", false},
		{"无法注入冒号", true, "/__dsh_loopback__/8080:9999/cb", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr, rewrite, ok := resolveLoopback(Config{LocalAddr: local, AllowLoopbackPorts: c.allow}, c.path)
			if ok != c.ok || addr != c.addr || rewrite != c.rewrite {
				t.Errorf("resolveLoopback(allow=%v, %q) = (%q, %q, %v), want (%q, %q, %v)",
					c.allow, c.path, addr, rewrite, ok, c.addr, c.rewrite, c.ok)
			}
		})
	}
}

// resolveLoopback 的返回值必须恒为 loopback 地址：任何允许的用例都不允许
// 返回非 127.0.0.1 的目标。这是「手机只能选端口」这条红线的独立把关。
func TestResolveLoopbackNeverReturnsNonLoopback(t *testing.T) {
	for port := 1; port <= 1024; port++ {
		addr, _, ok := resolveLoopback(Config{LocalAddr: "127.0.0.1:3082", AllowLoopbackPorts: true},
			"/__dsh_loopback__/"+strconv.Itoa(port)+"/cb")
		if !ok {
			t.Fatalf("port %d should be allowed", port)
		}
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			t.Fatalf("port %d resolved to non-loopback addr %q", port, addr)
		}
	}
}

// ownCookieNameFor mirrors tunnel.guiCookieName for test assertions.
func ownCookieNameFor(authority string) string {
	sum := sha256.Sum256([]byte(authority))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
