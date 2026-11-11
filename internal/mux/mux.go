// Package mux implements stream multiplexing over a single WebSocket
// connection, shared by the relay (public side) and the tunnel (device side).
//
// The wire format is defined in dsh_mobile/internal/protocol: JSON text
// frames carry control messages, binary frames carry stream data with a
// 4-byte streamId prefix. A single writer goroutine serializes all frames;
// per-stream bounded queues provide backpressure (upstream pause) without
// dropping frames.
//
// Lifecycle of a stream (phone -> relay -> tunnel -> local dsh web server):
//
//	relay   -> tunnel:  stream.open{method,path,headers,upgrade}
//	relay   -> tunnel:  data frames (request body)
//	relay   -> tunnel:  stream.end (request body complete)
//	tunnel  -> relay:   stream.accept{status,headers} (local response head)
//	                    or stream.reject{status} (local dial failed)
//	tunnel  -> relay:   data frames (response body)
//	tunnel  -> relay:   stream.end (response complete)
package mux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"dsh_mobile/internal/diag"
	"dsh_mobile/internal/protocol"
)

var (
	// ErrClosed is returned by operations on a closed mux or stream.
	ErrClosed = errors.New("mux: closed")
	// ErrIdle is returned when a stream is closed by the idle sweep.
	ErrIdle = errors.New("mux: idle timeout")
)

// RejectedError reports the status carried by a stream.reject frame.
type RejectedError struct {
	Status int
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("stream rejected with status %d", e.Status)
}

// Config configures a Mux.
type Config struct {
	// HeartbeatInterval is the application-level ping interval. Reads are
	// bounded by 2x this interval, so the peer must ping (or send any
	// frame) at least that often.
	HeartbeatInterval time.Duration
	// MaxStreams caps concurrent streams; exceeding opens are rejected.
	MaxStreams int
	// PerStreamBufferBytes caps buffered bytes per stream per direction
	// before backpressure engages.
	PerStreamBufferBytes int
	// StreamRecvLimitBytes is the hard per-stream receive-queue cap. A stream
	// that buffers more than this is reset (its consumer has stopped reading).
	// 0 keeps the historical default of PerStreamBufferBytes*8.
	//
	// Why configurable: a multi-MB response written to a slow client (a phone
	// on cellular) legitimately buffers several MB on the relay before the
	// socket drains. The old 2 MB default reset such a stream mid-response,
	// and the browser saw a truncated JS bundle ("import failed") with a clean
	// HTTP 200 — silent and very hard to diagnose. Raise this on public relays.
	StreamRecvLimitBytes int
	// TotalRecvLimitBytes caps the sum of all per-stream receive queues so
	// raising StreamRecvLimitBytes cannot blow up memory. 0 = unlimited.
	TotalRecvLimitBytes int
	// IdleTimeout closes streams with no traffic for this long
	// (0 disables the sweep).
	IdleTimeout time.Duration
	// ReadRateLimit caps incoming bytes per second (0 = unlimited).
	ReadRateLimit int64
	// WriteRateLimit caps outgoing bytes per second (0 = unlimited).
	WriteRateLimit int64
	// SendPings makes this side send application pings on the heartbeat
	// interval (relay side).
	SendPings bool
	// WriteQueueFrames is the data-frame queue capacity (0 = 64). A larger
	// queue smooths bursts from a fast LAN producer into a slower WAN, at the
	// cost of buffering; backpressure still reaches the producer once full.
	WriteQueueFrames int
	// FrameWriteTimeout bounds a single WebSocket frame write (0 = no bound).
	// A write stuck longer than this means the link is effectively dead (NAT
	// drop, half-open TCP); without the bound the writer goroutine can hang
	// for minutes until the read side notices, stalling every stream.
	FrameWriteTimeout time.Duration
	// OnStreamOpen is invoked when the peer asks to open a stream
	// (tunnel side: dial the local dsh web server).
	OnStreamOpen func(s *Stream, msg protocol.Message)
	// Diagnostic, when non-nil, receives one line per connection lifecycle
	// event (link up, link down with reason, stream swept as idle). Use
	// diag.NewFileWriter / diag.New for a file-backed sink.
	Diagnostic io.Writer
	// Logger, defaults to slog.Default().
	Logger *slog.Logger
}

// AcceptInfo carries the peer's stream.accept details (HTTP response head).
type AcceptInfo struct {
	Status  int
	Headers map[string][]string
}

