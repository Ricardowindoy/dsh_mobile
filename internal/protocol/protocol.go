// Package protocol defines the shared wire protocol (v1) spoken between the
// dsh-tunnel device connector and the public relay service.
//
// Device <-> Relay runs over a single WebSocket connection and is
// multiplexed:
//
//   - Control frames: text frames carrying JSON messages
//     (hello / ok / err / stream.open / stream.accept / stream.reject /
//     stream.end / ping / pong).
//   - Data frames: binary frames of the form
//     [4-byte big-endian streamId][payload], payload capped at 64KB.
//
// stream.end is a half-close: the sender promises not to send further data
// on that stream; the receiver keeps its own send direction open until it,
// too, sends stream.end.
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
)

const (
	// Version is the wire protocol version spoken by both ends.
	Version = 1

	// MaxFrameSize is the payload cap of a data frame we emit (16KB).
	// 小帧缩短单帧占用 mux 写循环的时间，让 stream.accept 等控制帧更快
	// 插入——64KB 帧在弱网（约 1Mbps 上行）下要占 0.5s，队列一积压就是
	// 秒级首字节（实测 relay→tunnel 写方向排队 5~14s）。
	MaxFrameSize = 16 * 1024

	// MaxDecodeFrameSize is the largest payload we accept from a peer.
	// 保持旧上限 64KB：滚动升级时新旧两端混跑（新编码 16KB / 旧解码 64KB、
	// 旧编码 64KB / 新解码 64KB）都能互通，不会因帧上限不一致断流。
	MaxDecodeFrameSize = 64 * 1024

	// DefaultHeartbeatMs is the default application-level heartbeat interval.
	DefaultHeartbeatMs = 30_000
)

// Control frame types.
const (
	TypeHello        = "hello"
	TypeOK           = "ok"
	TypeErr          = "err"
	TypeStreamOpen   = "stream.open"
	TypeStreamAccept = "stream.accept"
	TypeStreamReject = "stream.reject"
	TypeStreamEnd    = "stream.end"
	// TypeStreamReset aborts a stream in both directions. Unlike
	// stream.end (a half-close), reset tells the peer to tear down the
	// stream immediately: its blocked Read/Write return at once instead
	// of hanging until an idle sweep that never fires for actively
	// streaming SSE responses.
	TypeStreamReset = "stream.reset"
	TypePing        = "ping"
	TypePong        = "pong"
)

// Message is a JSON control frame. Fields are optional per message type.
type Message struct {
	Type string `json:"type"`

	// hello
	V        int    `json:"v,omitempty"`
	DeviceID string `json:"deviceId,omitempty"`
	Token    string `json:"token,omitempty"`

	// ok / err
	HeartbeatMs int    `json:"heartbeatMs,omitempty"`
	Reason      string `json:"reason,omitempty"`

	// stream.*
	SID     uint32      `json:"sid,omitempty"`
	Method  string      `json:"method,omitempty"`
	Path    string      `json:"path,omitempty"`
	Headers http.Header `json:"headers,omitempty"`
	Upgrade bool        `json:"upgrade,omitempty"`
	Status  int         `json:"status,omitempty"`
}

// Marshal encodes a control message as JSON.
func Marshal(m Message) ([]byte, error) {
	return json.Marshal(m)
}

// Unmarshal decodes a JSON control message and rejects unknown types.
func Unmarshal(data []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return Message{}, fmt.Errorf("protocol: bad control frame: %w", err)
	}
	switch m.Type {
	case TypeHello, TypeOK, TypeErr, TypeStreamOpen, TypeStreamAccept,
		TypeStreamReject, TypeStreamEnd, TypeStreamReset, TypePing, TypePong:
	default:
		return Message{}, fmt.Errorf("protocol: unknown control frame type %q", m.Type)
	}
	return m, nil
}

// EncodeData wraps payload into a binary data frame:
// [4-byte big-endian streamId][payload].
func EncodeData(sid uint32, payload []byte) ([]byte, error) {
	if len(payload) > MaxFrameSize {
		return nil, fmt.Errorf("protocol: payload %d exceeds frame cap %d", len(payload), MaxFrameSize)
	}
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame, sid)
	copy(frame[4:], payload)
	return frame, nil
}

// DecodeData splits a binary data frame into streamId and payload.
func DecodeData(frame []byte) (uint32, []byte, error) {
	if len(frame) < 4 {
		return 0, nil, fmt.Errorf("protocol: data frame too short (%d bytes)", len(frame))
	}
	sid := binary.BigEndian.Uint32(frame[:4])
	payload := frame[4:]
	if len(payload) > MaxDecodeFrameSize {
		return 0, nil, fmt.Errorf("protocol: payload %d exceeds frame cap %d", len(payload), MaxDecodeFrameSize)
	}
	if sid == 0 {
		return 0, nil, fmt.Errorf("protocol: streamId 0 is reserved")
	}
	return sid, payload, nil
}

// Hello builds the hello control message.
func Hello(deviceID, token string) Message {
	return Message{Type: TypeHello, V: Version, DeviceID: deviceID, Token: token}
}

// OK builds the ok control message.
func OK(heartbeatMs int) Message {
	return Message{Type: TypeOK, HeartbeatMs: heartbeatMs}
}

// Err builds the err control message.
func Err(reason string) Message {
	return Message{Type: TypeErr, Reason: reason}
}
