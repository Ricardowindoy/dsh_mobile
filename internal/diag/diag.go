// Package diag holds the connection-lifecycle diagnostics shared by the
// tunnel (device side) and the relay (public side).
//
// Why this exists: both binaries log plenty of per-request detail, but the
// connection itself — dial, pairing handshake, drops, backoff, recovery —
// was only ever visible as a one-line "connection lost, reconnecting" with
// no reason, no uptime and no total recovery time. Those are exactly the
// facts needed to tell "the phone's network flaps" apart from "the relay
// kicked us" apart from "our own read deadline fired", and to explain why a
// reconnect sometimes takes tens of seconds.
//
// Design constraints:
//
//   - Diagnostic writes must never be able to kill the process or the
//     connection loop. Every failure path logs to stderr and continues.
//   - Each line carries its own wall-clock timestamp, so the file can be
//     read on its own (the remote relay's journal has its own clock).
//   - Lines are written one Write call each, so a single append is atomic
//     (O_APPEND) and the file can be safely grabbed while running.
//   - Size-capped with one rotation, so a long-lived tunnel cannot fill the
//     disk with diagnostics.
package diag

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Version identifies the lifecycle line format so a reader can tell which
// flavour of fields to expect. Bump it when fields are removed or renamed.
const Version = 1

// maxFileBytes caps a lifecycle log before it rotates (one .1 backup kept).
const maxFileBytes int64 = 8 << 20 // 8 MiB

// event prefixes emitted by both sides. They are documented here because the
// lifecycle file is not slog-formatted: it reads as
//
//	2026-10-14T22:47:01.123+08:00 tunnel-lost conn=home-dsh-3 ... reason=...
const (
	PrefixTunnelAttempt = "tunnel-attempt"
	PrefixTunnelLinked  = "tunnel-linked"
	PrefixTunnelLost    = "tunnel-lost"
	PrefixTunnelRetry   = "tunnel-retry"
	PrefixTunnelRecover = "tunnel-recover"
	PrefixDeviceLinked  = "device-linked"
	PrefixDeviceLost    = "device-lost"
	PrefixDeviceKick    = "device-kick"
)

// Store is the subset of the *slog.Logger surface the lifecycle logger needs.
// *slog.Logger satisfies it.
type Store interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

type nopStore struct{}

func (nopStore) Info(string, ...any) {}
func (nopStore) Warn(string, ...any) {}

// Logger writes lifecycle lines to a sink (usually a file) and to a secondary
// Store (the process's stderr logger), so the same fact shows up both in the
// journal and in the file the user uploads.
type Logger struct {
	mu     sync.Mutex
	w      io.Writer
	back   Store
	seen   map[string]struct{}
	header string
}

// New returns a Logger appending to w; back may be nil (records then only go
// to w). No header is written here: several components share one file (tunnel
// + one mux per connection), so the owner writes the file header once via
// WriteHeader and passes Header: false to the rest.
func New(w io.Writer, back Store, header string) *Logger {
	return NewWithOptions(w, back, Options{Header: header})
}

// Options configures a lifecycle logger.
type Options struct {
	// Header is written as a "# ..." line at the top of the sink.
	Header string
	// HeaderOnce suppresses the header (for shared sinks).
	HeaderOnce bool
}

// NewWithOptions builds a Logger; the header is emitted only when
// opts.HeaderOnce is true (or NewWithOptions is used with a header).
func NewWithOptions(w io.Writer, back Store, opts Options) *Logger {
	if back == nil {
		back = nopStore{}
	}
	l := &Logger{w: w, back: back, seen: make(map[string]struct{}), header: opts.Header}
	if w != nil && opts.HeaderOnce && opts.Header != "" {
		_, _ = fmt.Fprintf(w, "# dsh connection lifecycle log v%d — %s\n", Version, opts.Header)
	}
	return l
}

// WriteHeader emits the file header once (owner side).
func (l *Logger) WriteHeader() {
	if l == nil || l.w == nil || l.header == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.w, "# dsh connection lifecycle log v%d — %s\n", Version, l.header)
}

// Records writes one structured lifecycle line. Args are key/value pairs
// (odd length allowed, the last key gets an empty value).
func (l *Logger) Records(kind, msg string, args ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	seen := false
	if l.seen != nil {
		_, seen = l.seen[kind]
		l.seen[kind] = struct{}{}
	}
	w := l.w
	if w != nil {
		var b strings.Builder
		b.WriteString(time.Now().Format("2006-01-02T15:04:05.000-07:00"))
		b.WriteByte(' ')
		b.WriteString(kind)
		b.WriteByte(' ')
		b.WriteString(msg)
		for i := 0; i < len(args); i += 2 {
			b.WriteByte(' ')
			b.WriteString(fmt.Sprint(args[i]))
			b.WriteByte('=')
			if i+1 < len(args) {
				b.WriteString(fmt.Sprint(args[i+1]))
			}
		}
		b.WriteByte('\n')
		_, _ = io.WriteString(w, b.String())
	}
	l.mu.Unlock()

	switch kind {
	case PrefixTunnelLost, PrefixTunnelRetry:
		l.back.Warn(kind, args...)
	default:
		// Keep the journal readable during normal operation: only the first
		// occurrence of each kind goes to the process log, the rest stay in
		// the lifecycle file.
		if !seen {
			l.back.Info(kind, args...)
		}
	}
}

// FileWriter appends to path (creating parent directories) and rotates to
// path+".1" once when the file grows past maxFileBytes. It implements
// io.Writer so it can feed a *slog.Handler as well as the lifecycle Logger.
type FileWriter struct {
	path string
	f    *os.File
	size int64
}

// NewFileWriter opens (or creates) path for appending.
func NewFileWriter(path string) (*FileWriter, error) {
	fw := &FileWriter{path: path}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	if err := fw.open(); err != nil {
		return nil, err
	}
	return fw, nil
}

func (w *FileWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	size := int64(0)
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}
	w.f, w.size = f, size
	return nil
}

// Write appends p, rotating first when the file is already at the cap.
func (w *FileWriter) Write(p []byte) (int, error) {
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if w.size >= maxFileBytes {
		w.rotate()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *FileWriter) rotate() {
	_ = w.f.Close()
	_ = os.Rename(w.path, w.path+".1")
	if err := w.open(); err != nil {
		// Rotation must not take the process down: keep the old handle if
		// reopening failed, the next Write will surface the error.
		return
	}
}

// Close closes the underlying file.
func (w *FileWriter) Close() error {
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// LifecycleFromEnv returns the lifecycle log path from an env var, or ""
// when it is unset. Kept here so both binaries agree on the variable name.
func LifecycleFromEnv(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// LogFileName is the name of the tunnel lifecycle log inside its log dir.
const LogFileName = "lifecycle.log"
