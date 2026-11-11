package tunnel

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 发现循环成本门禁（泄漏根因回归）：
// ① 缓存 token 仍有效时，journal/heap 发现路径不得运行（生产中 = 每 5s
//
//	spawn journalctl + 整读 node 堆）；
//
// ② 缓存失效后必须重新发现并更新文件（dsh 重启换 token 的自愈不变）；
// ③ 被拒候选进负缓存：同一垃圾候选在 TTL 内不得重复 probe。
func TestDiscoverLoopSkipsWhenCacheValid(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	tokA := strings.Repeat("A", 43)
	tokB := strings.Repeat("B", 43)
	var current atomic.Value // 当前有效 token
	current.Store(tokA)
	var probes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		if r.URL.Query().Get("token") == current.Load().(string) {
			w.Header().Add("Set-Cookie", "dsh-auth-x=1; Path=/")
		}
		w.WriteHeader(http.StatusOK) // 0.2.2：对错都 200，判据只看 cookie
	}))
	defer srv.Close()

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(tokA), 0o600); err != nil {
		t.Fatal(err)
	}

	tu := New(Config{
		LocalAddr:        strings.TrimPrefix(srv.URL, "http://"),
		GUIAuthTokenFile: tokenFile,
		DiscoverInterval: 30 * time.Millisecond,
		Logger:           quiet,
	})
	defer tu.Close()

	// 等 seed 生效
	deadline := time.Now().Add(2 * time.Second)
	for tu.guiToken.get() != tokA && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if tu.guiToken.get() != tokA {
		t.Fatal("token not seeded from file")
	}
	// 稳态门禁：跑 ~10 个 tick，发现路径（journal+heap）运行次数必须为 0。
	// 旧代码每 tick 无条件 findGUIToken → discoverRuns ≈ tick 数 → 必红。
	runsBase := tu.discoverRuns.Load()
	validBase := tu.validTicks.Load()
	time.Sleep(350 * time.Millisecond)
	if runs := tu.discoverRuns.Load() - runsBase; runs != 0 {
		t.Fatalf("discovery ran %d times while cached token was valid (must be 0)", runs)
	}
	if ticks := tu.validTicks.Load() - validBase; ticks < 5 {
		t.Fatalf("expected >=5 cache-revalidation ticks in 350ms, got %d", ticks)
	}

	// dsh「重启」：换 token。缓存复核失败 → 必须走发现路径；无 journal/heap
	// 可发现时，文件里的新 token（外部 refresher 写入）必须被接住。
	current.Store(tokB)
	if err := os.WriteFile(tokenFile, []byte(tokB), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for tu.guiToken.get() != tokB && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if tu.guiToken.get() != tokB {
		t.Fatal("stale cache not refreshed after token rotation")
	}
}

// 负缓存门禁：同一被拒候选在 TTL 内只 probe 一次。
func TestRejectedCandidateNegativeCache(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	garbage := strings.Repeat("g", 43)
	var probes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		w.WriteHeader(http.StatusOK) // 无 cookie = 拒绝
	}))
	defer srv.Close()
	tu := &Tunnel{cfg: Config{LocalAddr: strings.TrimPrefix(srv.URL, "http://")}, log: quiet}

	if tu.probeGUIToken(garbage) {
		t.Fatal("garbage must not probe true")
	}
	tu.reject(garbage)
	for i := 0; i < 5; i++ {
		if !tu.rejected(garbage) {
			t.Fatalf("candidate must stay rejected within TTL (iter %d)", i)
		}
	}
	// rejected() 门禁本身不得发 probe
	before := probes.Load()
	_ = tu.rejected(garbage)
	if probes.Load() != before {
		t.Fatal("no probes expected from cache checks")
	}
}

// 发现退避门禁：token 根本无法发现时（生产中 = V8 mmap 堆里的 token 不在
// brk [heap]），发现路径不得每个 tick 都跑。30ms tick × 500ms：无退避 →
// ~16 次；有退避（首次失败后 ≥30s）→ 恰好 1 次。
func TestDiscoveryBacksOffWhenNothingValid(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // 一切候选都拒绝（无 cookie）
	}))
	defer srv.Close()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(strings.Repeat("z", 43)), 0o600); err != nil {
		t.Fatal(err)
	}
	tu := New(Config{
		LocalAddr:        strings.TrimPrefix(srv.URL, "http://"),
		GUIAuthTokenFile: tokenFile,
		DiscoverInterval: 30 * time.Millisecond,
		Logger:           quiet,
	})
	defer tu.Close()
	time.Sleep(500 * time.Millisecond)
	if runs := tu.discoverRuns.Load(); runs != 1 {
		t.Fatalf("discovery ran %d times in 500ms with nothing valid (want exactly 1, then backoff)", runs)
	}
}
