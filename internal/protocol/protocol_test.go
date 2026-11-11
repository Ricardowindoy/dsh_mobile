package protocol

import (
	"bytes"
	"testing"
)

func TestControlRoundtrip(t *testing.T) {
	msgs := []Message{
		Hello("dev-1", "secret"),
		OK(30000),
		Err("bad token"),
		{Type: TypeStreamOpen, SID: 42, Method: "GET", Path: "/", Headers: map[string][]string{"Host": {"x"}}, Upgrade: false},
		{Type: TypeStreamAccept, SID: 42},
		{Type: TypeStreamReject, SID: 42, Status: 502},
		{Type: TypeStreamEnd, SID: 42},
		{Type: TypePing},
		{Type: TypePong},
	}
	for _, m := range msgs {
		data, err := Marshal(m)
		if err != nil {
			t.Fatalf("marshal %q: %v", m.Type, err)
		}
		got, err := Unmarshal(data)
		if err != nil {
			t.Fatalf("unmarshal %q: %v", m.Type, err)
		}
		if got.Type != m.Type {
			t.Errorf("type = %q, want %q", got.Type, m.Type)
		}
		if got.SID != m.SID || got.Status != m.Status || got.Method != m.Method || got.Path != m.Path {
			t.Errorf("roundtrip mismatch for %q: %+v vs %+v", m.Type, got, m)
		}
		if m.DeviceID != "" && got.DeviceID != m.DeviceID {
			t.Errorf("deviceId = %q, want %q", got.DeviceID, m.DeviceID)
		}
	}
}

func TestUnmarshalUnknownType(t *testing.T) {
	_, err := Unmarshal([]byte(`{"type":"hack"}`))
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestUnmarshalBadJSON(t *testing.T) {
	if _, err := Unmarshal([]byte(`{`)); err == nil {
		t.Fatal("expected error for bad json")
	}
}

func TestDataFrameRoundtrip(t *testing.T) {
	payload := []byte("hello stream")
	frame, err := EncodeData(7, payload)
	if err != nil {
		t.Fatal(err)
	}
	sid, got, err := DecodeData(frame)
	if err != nil {
		t.Fatal(err)
	}
	if sid != 7 {
		t.Errorf("sid = %d, want 7", sid)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload = %q, want %q", got, payload)
	}
}

func TestDataFrameBoundary(t *testing.T) {
	// Exactly at the cap: fine.
	if _, err := EncodeData(1, make([]byte, MaxFrameSize)); err != nil {
		t.Fatalf("payload at cap rejected: %v", err)
	}
	// One over the cap: rejected.
	if _, err := EncodeData(1, make([]byte, MaxFrameSize+1)); err == nil {
		t.Fatal("expected oversize error")
	}
	// Decode side is deliberately more lenient than encode (rolling upgrades:
	// old peers may still emit 64KB frames). Up to MaxDecodeFrameSize is accepted.
	frame := append([]byte{0, 0, 0, 1}, make([]byte, MaxDecodeFrameSize)...)
	if _, payload, err := DecodeData(frame); err != nil {
		t.Fatalf("decode at MaxDecodeFrameSize rejected: %v", err)
	} else if len(payload) != MaxDecodeFrameSize {
		t.Fatalf("decoded payload = %d, want %d", len(payload), MaxDecodeFrameSize)
	}
	over := append([]byte{0, 0, 0, 1}, make([]byte, MaxDecodeFrameSize+1)...)
	if _, _, err := DecodeData(over); err == nil {
		t.Fatal("expected oversize decode error")
	}
	// 编码侧必须严格用 16KB 上限（这是本次性能修复的承重点）。
	if MaxFrameSize >= MaxDecodeFrameSize {
		t.Fatalf("MaxFrameSize(%d) must be smaller than MaxDecodeFrameSize(%d)", MaxFrameSize, MaxDecodeFrameSize)
	}
}

func TestDataFrameShortAndReserved(t *testing.T) {
	if _, _, err := DecodeData([]byte{0, 0, 0}); err == nil {
		t.Fatal("expected short-frame error")
	}
	if _, _, err := DecodeData([]byte{0, 0, 0, 0, 'x'}); err == nil {
		t.Fatal("expected reserved sid 0 error")
	}
}

func TestEncodeDataPreservesSID(t *testing.T) {
	frame, err := EncodeData(0xFFFFFFFF, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if sid, _, _ := DecodeData(frame); sid != 0xFFFFFFFF {
		t.Errorf("sid = %d, want 0xFFFFFFFF", sid)
	}
}
