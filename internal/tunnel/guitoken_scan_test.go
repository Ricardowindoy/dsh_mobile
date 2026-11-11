package tunnel

import (
	"bytes"
	"math/rand"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// 流式分块扫描必须与整缓冲扫描产出完全相同的候选序列（顺序含去重）。
// 性质测试钉恒等：随机填充 + 注入 token（含恰好跨块边界的）+ 诱饵
// （42/44 位、更长 base64url 串、非法字符）。
func TestScanStreamMatchesWholeBuffer(t *testing.T) {
	tok := strings.Repeat("aB3_-", 8) + "xyz" // 43 chars
	rng := rand.New(rand.NewSource(42))

	mk := func(size int, placements []int) []byte {
		const fillers = " \x00\n.:/{}\"'!@#$%^&*()=[]"
		b := make([]byte, size)
		for i := range b {
			// 混合：非 token 字节为主，掺 base64url 长串诱饵
			switch rng.Intn(10) {
			case 0:
				b[i] = byte('A' + rng.Intn(26))
			case 1:
				b[i] = byte('0' + rng.Intn(10))
			default:
				b[i] = fillers[rng.Intn(len(fillers))]
			}
		}
		for _, p := range placements {
			if p >= 0 && p+len(tok) <= len(b) {
				if p > 0 {
					b[p-1] = ' '
				}
				copy(b[p:], tok)
				if p+len(tok) < len(b) {
					b[p+len(tok)] = 0
				}
			}
		}
		return b
	}

	chunk := tokenScanChunkSize
	cases := []struct {
		name string
		b    []byte
	}{
		{"empty", nil},
		{"tiny", []byte(" " + tok + " ")},
		{"exact chunk", mk(chunk, []int{chunk / 2})},
		{"token straddles boundary", mk(2*chunk+123, []int{chunk - 20, chunk - 42, chunk - 1, 2*chunk - 10})},
		{"token at boundary-43", mk(2*chunk, []int{chunk - tokenCandidateLen, chunk - tokenCandidateLen - 1})},
		{"multi chunk random", mk(3*chunk+7777, []int{100, chunk + 5, 2*chunk - 21, 3 * chunk})},
		{"long base64url runs (decoys)", func() []byte {
			b := mk(2*chunk+50, []int{chunk - 30})
			// 一段 5000 字节的连续 base64url（跨块）——不得产出候选
			for i := chunk - 2500; i < chunk+2500; i++ {
				b[i] = byte('a' + i%26)
			}
			return b
		}()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := scanTokenCandidates(c.b)
			got := scanTokenCandidatesStream(bytes.NewReader(c.b), int64(len(c.b)))
			if len(got) == 0 && len(want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("stream != whole: got %d cands %v..., want %d %v...", len(got), head(got), len(want), head(want))
			}
		})
	}
}

func head(s []string) []string {
	if len(s) > 3 {
		return s[:3]
	}
	return s
}

// 内存门禁（对真实入口 tokenCandidatesFromHeap）：扫描 64MB「进程堆」的
// HeapAlloc 峰值增量必须远小于堆本身。修复前整缓冲 make([]byte, size)
// 在此规模直接把 64MB 钉进大对象 span（生产中 node [heap] 200MB+，每 5s
// 一次 → RSS 数百 MB 且不归还）。用 /proc/self/mem 读自身匿名映射，
// 走的是与生产完全相同的代码路径。
func TestHeapScanBoundedMemory(t *testing.T) {
	const size = 64 << 20
	region := make([]byte, size) // 匿名映射；/proc/self/maps 中无路径名
	for i := range region {
		region[i] = byte(i % 251)
	}
	start, end := regionBounds(t, region)
	fake := &Tunnel{cfg: Config{LocalAddr: "127.0.0.1:1"}}
	fake.heapPidOverride = os.Getpid()
	fake.heapRegionOverride = func(int) (int, int, error) { return start, end, nil }

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	stop := make(chan struct{})
	var peak atomic.Int64
	go func() {
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.ReadMemStats(&ms)
			if d := int64(ms.HeapAlloc) - int64(before.HeapAlloc); d > peak.Load() {
				peak.Store(d)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	cands := fake.tokenCandidatesFromHeap()
	close(stop)
	time.Sleep(5 * time.Millisecond)
	runtime.KeepAlive(region)
	if len(cands) > 1000 { // 随机字节偶尔撞出少量候选属正常
		t.Fatalf("unexpected candidate flood: %d", len(cands))
	}
	// 门禁：峰值增量 < 16MB（1MB 块 + 候选串 + 采样开销）。
	// 变异检验：换回 make([]byte, size) 整缓冲 → 峰值 ≥64MB → 必红。
	if peak.Load() > 16<<20 {
		t.Fatalf("heap scan peaked at +%d bytes (>16MB): whole-heap buffering regressed", peak.Load())
	}
	t.Logf("HeapAlloc peak delta for 64MB heap scan: %d bytes", peak.Load())
}

// regionBounds 从 /proc/self/maps 找到 backing slice 的匿名映射区间。
func regionBounds(t *testing.T, region []byte) (int, int) {
	t.Helper()
	addr := uintptr(unsafe.Pointer(&region[0]))
	data, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Skip("no /proc/self/maps")
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		parts := strings.SplitN(fields[0], "-", 2)
		if len(parts) != 2 {
			continue
		}
		lo, e1 := strconv.ParseUint(parts[0], 16, 64)
		hi, e2 := strconv.ParseUint(parts[1], 16, 64)
		if e1 != nil || e2 != nil {
			continue
		}
		if uint64(addr) >= lo && uint64(addr) < hi {
			return int(lo), int(hi)
		}
	}
	t.Fatal("region mapping not found")
	return 0, 0
}