// Stream is one multiplexed byte pipe inside the mux.
type Stream struct {
	sid uint32
	mux *Mux

	// recvMu 保护接收队列。数据投递必须永不阻塞 mux 的读循环：
	// 单个流的慢消费者（如公网手机下载大响应）曾把整个 readLoop 卡死
	// （recv chan 容量仅 5 帧，一满就阻塞），导致其他所有请求连
	// /favicon.svg 都要排 25 秒——这是"公网访问卡"的根因。
	recvMu     sync.Mutex
	recvCond   *sync.Cond
	recvQueue  [][]byte // FIFO 数据块
	recvBytes  int      // 队列中的总字节数
	recvEOF    bool     // 远端已 stream.end
	recvClosed bool     // 流已本地关闭
	recvLimit  int      // 队列上限（超出则重置该流，防止内存失控）

	// pending holds unread bytes not yet returned to the caller. Read is
	// single-goroutine per stream, so no locking is needed.
	pending []byte

	// verdict delivers the peer's stream.accept / stream.reject for
	// streams opened locally (relay side).
	verdict chan verdict

	// localEOF is closed when the local side sent stream.end.
	localEOF  chan struct{}
	localOnce sync.Once
	// remoteEOF is closed when stream.end is received from the peer.
	remoteEOF  chan struct{}
	remoteOnce sync.Once
	// closed is closed when the stream is torn down entirely.
	closed    chan struct{}
	closeOnce sync.Once

	// activity tracks the last read/write for the idle sweep.
	lastRead  atomic.Int64
	lastWrite atomic.Int64

	// openedAt is when the stream was created (stream age in diagnostics).
	openedAt time.Time
	// label carries the request path for diagnostics (relay side fills it
	// from stream.open; the tunnel side from the same message).
	label atomic.Value
	// bytes counts payload bytes carried in both directions, so an idle
	// sweep can say whether the stream ever moved data.
	bytes atomic.Int64

	// inflight 计数当前在写队列中排队的数据帧数（每流配额）。
	inflight   atomic.Int64
	inflightCh chan struct{} // 配额释放时通知等待者
}

type verdict struct {
	info AcceptInfo
	ok   bool
}

// Mux multiplexes streams over one WebSocket connection.
type Mux struct {
	conn *websocket.Conn
	cfg  Config
	log  *slog.Logger

	// writeQueue 承载数据帧；controlQueue 承载控制帧（open/accept/reject/end/
	// reset/ping/pong）。两者共用同一个 WebSocket 写循环，但控制帧永远优先，
	// 否则并发请求一多，stream.open 要等前面几百 KB 数据帧出队，每个请求的
	// waitAccept（首字节）被拖到秒级（实测 session/list 200KB 把 accept 堵了
	// 11.6s，其余小请求全被排到 9.6s 之后）。
	writeQueue   chan outFrame
	controlQueue chan outFrame
	done         chan struct{}
	closeOnce    sync.Once

	mu      sync.Mutex
	streams map[uint32]*Stream
	nextSID uint32

	readBucket  *tokenBucket
	writeBucket *tokenBucket
	lastRecv    atomic.Int64
	// recvTotal is the sum of every stream's queued-but-unread receive bytes.
	// It bounds total memory when per-stream limits are raised.
	recvTotal atomic.Int64
	// peerHeartbeat is the heartbeat interval the peer announced at pairing
	// (relay ok frame). 0 = unknown, fall back to our own interval.
	peerHeartbeat atomic.Int64

	// 断连归因：read/write 的失败原因与对端 close 帧。连接断开后调用方
	// （tunnel/relay）用它们打出"为什么断"，否则只剩一句 connection lost。
	reasonMu  sync.Mutex
	reason    string
	closeCode int
	closeText string
	openedAt  time.Time

	// 收发计数（诊断用，只累加）：区分"对端静默"与"我们自己发不出去"。
	readBytes    atomic.Int64
	writeBytes   atomic.Int64
	readFrames   atomic.Int64
	writeFrames  atomic.Int64
	controlReads atomic.Int64

	// 生命周期动作记录（连接建立/断开/空转流清理）。nil 时静默。
	diag     *diag.Logger
	diagOnce sync.Once

	// onStreamOpen is invoked when the peer asks to open a stream
	// (tunnel side: dial the local dsh web server).
	onStreamOpen func(s *Stream, msg protocol.Message)

	wg sync.WaitGroup
}

type outFrame struct {
	text bool
	data []byte
	// stream 非 nil 表示数据帧：writeLoop 写出后归还该流的在途配额。
	stream *Stream
}

// maxInflightFramesPerStream 限制单流在写队列中的在途帧数。
// 大响应（如几 MB 的 session.history）不再饿死并发的小流
// （bundle 脚本/SSE 心跳），超出配额的流背压等待。
const maxInflightFramesPerStream = 4

// framePool 复用数据帧的 4 字节头 + payload 缓冲：12MB 响应 = ~740 个 16KB 帧，
// 不复用时每次 Write 都是全新分配（实测 GC 压力在 6 小核 ARM 上直接体现为
// 传输速率 3 倍波动）。sync.Pool 的 per-P 缓存对这种高频小块最合适。
var framePool = sync.Pool{
	New: func() any { return make([]byte, 4+protocol.MaxFrameSize) },
}

// acquireFrame 从池里取一块 buf 并写入 [4B sid][payload]，返回整帧切片。
func acquireFrame(sid uint32, payload []byte) []byte {
	buf := framePool.Get().([]byte)[:4+len(payload)]
	binary.BigEndian.PutUint32(buf, sid)
	copy(buf[4:], payload)
	return buf
}

// releaseFrame 在帧写出后归还缓冲。调用方必须保证此后不再引用它。
func releaseFrame(buf []byte) {
	if cap(buf) >= 4+protocol.MaxFrameSize {
		framePool.Put(buf[:4+protocol.MaxFrameSize])
	}
}

