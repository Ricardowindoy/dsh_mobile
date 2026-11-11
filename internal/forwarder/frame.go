package forwarder

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// MaxFrameBytes caps one control frame. Control frames are tiny; a peer that
// sends more is either broken or hostile.
const MaxFrameBytes = 16 * 1024

// WriteFrame writes a uint32 big-endian length prefix followed by the frame.
func WriteFrame(w io.Writer, f Frame) error {
	data, err := Encode(f)
	if err != nil {
		return err
	}
	if len(data) > MaxFrameBytes {
		return fmt.Errorf("forwarder: frame %d bytes exceeds cap %d", len(data), MaxFrameBytes)
	}
	var head [4]byte
	binary.BigEndian.PutUint32(head[:], uint32(len(data)))
	if _, err := w.Write(head[:]); err != nil {
		return fmt.Errorf("forwarder: write frame head: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("forwarder: write frame body: %w", err)
	}
	return nil
}

// ReadFrame reads one length-prefixed frame. It rejects oversize frames before
// allocating, and returns io.EOF only when the stream ends cleanly at a frame
// boundary (a truncated frame is an error, not EOF).
func ReadFrame(r io.Reader) (Frame, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(head[:])
	if n == 0 {
		return Frame{}, fmt.Errorf("forwarder: empty frame")
	}
	if n > MaxFrameBytes {
		return Frame{}, fmt.Errorf("forwarder: frame %d bytes exceeds cap %d", n, MaxFrameBytes)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Frame{}, fmt.Errorf("forwarder: truncated frame: %w", err)
	}
	return Decode(buf)
}

// ReadDataID reads the first frame on a data connection and requires it to be
// a `data` frame carrying a server-issued id. Anything else is rejected.
func ReadDataID(r io.Reader) (string, error) {
	f, err := ReadFrame(r)
	if err != nil {
		return "", err
	}
	if f.Type != TypeData || f.ID == "" {
		return "", fmt.Errorf("forwarder: expected data frame with id, got %q", f.Type)
	}
	return f.ID, nil
}

// EncodeJSON is a small helper used by config loading (kept here so both
// binaries share the same JSON discipline).
func EncodeJSON(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }
