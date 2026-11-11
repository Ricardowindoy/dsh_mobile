package forwarder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"sync"
	"time"
)

// Client is the in-house (behind-NAT) side. It maintains one authenticated
// control connection and, on each `connect` frame, opens ONE new data
// connection to the server and dials the local target. Arbitrary TCP bytes
// never touch the dsh_mobile mux (phase-one isolation).
type Client struct {
	cfg ClientConfig
	log *slog.Logger

	mu      sync.Mutex
	conn    net.Conn
	send    chan Frame
	done    chan struct{}
	closed  bool
	closeOne sync.Once
}

// NewClient builds a client. Call Run to start it.
func NewClient(cfg ClientConfig, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{cfg: cfg, log: log, done: make(chan struct{})}
}

// Run dials, registers, and reconnects with exponential backoff until ctx is
// cancelled. A stable connection resets the backoff (3s stability rule).
func (c *Client) Run(ctx context.Context) error {
	minD := c.cfg.RetryMin.D()
	maxD := c.cfg.RetryMax.D()
	if minD <= 0 {
		minD = time.Second
	}
	if maxD <= 0 {
		maxD = 30 * time.Second
	}
	backoff := minD
	for {
		select {
		case <-ctx.Done():
			c.Close()
			return ctx.Err()
		case <-c.done:
			return nil
		default:
		}
		started := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			c.Close()
			return ctx.Err()
		}
		c.log.Warn("forwarder: control session ended, reconnecting", "err", err, "backoff", backoff)
		if time.Since(started) > 3*time.Second {
			backoff = minD
		}
		jitter := time.Duration(rand.Int63n(int64(backoff / 2)))
		select {
		case <-ctx.Done():
			c.Close()
			return ctx.Err()
		case <-time.After(backoff + jitter):
		}
		if backoff < maxD {
			backoff *= 2
			if backoff > maxD {
				backoff = maxD
			}
		}
	}
}

func (c *Client) session(ctx context.Context) error {
	dialTimeout := c.cfg.DialTimeout.D()
	if dialTimeout <= 0 {
		dialTimeout = 10 * time.Second
	}
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", c.cfg.ServerAddr)
	if err != nil {
		return fmt.Errorf("dial control %s: %w", c.cfg.ServerAddr, err)
	}
	defer conn.Close()

	send := make(chan Frame, 64)
	c.mu.Lock()
	c.conn = conn
	c.send = send
	c.mu.Unlock()

	if err := WriteFrame(conn, Hello(c.cfg.Token, c.cfg.ClientID)); err != nil {
		return err
	}
	f, err := ReadFrame(conn)
	if err != nil {
		return fmt.Errorf("read hello reply: %w", err)
	}
	if f.Type != TypeOK {
		return fmt.Errorf("server rejected hello: %s", f.Reason)
	}
	for _, fw := range c.cfg.Forwards {
		if err := WriteFrame(conn, Register(fw.Name, fw.LocalAddr)); err != nil {
			return err
		}
	}
	c.log.Info("forwarder: control session established", "server", c.cfg.ServerAddr, "forwards", len(c.cfg.Forwards))

	var wg sync.WaitGroup
	sessDone := make(chan struct{})
	errCh := make(chan error, 3)
	wg.Add(3)
	go func() { defer wg.Done(); errCh <- c.readLoop(conn, ctx, send, sessDone) }()
	go func() { defer wg.Done(); errCh <- c.writeLoop(conn, send, sessDone) }()
	go func() { defer wg.Done(); errCh <- c.heartbeatLoop(send, sessDone) }()
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-c.done:
			_ = conn.Close()
		case <-sessDone:
		}
	}()

	var first error
	for i := 0; i < 3; i++ {
		if e := <-errCh; e != nil && first == nil {
			first = e
		}
	}
	// One loop exited (read/write/heartbeat); tear down the whole session so
	// no goroutine from this generation leaks into the next reconnect.
	close(sessDone)
	_ = conn.Close()
	wg.Wait()
	return first
}

// heartbeatLoop sends a ping every 30s. The server's read-side liveness
// timeout is 90s (3x), so one or two lost pings do not kill the session.
func (c *Client) heartbeatLoop(send chan Frame, sessDone <-chan struct{}) error {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return nil
		case <-sessDone:
			return nil
		case <-t.C:
			select {
			case send <- Ping():
			default:
			}
		}
	}
}

func (c *Client) writeLoop(conn net.Conn, send <-chan Frame, sessDone <-chan struct{}) error {
	for {
		select {
		case <-c.done:
			return nil
		case <-sessDone:
			return nil
		case f := <-send:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := WriteFrame(conn, f); err != nil {
				return fmt.Errorf("control write: %w", err)
			}
		}
	}
}

func (c *Client) readLoop(conn net.Conn, ctx context.Context, send chan Frame, sessDone <-chan struct{}) error {
	for {
		f, err := ReadFrame(conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("control closed by server")
			}
			return fmt.Errorf("control read: %w", err)
		}
		switch f.Type {
		case TypePing:
			select {
			case send <- Pong():
			default:
			}
		case TypePong:
			// liveness observed
		case TypeRegistered:
			c.log.Info("forwarder: port registered", "name", f.Name, "remotePort", f.RemotePort)
		case TypeErr:
			c.log.Warn("forwarder: server error", "reason", f.Reason)
		case TypeConnect:
			// The data connection is opened in its own goroutine so a slow
			// local dial never blocks the control read loop.
			go c.handleConnect(ctx, f.ID, f.Name, time.Duration(f.DeadlineMs)*time.Millisecond)
		default:
			c.log.Warn("forwarder: unexpected control frame", "type", f.Type)
		}
	}
}

// handleConnect opens a NEW data connection to the server, identifies it with
// the server-issued id, dials the local target, and joins the two with
// half-close. Any failure (local dial refused, timeout) just closes both ends
// — the public peer sees a clean connection close, never a hung socket.
func (c *Client) handleConnect(ctx context.Context, id, name string, deadline time.Duration) {
	if id == "" {
		return
	}
	local := ""
	for _, f := range c.cfg.Forwards {
		if f.Name == name {
			local = f.LocalAddr
			break
		}
	}
	if local == "" {
		c.log.Warn("forwarder: connect for unknown forward name", "name", name)
		return
	}
	if deadline <= 0 {
		deadline = 10 * time.Second
	}

	dialCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	d := net.Dialer{Timeout: deadline}
	serverConn, err := d.DialContext(dialCtx, "tcp", c.cfg.ServerAddr)
	if err != nil {
		c.log.Warn("forwarder: data dial to server failed", "id", id, "err", err)
		return
	}
	// Identify this data connection BEFORE any payload.
	if err := WriteFrame(serverConn, Data(id)); err != nil {
		_ = serverConn.Close()
		return
	}
	localConn, err := d.DialContext(dialCtx, "tcp", local)
	if err != nil {
		c.log.Warn("forwarder: local dial failed", "name", name, "local", local, "id", id, "err", err)
		_ = serverConn.Close()
		return
	}
	joinHalfClose(serverConn, localConn)
}

// Close stops reconnection and drops the live control connection.
func (c *Client) Close() {
	c.closeOne.Do(func() {
		c.mu.Lock()
		c.closed = true
		conn := c.conn
		c.mu.Unlock()
		close(c.done)
		if conn != nil {
			_ = conn.Close()
		}
	})
}