func defaultConfig(cfg *Config) Config {
	c := *cfg
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = protocol.DefaultHeartbeatMs * time.Millisecond
	}
	if c.MaxStreams <= 0 {
		c.MaxStreams = 64
	}
	if c.PerStreamBufferBytes <= 0 {
		c.PerStreamBufferBytes = 256 * 1024
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// New wraps a WebSocket connection in a mux and starts its pumps.
func New(conn *websocket.Conn, cfg Config) *Mux {
	c := defaultConfig(&cfg)
	m := &Mux{
		conn: conn,
		cfg:  c,
		log:  c.logger(),
		// 写队列只留 64 帧（16KB/帧 ≈ 1MB）作为缓冲。此前按 MaxStreams*2+8
		// 开 520 帧（≈33MB），上行（/api/remote.mux 等长连接）积压时新请求的
		// stream.open 要在队尾等 5~14 秒。容量收紧后背压前移到发送方
		// （手机/本地 dsh 的 TCP 窗口），而不是在 relay 里无声排队。
		writeQueue:   make(chan outFrame, max(c.WriteQueueFrames, 0)+64),
		controlQueue: make(chan outFrame, c.MaxStreams*2+8),
		done:         make(chan struct{}),
		streams:      make(map[uint32]*Stream),
		nextSID:      1,
		openedAt:     time.Now(),
	}
	if c.ReadRateLimit > 0 {
		m.readBucket = newTokenBucket(c.ReadRateLimit)
	}
	if c.WriteRateLimit > 0 {
		m.writeBucket = newTokenBucket(c.WriteRateLimit)
	}
	// lastRecv 必须从"现在"起步：零值会让"多久没收到对端任何帧"算出
	// 从 Unix epoch 起的毫秒数（实测 1.79e12 这种明显错误的值）。
	m.lastRecv.Store(time.Now().UnixNano())
	m.onStreamOpen = c.OnStreamOpen
	m.ensureDiag(c.Diagnostic)
	m.log.Info("mux: link up",
		"diag", diag.Version,
		"maxStreams", c.MaxStreams,
		"heartbeatMs", c.HeartbeatInterval.Milliseconds(),
		"idleTimeout", c.IdleTimeout.String(),
		"rateLimit", c.WriteRateLimit)
	m.wg.Add(3)
	go m.writeLoop()
	go m.readLoop()
	go m.heartbeatLoop()
	return m
}

// logger returns the configured logger, falling back to slog.Default so a
// zero-value Config never panics (tests construct Mux via New with defaults).
func (c Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// ensureDiag installs the diagnostic sink once. New() calls it; it is also a
// safety net for muxes constructed through NewDiagWriter.
func (m *Mux) ensureDiag(w io.Writer) {
	m.diagOnce.Do(func() {
		if w != nil {
			m.diag = diag.NewWithOptions(w, m.log, diag.Options{})
		}
	})
}

// SetDiagnostic attaches a lifecycle sink after construction (the tunnel sets
// it once the log file is open).
func (m *Mux) SetDiagnostic(w io.Writer) { m.ensureDiag(w) }

// SetPeerHeartbeat records the heartbeat interval the peer announced during
// pairing, so the liveness bound matches the peer's actual ping cadence
// instead of a locally assumed default.
func (m *Mux) SetPeerHeartbeat(ms int) {
	if ms > 0 {
		m.peerHeartbeat.Store(int64(ms))
	}
}

// Diagnose returns a human-readable description of why the connection
// ended, plus the peer's websocket close code (0 when unknown).
func (m *Mux) Diagnose() (string, int) {
	m.reasonMu.Lock()
	defer m.reasonMu.Unlock()
	code := m.closeCode
	if m.closeText != "" {
		return fmt.Sprintf("%s (%d %q)", m.reason, code, m.closeText), code
	}
	return m.reason, code
}

// Uptime returns how long this mux has been alive.
func (m *Mux) Uptime() time.Duration { return time.Since(m.openedAt) }

// reasonOr classifies an I/O error into a stable, greppable reason string.
// It also records the classification (first one wins, since the first error
// is the cause and the rest are fallout of the same teardown).
func (m *Mux) reasonOr(err error) string {
	m.reasonMu.Lock()
	defer m.reasonMu.Unlock()
	if m.reason != "" {
		return m.reason
	}
	m.reason = classify(err)
	return m.reason
}

// SetReason records an explicit cause (used when the local side tears the
// connection down deliberately, e.g. a superseded device connection).
func (m *Mux) SetReason(reason string) {
	m.reasonMu.Lock()
	if m.reason == "" {
		m.reason = reason
	}
	m.reasonMu.Unlock()
}

// noteClose records the peer's close code/text when one arrived.
func (m *Mux) noteClose(code int, text string) {
	m.reasonMu.Lock()
	if m.closeCode == 0 {
		m.closeCode, m.closeText = code, text
	}
	m.reasonMu.Unlock()
}

// classify turns a websocket/network error into a stable reason string.
func classify(err error) string {
	if err == nil {
		return "closed"
	}
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return fmt.Sprintf("peer close frame (code=%d text=%q)", ce.Code, ce.Text)
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, websocket.ErrCloseSent) {
		return "connection closed locally"
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return "peer silent past read deadline (heartbeat lost)"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "network timeout"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return "peer closed connection (EOF)"
	}
	return "read/write error: " + err.Error()
}

// shutdown tears the mux down once; safe from any goroutine. explicit marks a
// deliberate local teardown (process restart, device superseded) as opposed to
// a transport failure, so the lifecycle log can say which one happened.
func (m *Mux) shutdown(explicit ...bool) {
	deliberate := len(explicit) > 0 && explicit[0]
	m.closeOnce.Do(func() {
		if !deliberate {
			m.diagLost("tunnel-lost")
		}
		close(m.done)
		_ = m.conn.Close()
		m.mu.Lock()
		streams := make([]*Stream, 0, len(m.streams))
		for _, s := range m.streams {
			streams = append(streams, s)
		}
		m.streams = make(map[uint32]*Stream)
		m.mu.Unlock()
		for _, s := range streams {
			s.close()
		}
	})
}

// diagLost writes the one line that explains why this link ended. It is the
// line every "connection is unstable" investigation starts from: without the
// reason and the uptime you cannot tell a phone-side network flap from a relay
// kick from our own read deadline firing.
func (m *Mux) diagLost(kind string) {
	reason, code := m.Diagnose()
	m.diag.Records(kind, "link down",
		"reason", reason,
		"closeCode", code,
		"uptime_s", fmt.Sprintf("%.1f", m.Uptime().Seconds()),
		"streamsAtClose", m.StreamCount(),
		"readFrames", m.readFrames.Load(),
		"writeFrames", m.writeFrames.Load(),
		"readBytes", m.readBytes.Load(),
		"writeBytes", m.writeBytes.Load(),
		"lastRecvAgoMs", m.LastRecvAgo().Milliseconds(),
	)
}

// Close shuts the mux down and closes every stream.
func (m *Mux) Close() error {
	m.SetReason("closed locally by this process")
	m.shutdown(true)
	m.wg.Wait()
	return ErrClosed
}

// Wait blocks until the mux has shut down.
func (m *Mux) Wait() {
	m.wg.Wait()
}

// Done reports whether the mux is closed.
func (m *Mux) Done() <-chan struct{} { return m.done }

// LastRecv reports when the last frame arrived from the peer (its own start
// time until the first frame arrives).
func (m *Mux) LastRecv() time.Time {
	return time.Unix(0, m.lastRecv.Load())
}

// LastRecvAgo is how long the peer has been silent, never negative.
func (m *Mux) LastRecvAgo() time.Duration {
	d := time.Since(m.LastRecv())
	if d < 0 {
		return 0
	}
	return d
}

// OpenStream allocates a stream id and sends stream.open to the peer.
// It returns immediately; the caller writes the request body, half-closes
// with CloseWrite, then awaits the verdict via WaitAccept. Used by the relay.
func (m *Mux) OpenStream(method, path string, headers map[string][]string, upgrade bool) (*Stream, error) {
	select {
	case <-m.done:
		return nil, ErrClosed
	default:
	}
	m.mu.Lock()
	if len(m.streams) >= m.cfg.MaxStreams {
		m.mu.Unlock()
		return nil, fmt.Errorf("mux: max concurrent streams (%d) reached", m.cfg.MaxStreams)
	}
	sid := m.nextSID
	m.nextSID++
	if m.nextSID == 0 {
		m.nextSID = 1
	}
	s := &Stream{
		sid:        sid,
		mux:        m,
		recvLimit:  m.streamRecvLimit(),
		verdict:    make(chan verdict, 1),
		localEOF:   make(chan struct{}),
		remoteEOF:  make(chan struct{}),
		closed:     make(chan struct{}),
		inflightCh: make(chan struct{}, 1),
		openedAt:   time.Now(),
	}
	s.recvCond = sync.NewCond(&s.recvMu)
	s.touchRead()
	s.touchWrite()
	s.SetLabel(method + " " + path)
	m.streams[sid] = s
	m.mu.Unlock()

	msg := protocol.Message{
		Type:    protocol.TypeStreamOpen,
		SID:     sid,
		Method:  method,
		Path:    path,
		Headers: headers,
		Upgrade: upgrade,
	}
	if err := m.sendControl(msg); err != nil {
		m.removeStream(sid)
		return nil, err
	}
	return s, nil
}

// WaitAccept blocks until the peer answers with stream.accept or
// stream.reject. Call after writing the request body and CloseWrite.
// Data frames arriving before the verdict are buffered into the stream's
// pending queue (bounded), so an eager peer cannot wedge the read loop.
func (s *Stream) WaitAccept() (AcceptInfo, error) {
	for {
		select {
		case v := <-s.verdict:
			if !v.ok {
				return AcceptInfo{}, &RejectedError{Status: v.info.Status}
			}
			return v.info, nil
		default:
		}
		select {
		case v := <-s.verdict:
			if !v.ok {
				return AcceptInfo{}, &RejectedError{Status: v.info.Status}
			}
			return v.info, nil
		case <-s.closed:
			return AcceptInfo{}, ErrClosed
		case <-s.mux.done:
			return AcceptInfo{}, ErrClosed
		}
	}
}

// acceptStream registers a stream announced by the peer's stream.open.
func (m *Mux) acceptStream(sid uint32) (*Stream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.streams) >= m.cfg.MaxStreams {
		return nil, fmt.Errorf("mux: max concurrent streams (%d) reached", m.cfg.MaxStreams)
	}
	if _, exists := m.streams[sid]; exists {
		return nil, fmt.Errorf("mux: duplicate stream id %d", sid)
	}
	s := &Stream{
		sid:        sid,
		mux:        m,
		recvLimit:  m.streamRecvLimit(),
		verdict:    make(chan verdict, 1),
		localEOF:   make(chan struct{}),
		remoteEOF:  make(chan struct{}),
		closed:     make(chan struct{}),
		inflightCh: make(chan struct{}, 1),
		openedAt:   time.Now(),
	}
	s.recvCond = sync.NewCond(&s.recvMu)
	s.touchRead()
	s.touchWrite()
	m.streams[sid] = s
	return s, nil
}

