package mux

import (
	"bytes"
	"testing"
	"time"

	"dsh_mobile/internal/protocol"
)

// pairWithOpen installs OnStreamOpen BEFORE the mux is built (the config is
// copied by value in New) and returns the server-side streams on a channel.
func pairWithOpen(t *testing.T, clientCfg, serverCfg Config) (*Mux, *Mux, chan *Stream) {
	t.Helper()
	opened := make(chan *Stream, 8)
	serverCfg.OnStreamOpen = func(s *Stream, _ protocol.Message) { opened <- s }
	client, server := pair(t, clientCfg, serverCfg)
	return client, server, opened
}

// acceptOne opens a stream from the client and returns the server-side Stream
// (the side that receives response bytes from a local server in production).
func acceptOne(t *testing.T, client *Mux, opened chan *Stream, path string) *Stream {
	t.Helper()
	if _, err := client.OpenStream("GET", path, nil, false); err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	select {
	case ss := <-opened:
		return ss
	case <-time.After(2 * time.Second):
		t.Fatal("server stream not opened")
		return nil
	}
}

func waitClosed(s *Stream, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		s.recvMu.Lock()
		closed := s.recvClosed
		s.recvMu.Unlock()
		if closed {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// The historical default (PerStreamBufferBytes*8) resets a stream that buffers
// a large payload. This is the behaviour that truncated 12 MB bundles: keep it
// covered so a regression is visible.
func TestStreamRecvLimitDefaultResetsLargeOverflow(t *testing.T) {
	clientCfg := testCfg()
	serverCfg := testCfg() // PerStreamBufferBytes 64KB -> default limit 512KB
	client, server, opened := pairWithOpen(t, clientCfg, serverCfg)
	defer client.Close()
	defer server.Close()

	ss := acceptOne(t, client, opened, "/big-default")
	ss.enqueueRecv(make([]byte, 600*1024)) // one chunk over the 512KB cap
	if !waitClosed(ss, 2*time.Second) {
		t.Fatal("default 512KB cap should reset a 600KB buffered chunk")
	}
}

// Raising StreamRecvLimitBytes must let a multi-MB response buffer fully, and
// reading it out must return the exact bytes and release the mux-wide budget.
func TestStreamRecvLimitRaisedAllowsLargeResponse(t *testing.T) {
	clientCfg := testCfg()
	serverCfg := testCfg()
	serverCfg.StreamRecvLimitBytes = 8 * 1024 * 1024
	serverCfg.TotalRecvLimitBytes = 16 * 1024 * 1024
	client, server, opened := pairWithOpen(t, clientCfg, serverCfg)
	defer client.Close()
	defer server.Close()

	ss := acceptOne(t, client, opened, "/big-raised")
	const chunks, chunkSize = 60, 64 * 1024 // ~3.75 MB
	sent := bytes.Repeat([]byte("0123456789abcdef"), chunkSize/16)
	for i := 0; i < chunks; i++ {
		ss.enqueueRecv(append([]byte(nil), sent...))
	}
	if waitClosed(ss, 200*time.Millisecond) {
		t.Fatal("raised limit must NOT reset a 3.75MB response")
	}
	if got := ss.mux.recvTotal.Load(); got != int64(chunks*chunkSize) {
		t.Fatalf("recvTotal = %d, want %d", got, chunks*chunkSize)
	}

	var total int
	buf := make([]byte, 32*1024)
	for total < chunks*chunkSize {
		n, err := ss.Read(buf)
		total += n
		if err != nil {
			t.Fatalf("Read after %d bytes: %v", total, err)
		}
	}
	if total != chunks*chunkSize {
		t.Fatalf("read %d bytes, want %d", total, chunks*chunkSize)
	}
	if got := ss.mux.recvTotal.Load(); got != 0 {
		t.Fatalf("recvTotal after drain = %d, want 0", got)
	}
}

// The mux-wide budget must bound memory when several large streams coexist:
// once exhausted, the offending stream is reset rather than growing forever.
func TestMuxTotalRecvBudgetResetsStream(t *testing.T) {
	clientCfg := testCfg()
	serverCfg := testCfg()
	serverCfg.StreamRecvLimitBytes = 4 * 1024 * 1024
	serverCfg.TotalRecvLimitBytes = 100 * 1024
	client, server, opened := pairWithOpen(t, clientCfg, serverCfg)
	defer client.Close()
	defer server.Close()

	a := acceptOne(t, client, opened, "/budget-a")
	b := acceptOne(t, client, opened, "/budget-b")
	a.enqueueRecv(make([]byte, 80*1024)) // under both caps
	b.enqueueRecv(make([]byte, 40*1024)) // 80+40 > 100KB budget -> reset b
	if !waitClosed(b, 2*time.Second) {
		t.Fatal("mux-wide budget exhaustion must reset the offending stream")
	}
	if waitClosed(a, 100*time.Millisecond) {
		t.Fatal("stream within budget must not be reset")
	}
}

// close() must release a stream's unread bytes from the mux-wide counter,
// otherwise a few aborted downloads would permanently shrink the budget.
func TestStreamCloseReleasesRecvBudget(t *testing.T) {
	clientCfg := testCfg()
	serverCfg := testCfg()
	serverCfg.StreamRecvLimitBytes = 4 * 1024 * 1024
	serverCfg.TotalRecvLimitBytes = 16 * 1024 * 1024
	client, server, opened := pairWithOpen(t, clientCfg, serverCfg)
	defer client.Close()
	defer server.Close()

	ss := acceptOne(t, client, opened, "/release")
	ss.enqueueRecv(make([]byte, 64*1024))
	if got := ss.mux.recvTotal.Load(); got != 64*1024 {
		t.Fatalf("recvTotal = %d, want 65536", got)
	}
	_ = ss.Reset()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ss.mux.recvTotal.Load() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("recvTotal after close = %d, want 0", ss.mux.recvTotal.Load())
}
