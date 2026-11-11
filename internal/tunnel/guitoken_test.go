package tunnel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scanTokenCandidates 守门：43 位 base64url 且两侧非 token 字节才命中；
// 42/44 位、含非法字符、被更长 token 串包裹的都不命中。
func TestScanTokenCandidates(t *testing.T) {
	tok := strings.Repeat("aB3_-", 8) + "xyz" // 43 chars
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"boundary spaces", " " + tok + " ", []string{tok}},
		{"boundary nul", "\x00" + tok + "\x00", []string{tok}},
		{"too short", " " + tok[:42] + " ", nil},
		{"too long", " " + tok + "Q ", nil},
		{"embedded in longer run", "x" + tok, nil},
		{"illegal char inside", " " + tok[:20] + "." + tok[21:] + " ", nil},
		{"dedup", " " + tok + " " + tok + " ", []string{tok}},
	}
	for _, c := range cases {
		got := scanTokenCandidates([]byte(c.in))
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: got[%d]=%q want %q", c.name, i, got[i], c.want[i])
			}
		}
	}
}

// probeGUIToken 守门：303（旧版）或 200+Set-Cookie dsh-auth-（0.2.2+）才算
// 有效；裸 200（0.2.2 对错误 token 也回 200）/401/不可达都无效。
func TestProbeGUIToken(t *testing.T) {
	tok := strings.Repeat("k", 43)
	garbage := "--force-node-api-uncaught-exceptions-policy" // 43 chars, real-world heap garbage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("token") {
		case tok:
			// DSH 0.2.2+: 200 + mint cookie
			w.Header().Add("Set-Cookie", "dsh-auth-abc123=session; Path=/; HttpOnly")
			w.WriteHeader(http.StatusOK)
		case "legacy303token00000000000000000000000000000":
			w.WriteHeader(http.StatusFound)
		default:
			// 0.2.2 answers 200 for WRONG tokens too — status alone must not pass
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	tu := &Tunnel{cfg: Config{LocalAddr: addr}}
	if !tu.probeGUIToken(tok) {
		t.Fatal("valid token (200 + dsh-auth cookie) must probe true")
	}
	if !tu.probeGUIToken("legacy303token00000000000000000000000000000") {
		t.Fatal("legacy 303 token must probe true")
	}
	if tu.probeGUIToken(garbage) {
		t.Fatal("bare 200 without dsh-auth cookie must probe FALSE (0.2.2 gate answers 200 for wrong tokens)")
	}
	if tu.probeGUIToken(strings.Repeat("x", 43)) {
		t.Fatal("wrong token must probe false")
	}
	if tu.probeGUIToken("short") {
		t.Fatal("wrong length must probe false without network")
	}
}

// 注入内存优先：guiToken 有值时不读文件（文件缺失也注入）。
func TestInjectMemoryFirst(t *testing.T) {
	tok := strings.Repeat("m", 43)
	tu := &Tunnel{cfg: Config{GUICookieAuthority: "127.0.0.1:3082"}}
	tu.guiToken.set(tok)
	got := tu.injectGUIToken("GET", "/", nil, "/nonexistent/token-file")
	if got != "/?token="+tok {
		t.Fatalf("memory-first injection = %q", got)
	}
}