func (m *Mux) removeStream(sid uint32) {
	m.mu.Lock()
	s, ok := m.streams[sid]
	if ok {
		delete(m.streams, sid)
	}
	m.mu.Unlock()
	if ok {
		s.close()
	}
}

// StreamCount returns the number of live streams.
func (m *Mux) StreamCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.streams)
}

// sendControl queues a JSON control frame on the high-priority queue.
// Control frames are tiny and latency-critical (stream.accept carries most of
// the phone's perceived TTFB), so they must never queue behind data frames.
func (m *Mux) sendControl(msg protocol.Message) error {
	data, err := protocol.Marshal(msg)
	if err != nil {
		return err
	}
	select {
	case <-m.done:
		return ErrClosed
	case m.controlQueue <- outFrame{text: true, data: data}:
		return nil
	}
}

// enqueue queues a data frame on the regular (backpressure) queue.
func (m *Mux) enqueue(f outFrame) error {
	select {
	case <-m.done:
		return ErrClosed
	case m.writeQueue <- f:
		return nil
	}
}

// writeLoop serializes all writes to the WebSocket, draining the control
// queue first so control frames (stream.open/accept/reject/end/reset) never
// queue behind multi-hundred-KB data frames. Without this, a burst of
// concurrent requests makes every stream.waitAccept wait for the whole write
// backlog (measured: an accept behind a 200KB response took 11.6s, dragging
// all other first-bytes to ~10s).
func (m *Mux) writeLoop() {
	defer m.wg.Done()
	for {
		// 控制帧高优先级：非阻塞地先清空 controlQueue。
		for {
			select {
			case f := <-m.controlQueue:
				if m.writeFrame(f) {
					return
				}
				continue
			case <-m.done:
				return
			default:
			}
			break
		}
		// 控制队列空了，再等数据帧（或新到的控制帧，仍优先）。
		select {
		case <-m.done:
			return
		case f := <-m.controlQueue:
			if m.writeFrame(f) {
				return
			}
		case f := <-m.writeQueue:
			if m.writeFrame(f) {
				return
			}
		}
	}
}

