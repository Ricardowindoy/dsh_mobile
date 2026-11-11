package mux

import (
	"bytes"
	"testing"
	"time"

	"dsh_mobile/internal/protocol"
)

// The pooled frame buffer must round-trip the sid+payload exactly, and two
// consecutive acquisitions must not alias each other after a release.
func TestFramePoolRoundTripAndNoAliasing(t *testing.T) {
	payload1 := bytes.Repeat([]byte("AB"), protocol.MaxFrameSize/2) // 16KB
	f1 := acquireFrame(7, payload1)
	if got := f1[:4]; string(got) != string([]byte{0, 0, 0, 7}) {
		t.Fatalf("sid header = %v, want [0 0 0 7]", got)
	}
	if !bytes.Equal(f1[4:], payload1) {
		t.Fatal("payload mismatch on first acquire")
	}
	sid, p, err := protocol.DecodeData(f1)
	if err != nil || sid != 7 || !bytes.Equal(p, payload1) {
		t.Fatalf("DecodeData = %d, %d bytes, %v", sid, len(p), err)
	}
	releaseFrame(f1)

	// Reacquire: the pool hands back the same buffer; the second frame must
	// fully overwrite the first payload's prefix.
	payload2 := bytes.Repeat([]byte("xy"), 16) // 32B, far shorter
	f2 := acquireFrame(9, payload2)
	if got := f2[:4]; string(got) != string([]byte{0, 0, 0, 9}) {
		t.Fatalf("second sid header = %v, want [0 0 0 9]", got)
	}
	if !bytes.Equal(f2[4:], payload2) {
		t.Fatal("payload mismatch on pooled reacquire (stale bytes?)")
	}
	releaseFrame(f2)
}

// End-to-end over a real websocket pair: a multi-frame response written
// through the pooled Write path must arrive byte-identical.
func TestPooledWritePathByteIntegrity(t *testing.T) {
	clientCfg := testCfg()
	serverCfg := testCfg()
	client, server, opened := pairWithOpen(t, clientCfg, serverCfg)
	defer client.Close()
	defer server.Close()

	s, err := client.OpenStream("GET", "/pool-integrity", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var ss *Stream
	select {
	case ss = <-opened:
	case <-time.After(2 * time.Second):
		t.Fatal("server stream not opened")
	}

	// Writer: several 16KB frames + one short tail, matching production shape.
	const chunk = protocol.MaxFrameSize
	want := bytes.Repeat([]byte{0xA5}, chunk*3+1234)
	go func() {
		for off := 0; off < len(want); {
			end := off + chunk
			if end > len(want) {
				end = len(want)
			}
			if _, werr := s.Write(want[off:end]); werr != nil {
				return
			}
			off = end
		}
		_ = s.CloseWrite()
	}()

	got := make([]byte, 0, len(want))
	buf := make([]byte, 32*1024)
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < len(want) && time.Now().Before(deadline) {
		n, err := ss.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("byte integrity lost: got %d bytes, want %d; first diff at %d",
			len(got), len(want), func() int {
				n := min(len(got), len(want))
				for i := 0; i < n; i++ {
					if got[i] != want[i] {
						return i
					}
				}
				return n
			}())
	}
}
