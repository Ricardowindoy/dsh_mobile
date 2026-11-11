package mux

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"dsh_mobile/internal/protocol"
)

// pair spins up a websocket server and returns a client-connected mux plus
// the server-side mux.
func pair(t *testing.T, clientCfg, serverCfg Config) (*Mux, *Mux) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	serverMuxCh := make(chan *Mux, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		serverMuxCh <- New(conn, serverCfg)
	}))
	t.Cleanup(srv.Close)

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	clientMux := New(conn, clientCfg)
	// Wait for the server side to be created.
	select {
	case serverMux := <-serverMuxCh:
		return clientMux, serverMux
	case <-time.After(2 * time.Second):
		t.Fatal("server mux not created")
		return nil, nil
	}
}

func testCfg() Config {
	return Config{
		HeartbeatInterval:    time.Second,
		MaxStreams:           16,
		PerStreamBufferBytes: 64 * 1024,
		SendPings:            true,
		Logger:               discardLogger(),
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestOpenAndAccept(t *testing.T) {
	gotOpenCh := make(chan protocol.Message, 1)
	serverCfg := testCfg()
	serverCfg.OnStreamOpen = func(s *Stream, msg protocol.Message) {
		gotOpenCh <- msg
		_ = s.Accept(200, map[string][]string{"Content-Type": {"text/plain"}})
		go func() {
			_, _ = s.Write([]byte("hello"))
			_ = s.CloseWrite()
		}()
	}
	client, server := pair(t, testCfg(), serverCfg)
	defer client.Close()
	defer server.Close()

	s, err := client.OpenStream("GET", "/api/x", map[string][]string{"Host": {"relay"}}, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.CloseWrite(); err != nil {
		t.Fatalf("closewrite: %v", err)
	}
	info, err := s.WaitAccept()
	if err != nil {
		t.Fatalf("wait accept: %v", err)
	}
	if info.Status != 200 {
		t.Errorf("status = %d, want 200", info.Status)
	}
	data, err := io.ReadAll(s)
	if err != nil && err != io.EOF {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("body = %q, want hello", data)
	}
	msg := <-gotOpenCh
	if msg.Method != "GET" || msg.Path != "/api/x" {
		t.Errorf("open msg = %+v", msg)
	}
}

func TestReject(t *testing.T) {
	serverCfg := testCfg()
	serverCfg.OnStreamOpen = func(s *Stream, msg protocol.Message) {
		_ = s.Reject(502)
	}
	client, server := pair(t, testCfg(), serverCfg)
	defer client.Close()
	defer server.Close()

	s, err := client.OpenStream("GET", "/", nil, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = s.WaitAccept()
	rej, ok := err.(*RejectedError)
	if !ok {
		t.Fatalf("err = %v, want RejectedError", err)
	}
	if rej.Status != 502 {
		t.Errorf("status = %d, want 502", rej.Status)
	}
}

func TestConcurrentStreams(t *testing.T) {
	const n = 8
	const payloadSize = 100 * 1024
	serverCfg := testCfg()
	serverCfg.OnStreamOpen = func(s *Stream, msg protocol.Message) {
		_ = s.Accept(200, nil)
		go func() {
			// Echo everything back.
			_, _ = io.Copy(s, s)
			_ = s.CloseWrite()
		}()
	}
	client, server := pair(t, testCfg(), serverCfg)
	defer client.Close()
	defer server.Close()

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := client.OpenStream("POST", "/echo", nil, false)
			if err != nil {
				errs <- err
				return
			}
			payload := bytes.Repeat([]byte{byte(i)}, payloadSize)
			go func() {
				_, _ = s.Write(payload)
				_ = s.CloseWrite()
			}()
			info, err := s.WaitAccept()
			if err != nil {
				errs <- err
				return
			}
			if info.Status != 200 {
				errs <- io.ErrUnexpectedEOF
				return
			}
			got, err := io.ReadAll(s)
			if err != nil && err != io.EOF {
				errs <- err
				return
			}
			if !bytes.Equal(got, payload) {
				errs <- io.ErrShortWrite
				return
			}
			errs <- nil
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
	}
}

func TestBackpressureNoFrameLoss(t *testing.T) {
	// Server writes a large payload then closes; client reads slowly.
	// The per-stream queue must not drop data.
	const payloadSize = 512 * 1024
	serverCfg := testCfg()
	serverCfg.OnStreamOpen = func(s *Stream, msg protocol.Message) {
		_ = s.Accept(200, nil)
		go func() {
			_, _ = s.Write(bytes.Repeat([]byte("z"), payloadSize))
			_ = s.CloseWrite()
		}()
	}
	client, server := pair(t, testCfg(), serverCfg)
	defer client.Close()
	defer server.Close()

	s, err := client.OpenStream("GET", "/big", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.CloseWrite()
	if _, err := s.WaitAccept(); err != nil {
		t.Fatal(err)
	}
	total := 0
	buf := make([]byte, 4*1024)
	for {
		n, err := s.Read(buf)
		total += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		time.Sleep(time.Millisecond) // simulate slow consumer
	}
	if total != payloadSize {
		t.Errorf("total = %d, want %d (frame loss)", total, payloadSize)
	}
}

func TestHalfClose(t *testing.T) {
	// Client writes then half-closes; server must read exactly the payload
	// then io.EOF.
	serverCfg := testCfg()
	got := make(chan []byte, 1)
	serverCfg.OnStreamOpen = func(s *Stream, msg protocol.Message) {
		_ = s.Accept(200, nil)
		go func() {
			data, err := io.ReadAll(s)
			if err != nil && err != io.EOF {
				t.Errorf("server read: %v", err)
			}
			got <- data
		}()
	}
	client, server := pair(t, testCfg(), serverCfg)
	defer client.Close()
	defer server.Close()

	s, err := client.OpenStream("POST", "/", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("body")); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-got:
		if string(data) != "body" {
			t.Errorf("server got %q", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not observe half-close")
	}
}

func TestMaxStreams(t *testing.T) {
	serverCfg := testCfg()
	serverCfg.MaxStreams = 2
	// Accept and hold the first two streams open.
	serverCfg.OnStreamOpen = func(s *Stream, msg protocol.Message) {
		_ = s.Accept(200, nil)
		<-s.mux.done
	}
	clientCfg := testCfg()
	clientCfg.MaxStreams = 2
	client, server := pair(t, clientCfg, serverCfg)
	defer client.Close()
	defer server.Close()

	s1, err := client.OpenStream("GET", "/1", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s2, err := client.OpenStream("GET", "/2", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := client.OpenStream("GET", "/3", nil, false); err == nil {
		t.Fatal("expected max streams error")
	}
}

func TestIdleSweep(t *testing.T) {
	serverCfg := testCfg()
	serverCfg.IdleTimeout = 50 * time.Millisecond
	serverCfg.OnStreamOpen = func(s *Stream, msg protocol.Message) {
		_ = s.Accept(200, nil)
		<-s.mux.done
	}
	client, server := pair(t, testCfg(), serverCfg)
	defer client.Close()
	defer server.Close()

	s, err := client.OpenStream("GET", "/", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WaitAccept(); err != nil {
		t.Fatal(err)
	}
	// No traffic: the idle sweep must close the stream.
	if _, err := s.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected idle close")
	}
}

// TestResetUnblocksPeer verifies the core of the leak fix: when one side
// aborts a stream with Reset (phone hung up mid-SSE), the peer's blocked
// Read returns immediately instead of hanging until an idle sweep that never
// fires for an actively-streaming stream.
func TestResetUnblocksPeer(t *testing.T) {
	serverCfg := testCfg()
	readErrCh := make(chan error, 1)
	serverCfg.OnStreamOpen = func(s *Stream, msg protocol.Message) {
		// Simulate a handler blocked reading the peer's stream (like
		// tunnel's io.Copy(local, s) waiting for the request body).
		go func() {
			buf := make([]byte, 16)
			for {
				if _, err := s.Read(buf); err != nil {
					readErrCh <- err
					return
				}
			}
		}()
	}
	client, server := pair(t, testCfg(), serverCfg)
	defer client.Close()
	defer server.Close()

	s, err := client.OpenStream("GET", "/", nil, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Abort the stream from the client side (relay's phone hang-up path).
	if err := s.Reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	// The server's blocked Read must return promptly.
	select {
	case err := <-readErrCh:
		if err == nil {
			t.Fatal("read returned nil error after reset, want error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server read still blocked 2s after reset (connection leak)")
	}
}

// 断连原因分类是排查"连接不稳"的核心：必须能把对端 close 帧、读超时、
// EOF、本地关闭区分开，否则日志里只剩一句 connection lost。
func TestClassifyDisconnectReasons(t *testing.T) {
	// 注意：这里直接构造错误值，不依赖真实网络。
	if got := classify(nil); got != "closed" {
		t.Errorf("nil error = %q", got)
	}
	if got := classify(&websocket.CloseError{Code: websocket.CloseGoingAway, Text: "bye"}); !strings.Contains(got, "close frame") || !strings.Contains(got, "1001") {
		t.Errorf("close error = %q", got)
	}
	if got := classify(os.ErrDeadlineExceeded); !strings.Contains(got, "read deadline") {
		t.Errorf("deadline = %q", got)
	}
	if got := classify(io.EOF); !strings.Contains(got, "EOF") {
		t.Errorf("eof = %q", got)
	}
	if got := classify(net.ErrClosed); !strings.Contains(got, "locally") {
		t.Errorf("net.ErrClosed = %q", got)
	}
	if got := classify(errors.New("dial tcp: i/o timeout")); !strings.Contains(got, "i/o timeout") {
		t.Errorf("generic error = %q", got)
	}
}

// lockedBuffer 让测试可以安全地在多个 goroutine（mux 读循环 + 主测试）之间
// 共享一个 bytes.Buffer（-race 下裸 bytes.Buffer 会报数据竞争）。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// diagSinkFor 让测试可以复用 lockedBuffer 作为 diag sink。
func diagSinkFor(b *lockedBuffer) io.Writer { return b }

// 生命周期 sink 必须真的收到"link down + 原因"，且只写一次（shutdown 幂等）。
func TestMuxWritesLifecycleRecordOnPeerClose(t *testing.T) {
	diagSink := &lockedBuffer{}
	cfg := testCfg()
	cfg.Diagnostic = diagSink
	client, server := pair(t, cfg, cfg)
	s, err := client.OpenStream("GET", "/api/whatever", map[string][]string{"/api/whatever": {""}}, false)
	if err == nil {
		_ = s.CloseWrite()
	}
	// 让对端主动断开，模拟网络中断。
	server.Close()
	select {
	case <-client.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("client mux did not notice the peer going away")
	}
	client.Close()

	out := diagSink.String()
	if !strings.Contains(out, "link down") {
		t.Fatalf("no lifecycle record for the drop: %q", out)
	}
	if !strings.Contains(out, "reason=") {
		t.Fatalf("lifecycle record has no reason: %q", out)
	}
	if n := strings.Count(out, "link down"); n != 1 {
		t.Errorf("link down recorded %d times, want exactly 1: %q", n, out)
	}
	if _, code := client.Diagnose(); code != 0 && code != websocket.CloseNormalClosure && code != websocket.CloseAbnormalClosure && code != websocket.CloseGoingAway {
		t.Errorf("unexpected close code %d", code)
	}
}

// 控制帧必须优先于数据帧：并发打开多个流、同时塞入大数据帧时，
// stream.accept 不能等数据帧全部出队。这个行为直接决定 waitAccept/TTFB。
func TestControlFramesPrioritizedOverData(t *testing.T) {
	var diagBuf lockedBuffer
	cfg := testCfg()
	cfg.Diagnostic = diagSinkFor(&diagBuf)
	// server 端：客户端一 open 就立刻 Accept + 写 2MB 数据 + 关流。
	// client 端并发读，否则每流背压会让写端永久阻塞。
	serverCfg := testCfg()
	serverCfg.OnStreamOpen = func(s *Stream, msg protocol.Message) {
		_ = s.Accept(200, map[string][]string{"Content-Type": {"application/octet-stream"}})
		go func() {
			big := make([]byte, 256*1024)
			for i := 0; i < 8; i++ {
				if _, err := s.Write(big); err != nil {
					break
				}
			}
			_ = s.CloseWrite()
		}()
	}
	client, server := pair(t, cfg, serverCfg)
	defer client.Close()
	defer server.Close()

	// 同时打开 8 个流；每个流 server 都会立刻 Accept，同时开始写大块数据。
	// 若 accept 控制帧被数据帧堵住，WaitAccept 会被拖到数据帧写完。
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			s, err := client.OpenStream("GET", "/api/x", nil, false)
			if err != nil {
				done <- err
				return
			}
			_, err = s.WaitAccept()
			if err != nil {
				done <- err
				return
			}
			// 读掉数据，避免写端背压卡死（这同时也验证流本身能工作）。
			_, _ = io.Copy(io.Discard, s)
			done <- nil
		}()
	}
	start := time.Now()
	var first error
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil && first == nil {
			first = err
		}
	}
	elapsed := time.Since(start)
	if first != nil {
		t.Fatalf("open/accept failed: %v", first)
	}
	// 8 个流共 2MB 数据，每个都要求 accept 尽快返回；宽松阈值 2s，
	// 若无优先级控制帧会被拖到远大于此。
	if elapsed > 2*time.Second {
		t.Errorf("accepts took %v (>2s) — control frames stuck behind data frames", elapsed)
	}
}

// 慢消费者不能阻塞其他流：这是"公网访问卡"的根因回归测试。
// 旧实现用容量 5 帧的 chan 直接投递数据，一个不读的流（手机慢下载）
// 会让 mux 的 readLoop 阻塞，导致所有其他流的 open/accept 全部停摆
// （实测生产环境 /favicon.svg 首字节 25 秒）。
func TestSlowConsumerDoesNotBlockOtherStreams(t *testing.T) {
	slowStarted := make(chan struct{})
	serverCfg := testCfg()
	serverCfg.OnStreamOpen = func(s *Stream, msg protocol.Message) {
		_ = s.Accept(200, nil)
		if msg.Path == "/slow" {
			go func() {
				close(slowStarted)
				big := make([]byte, 64*1024)
				for i := 0; i < 8; i++ { // 512KB > 旧实现的 5 帧(320KB) 缓冲
					if _, err := s.Write(big); err != nil {
						return
					}
				}
				_ = s.CloseWrite()
			}()
			return
		}
		go func() {
			_, _ = s.Write([]byte("ok"))
			_ = s.CloseWrite()
		}()
	}
	client, server := pair(t, testCfg(), serverCfg)
	defer client.Close()
	defer server.Close()

	// 打开 /slow 但从不 Read —— 模拟手机在弱网下慢慢下载大响应。
	slow, err := client.OpenStream("GET", "/slow", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := slow.WaitAccept(); err != nil {
		t.Fatal(err)
	}
	<-slowStarted
	time.Sleep(300 * time.Millisecond) // 让 /slow 的接收队列积压（旧实现此时已堵死 readLoop）

	// /fast 必须照常完成；旧实现会卡在 readLoop 上直到超时。
	done := make(chan error, 1)
	go func() {
		fast, err := client.OpenStream("GET", "/fast", nil, false)
		if err != nil {
			done <- err
			return
		}
		if _, err := fast.WaitAccept(); err != nil {
			done <- err
			return
		}
		data, err := io.ReadAll(fast)
		if err != nil {
			done <- err
			return
		}
		if string(data) != "ok" {
			done <- fmt.Errorf("fast got %q", data)
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stream B failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream B blocked >3s by slow consumer on stream A — readLoop head-of-line blocking")
	}
}