// writeFrame writes one frame to the WebSocket and returns true if the
// connection failed (writeLoop should then exit via shutdown).
func (m *Mux) writeFrame(f outFrame) bool {
	if m.writeBucket != nil && !f.text {
		m.writeBucket.wait(len(f.data))
	}
	msgType := websocket.BinaryMessage
	if f.text {
		msgType = websocket.TextMessage
	}
	// 单帧写超时（可选）：卡死的链路不能拖死整个写循环。deadline 到期后
	// WriteMessage 返回错误，走统一的 shutdown 路径。
	if m.cfg.FrameWriteTimeout > 0 {
		_ = m.conn.SetWriteDeadline(time.Now().Add(m.cfg.FrameWriteTimeout))
	}
	writeStart := time.Now()
	if err := m.conn.WriteMessage(msgType, f.data); err != nil {
		m.reasonOr(err)
		m.log.Warn("mux: write failed", "err", err, "uptime", m.Uptime().String())
		m.shutdown()
		return true
	}
	if m.cfg.FrameWriteTimeout > 0 {
		_ = m.conn.SetWriteDeadline(time.Time{})
	}
	// 慢写诊断：单帧写出超过 1s 说明对端接收窗口/链路在阻塞，
	// 此时控制帧也只能排队——是"首字节秒级"问题的直接证据。
	if d := time.Since(writeStart); d > time.Second {
		m.log.Warn("mux: slow frame write", "dur", d.String(), "bytes", len(f.data), "text", f.text)
	}
	m.writeBytes.Add(int64(len(f.data)))
	m.writeFrames.Add(1)
	// 数据帧缓冲回到池里；控制帧（text）是一次性 JSON，交给 GC。
	if !f.text {
		releaseFrame(f.data)
	}
	// 归还该流的在途配额并唤醒等待者（大流背压时其他流照常走）。
	if f.stream != nil {
		if n := f.stream.inflight.Add(-1); n <= maxInflightFramesPerStream {
			select {
			case f.stream.inflightCh <- struct{}{}:
			default:
			}
		}
	}
	return false
}

// readLoop dispatches incoming frames to streams.
func (m *Mux) readLoop() {
	defer m.wg.Done()
	defer m.shutdown()
	for {
		// Liveness: the peer must send something within 2 heartbeats.
		if m.cfg.HeartbeatInterval > 0 {
			_ = m.conn.SetReadDeadline(time.Now().Add(m.readDeadline()))
		}
		msgType, data, err := m.conn.ReadMessage()
		if err != nil {
			m.reasonOr(err)
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				m.noteClose(ce.Code, ce.Text)
			}
			select {
			case <-m.done:
				m.log.Debug("mux: read ended after shutdown", "err", err)
			default:
				m.log.Warn("mux: read failed", "err", err, "uptime", m.Uptime().String())
			}
			return
		}
		if m.readBucket != nil {
			m.readBucket.wait(len(data))
		}
		m.lastRecv.Store(time.Now().UnixNano())
		m.readBytes.Add(int64(len(data)))
		switch msgType {
		case websocket.TextMessage:
			m.controlReads.Add(1)
			m.handleControl(data)
		case websocket.BinaryMessage:
			m.readFrames.Add(1)
			m.handleData(data)
		default:
			m.log.Warn("mux: unexpected message type", "type", msgType)
		}
	}
}

// readDeadline returns the current liveness bound: 2 heartbeats, but never
// longer than what the peer asked for at pairing time (the relay's ok frame
// carries its heartbeat interval, and a hard-coded expectation mismatch is
// exactly how a healthy link gets torn down as "silent").
func (m *Mux) readDeadline() time.Duration {
	d := 2 * m.cfg.HeartbeatInterval
	if m.peerHeartbeat.Load() > 0 {
		if peer := time.Duration(m.peerHeartbeat.Load()) * time.Millisecond; 2*peer < d {
			d = 2 * peer
		}
	}
	return d
}

// heartbeatLoop pings on the interval when configured, and sweeps idle
// streams.
func (m *Mux) heartbeatLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			if m.cfg.SendPings {
				_ = m.sendControl(protocol.Message{Type: protocol.TypePing})
			}
			if m.cfg.IdleTimeout > 0 {
				m.sweepIdle()
			}
		}
	}
}

func (m *Mux) sweepIdle() {
	cutoff := time.Now().Add(-m.cfg.IdleTimeout)
	var idle []*Stream
	m.mu.Lock()
	for _, s := range m.streams {
		last := time.Unix(0, max(s.lastRead.Load(), s.lastWrite.Load()))
		if last.Before(cutoff) {
			idle = append(idle, s)
		}
	}
	m.mu.Unlock()
	for _, s := range idle {
		idleFor := time.Since(time.Unix(0, max(s.lastRead.Load(), s.lastWrite.Load())))
		m.log.Info("mux: closing idle stream", "sid", s.sid, "idle", idleFor.Truncate(time.Second).String())
		m.diag.Records("stream-idle-swept", "stream closed by idle sweep",
			"sid", s.sid,
			"path", s.pathLabel(),
			"idle_s", fmt.Sprintf("%.1f", idleFor.Seconds()),
			"total_s", fmt.Sprintf("%.1f", time.Since(s.openedAt).Seconds()),
			"bytes", s.byteCount(),
		)
		_ = s.CloseWrite() // notify the peer (half-close) before teardown
		_ = s.Close()
	}
}

func (m *Mux) handleControl(data []byte) {
	msg, err := protocol.Unmarshal(data)
	if err != nil {
		m.log.Warn("mux: bad control frame", "err", err)
		return
	}
	switch msg.Type {
	case protocol.TypePing:
		if err := m.sendControl(protocol.Message{Type: protocol.TypePong}); err != nil {
			return
		}
	case protocol.TypePong:
		// Liveness is tracked via LastRecv; nothing else to do.
	case protocol.TypeStreamOpen:
		s, err := m.acceptStream(msg.SID)
		if err != nil {
			m.log.Warn("mux: refuse stream.open", "sid", msg.SID, "err", err)
			_ = m.sendControl(protocol.Message{
				Type:   protocol.TypeStreamReject,
				SID:    msg.SID,
				Status: 503,
			})
			return
		}
		s.SetLabel(msg.Method + " " + msg.Path)
		if m.onStreamOpen != nil {
			m.wg.Add(1)
			go func() {
				defer m.wg.Done()
				m.onStreamOpen(s, msg)
			}()
		}
	case protocol.TypeStreamAccept:
		m.mu.Lock()
		s, ok := m.streams[msg.SID]
		m.mu.Unlock()
		if ok {
			select {
			case s.verdict <- verdict{ok: true, info: AcceptInfo{Status: msg.Status, Headers: msg.Headers}}:
			default:
			}
		}
	case protocol.TypeStreamReject:
		m.mu.Lock()
		s, ok := m.streams[msg.SID]
		m.mu.Unlock()
		if ok {
			select {
			case s.verdict <- verdict{ok: false, info: AcceptInfo{Status: msg.Status}}:
			default:
			}
		}
	case protocol.TypeStreamEnd:
		m.mu.Lock()
		s, ok := m.streams[msg.SID]
		m.mu.Unlock()
		if !ok {
			return
		}
		s.signalRemoteEOF()
	case protocol.TypeStreamReset:
		// Peer aborted the stream in both directions (e.g. the phone hung
		// up mid-SSE). Tear it down immediately so the local handler's
		// blocked Read/Write/io.Copy return and its TCP conn is released.
		m.removeStream(msg.SID)
	default:
		// hello/ok/err are consumed outside the mux (handshake phase).
		m.log.Warn("mux: unexpected control type in stream phase", "type", msg.Type)
	}
}

func (m *Mux) handleData(data []byte) {
	sid, payload, err := protocol.DecodeData(data)
	if err != nil {
		m.log.Warn("mux: bad data frame", "err", err)
		return
	}
	m.mu.Lock()
	s, ok := m.streams[sid]
	m.mu.Unlock()
	if !ok {
		m.log.Debug("mux: data for unknown stream", "sid", sid)
		return
	}
	s.bytes.Add(int64(len(payload)))
	s.enqueueRecv(payload)
}

// enqueueRecv 把数据块追加到该流的接收队列。它永不阻塞——这是硬约束：
// 曾经这里用容量 5 帧的 chan 直接投递，慢消费者（公网手机下载 200KB+
// 响应）让 chan 满后阻塞整个 mux readLoop，所有其他流的 open/accept 全部
// 停摆（实测 /favicon.svg 首字节 25 秒、13 个请求同一毫秒一起释放）。
//
// 队列超限说明消费端长期不读。这里必须区分两种情况：
//   - 被遗弃的流（真正卡死）→ 重置该流，避免拖死隧道；
//   - 大响应 + 慢客户端（手机下载一个 12MB 的 JS bundle）→ 属于正常场景，
//     以前一视同仁地 Reset 会让浏览器拿到半个文件，表现为 "import failed"
//     且 HTTP 仍为 200（静默截断，极难排查）。因此上限可配置，并在溢出时
//     打一条 Warn（带 path/字节数），下次能从日志直接归因。
func (s *Stream) enqueueRecv(payload []byte) {
	s.recvMu.Lock()
	if s.recvClosed || s.recvEOF {
		s.recvMu.Unlock()
		return
	}
	if s.recvLimit > 0 && s.recvBytes+len(payload) > s.recvLimit {
		queued := s.recvBytes
		s.recvMu.Unlock()
		s.mux.log.Warn("mux: stream receive queue over limit, resetting stream",
			"sid", s.sid, "path", s.pathLabel(), "queued_bytes", queued, "limit", s.recvLimit)
		// 异步重置：避免在读循环里做可能阻塞的控制帧发送。
		go func() { _ = s.Reset() }()
		return
	}
	if total := s.mux.cfg.TotalRecvLimitBytes; total > 0 &&
		s.mux.recvTotal.Load()+int64(len(payload)) > int64(total) {
		queued := s.recvBytes
		s.recvMu.Unlock()
		s.mux.log.Warn("mux: mux-wide receive budget exhausted, resetting stream",
			"sid", s.sid, "path", s.pathLabel(), "queued_bytes", queued,
			"total_buffered", s.mux.recvTotal.Load(), "total_limit", total)
		go func() { _ = s.Reset() }()
		return
	}
	s.recvQueue = append(s.recvQueue, payload)
	s.recvBytes += len(payload)
	s.mux.recvTotal.Add(int64(len(payload)))
	s.recvCond.Signal()
	s.recvMu.Unlock()
}

// streamRecvLimit resolves the per-stream receive cap for this mux.
func (m *Mux) streamRecvLimit() int {
	if m.cfg.StreamRecvLimitBytes > 0 {
		return m.cfg.StreamRecvLimitBytes
	}
	return m.cfg.PerStreamBufferBytes * 8
}

// SID returns the stream id.
func (s *Stream) SID() uint32 { return s.sid }

// SetLabel attaches a human-readable label (usually the request path) used in
// lifecycle diagnostics.
func (s *Stream) SetLabel(label string) { s.label.Store(label) }

func (s *Stream) pathLabel() string {
	if v, ok := s.label.Load().(string); ok {
		return v
	}
	return ""
}

func (s *Stream) byteCount() int64 { return s.bytes.Load() }

// Read reads stream data, blocking under backpressure. It returns io.EOF
// once the peer half-closed (stream.end) and all buffered data is drained.
func (s *Stream) Read(p []byte) (int, error) {
	if len(s.pending) > 0 {
		n := copy(p, s.pending)
		s.pending = s.pending[n:]
		return n, nil
	}
	s.recvMu.Lock()
	for len(s.recvQueue) == 0 {
		if s.recvClosed {
			s.recvMu.Unlock()
			return 0, ErrClosed
		}
		if s.recvEOF {
			s.recvMu.Unlock()
			return 0, io.EOF
		}
		s.recvCond.Wait()
	}
	chunk := s.recvQueue[0]
	s.recvQueue[0] = nil // 允许底层数组被回收
	s.recvQueue = s.recvQueue[1:]
	s.recvBytes -= len(chunk)
	s.recvMu.Unlock()
	s.mux.recvTotal.Add(-int64(len(chunk)))

	n := copy(p, chunk)
	if n < len(chunk) {
		s.pending = append(s.pending[:0], chunk[n:]...)
	}
	s.touchRead()
	return n, nil
}

// Write sends stream data as binary frames, blocking under backpressure.
// Per-stream inflight quota keeps one fat stream (multi-MB response) from
// starving concurrent small streams.
func (s *Stream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > protocol.MaxFrameSize {
			n = protocol.MaxFrameSize
		}
		// 在途配额：超过 maxInflightFramesPerStream 帧在队列中则等
		// writeLoop 归还（大流自限速，小流畅通）。
		if s.inflight.Load() >= maxInflightFramesPerStream {
			select {
			case <-s.inflightCh:
			case <-s.closed:
				return written, ErrClosed
			case <-s.mux.done:
				return written, ErrClosed
			}
			continue
		}
		frame := acquireFrame(s.sid, p[:n])
		select {
		case <-s.closed:
			return written, ErrClosed
		default:
		}
		s.inflight.Add(1)
		if err := s.mux.enqueue(outFrame{data: frame, stream: s}); err != nil {
			s.inflight.Add(-1)
			return written, err
		}
		s.touchWrite()
		s.bytes.Add(int64(n))
		written += n
		p = p[n:]
	}
	return written, nil
}

// Accept replies stream.accept carrying the response head (tunnel side).
func (s *Stream) Accept(status int, headers map[string][]string) error {
	return s.mux.sendControl(protocol.Message{
		Type:    protocol.TypeStreamAccept,
		SID:     s.sid,
		Status:  status,
		Headers: headers,
	})
}

// Reject replies stream.reject with a status to the peer's stream.open.
func (s *Stream) Reject(status int) error {
	return s.mux.sendControl(protocol.Message{Type: protocol.TypeStreamReject, SID: s.sid, Status: status})
}

// CloseWrite sends stream.end (half-close) to the peer.
//
// ⚠️ 这个帧必须与同流的数据帧保持 FIFO 顺序：stream.end 承诺"之后不再发
// 数据"，如果它被优先队列提前到数据帧前面，对端会先看到 end、丢弃随后的
// payload → 表现为 short write / 读到空 body（实测 TestHalfClose）。因此
// 它走普通数据队列，而不是 sendControl 的高优先级队列。
func (s *Stream) CloseWrite() error {
	var err error
	s.localOnce.Do(func() {
		data, merr := protocol.Marshal(protocol.Message{Type: protocol.TypeStreamEnd, SID: s.sid})
		if merr != nil {
			err = merr
			return
		}
		err = s.mux.enqueue(outFrame{text: true, data: data})
	})
	if err != nil {
		return err
	}
	s.noteLocalEOF()
	return nil
}

// Close tears down the stream locally and unregisters it.
func (s *Stream) Close() error {
	s.mux.removeStream(s.sid)
	return nil
}

// Reset aborts the stream in both directions: it notifies the peer with
// stream.reset and tears the stream down locally. The peer's blocked
// Read/Write/io.Copy return immediately instead of hanging until an idle
// sweep that never fires for actively-streaming SSE responses. Safe to call
// even after Close; the peer ignores a reset for a stream it has already
// removed.
func (s *Stream) Reset() error {
	err := s.mux.sendControl(protocol.Message{Type: protocol.TypeStreamReset, SID: s.sid})
	s.mux.removeStream(s.sid)
	return err
}

// Done reports when the stream is torn down (closed locally, swept as idle,
// or the mux shut down). Consumers blocking on I/O can select on it to avoid
// hanging forever when the peer aborts a request without sending stream.end.
func (s *Stream) Done() <-chan struct{} { return s.closed }

func (s *Stream) touchRead()  { s.lastRead.Store(time.Now().UnixNano()) }
func (s *Stream) touchWrite() { s.lastWrite.Store(time.Now().UnixNano()) }

func (s *Stream) signalRemoteEOF() {
	s.remoteOnce.Do(func() {
		close(s.remoteEOF)
		s.recvMu.Lock()
		s.recvEOF = true
		s.recvCond.Broadcast()
		s.recvMu.Unlock()
		// If both directions have ended, the stream is dead: unregister.
		select {
		case <-s.localEOF:
			s.mux.removeStream(s.sid)
		default:
		}
	})
}

func (s *Stream) noteLocalEOF() {
	s.localOnce.Do(func() {
		close(s.localEOF)
		select {
		case <-s.remoteEOF:
			s.mux.removeStream(s.sid)
		default:
		}
	})
}

func (s *Stream) close() {
	s.signalRemoteEOF()
	s.closeOnce.Do(func() {
		close(s.closed)
		s.recvMu.Lock()
		s.recvClosed = true
		// Release this stream's share of the mux-wide receive budget; the
		// queue is dropped without ever being Read.
		if s.recvBytes > 0 {
			s.mux.recvTotal.Add(-int64(s.recvBytes))
			s.recvBytes = 0
		}
		s.recvQueue = nil
		s.recvCond.Broadcast()
		s.recvMu.Unlock()
	})
}

// tokenBucket is a simple blocking token bucket (bytes per second).
type tokenBucket struct {
	rate   float64
	burst  float64
	tokens float64
	mu     sync.Mutex
	last   time.Time
}

func newTokenBucket(bytesPerSec int64) *tokenBucket {
	b := &tokenBucket{
		rate:   float64(bytesPerSec),
		burst:  float64(bytesPerSec),
		tokens: float64(bytesPerSec),
		last:   time.Now(),
	}
	return b
}

func (b *tokenBucket) wait(n int) {
	if n <= 0 {
		return
	}
	for {
		b.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(b.last).Seconds()
		b.last = now
		b.tokens = min(b.burst, b.tokens+elapsed*b.rate)
		if b.tokens >= float64(n) {
			b.tokens -= float64(n)
			b.mu.Unlock()
			return
		}
		deficit := float64(n) - b.tokens
		b.mu.Unlock()
		time.Sleep(time.Duration(deficit / b.rate * float64(time.Second)))
	}
}
