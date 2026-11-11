// Package forwarder implements a self-contained reverse TCP port forwarder:
// a public server owns the listening ports, and an in-house client behind NAT
// dials out to register the ports it wants exposed. It intentionally uses a
// separate control connection plus one data connection per forwarded TCP
// connection (phase one), keeping arbitrary TCP traffic OFF the existing
// dsh_mobile mux until that mux gains windowed backpressure.
//
// Wire protocol (control connection, length-prefixed JSON text frames):
//
//	client -> server: {"t":"hello","token":"...","clientId":"..."}
//	server -> client: {"t":"ok"} | {"t":"err","reason":"..."}
//	client -> server: {"t":"register","name":"...","localAddr":"..."}
//	server -> client: {"t":"registered","name":"...","remotePort":N}
//	             or  {"t":"err","reason":"..."}
//	server -> client: {"t":"connect","id":"<nonce>","name":"...","deadlineMs":N}
//	             (server has accepted a public TCP connection for `name`)
//	client -> server (on a NEW data connection): {"t":"data","id":"<nonce>"}
//	             then raw bytes follow for the lifetime of that connection.
//	either  -> other: {"t":"ping"} / {"t":"pong"}
//
// The id is issued by the server (crypto/rand) and bound to the authenticated
// control session; a client cannot pick its own id. Pending ids are GC'd after
// a short TTL so a client that never opens the data connection does not leak.
package forwarder

import (
	"encoding/json"
	"fmt"
)

// Frame is one control-plane message. Unknown types are rejected, never
// silently ignored.
type Frame struct {
	Type       string `json:"t"`
	Token      string `json:"token,omitempty"`
	ClientID   string `json:"clientId,omitempty"`
	Name       string `json:"name,omitempty"`
	LocalAddr  string `json:"localAddr,omitempty"`
	ID         string `json:"id,omitempty"`
	RemotePort int    `json:"remotePort,omitempty"`
	DeadlineMs int64  `json:"deadlineMs,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

const (
	TypeHello      = "hello"
	TypeOK         = "ok"
	TypeErr        = "err"
	TypeRegister   = "register"
	TypeRegistered = "registered"
	TypeConnect    = "connect"
	TypeData       = "data"
	TypePing       = "ping"
	TypePong       = "pong"
)

// Encode marshals one frame. It rejects unknown frame types (fail closed).
func Encode(f Frame) ([]byte, error) {
	switch f.Type {
	case TypeHello, TypeOK, TypeErr, TypeRegister, TypeRegistered,
		TypeConnect, TypeData, TypePing, TypePong:
	default:
		return nil, fmt.Errorf("forwarder: unknown frame type %q", f.Type)
	}
	return json.Marshal(f)
}

// Decode parses one frame. It rejects unknown types and malformed JSON.
func Decode(data []byte) (Frame, error) {
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return Frame{}, fmt.Errorf("forwarder: bad frame: %w", err)
	}
	switch f.Type {
	case TypeHello, TypeOK, TypeErr, TypeRegister, TypeRegistered,
		TypeConnect, TypeData, TypePing, TypePong:
		return f, nil
	default:
		return Frame{}, fmt.Errorf("forwarder: unknown frame type %q", f.Type)
	}
}

// Hello / OK / Err / Register / Registered / Connect / Data helpers keep
// call sites from hand-building frames with typo-prone field names.

func Hello(token, clientID string) Frame {
	return Frame{Type: TypeHello, Token: token, ClientID: clientID}
}
func OK() Frame        { return Frame{Type: TypeOK} }
func Err(reason string) Frame { return Frame{Type: TypeErr, Reason: reason} }
func Register(name, localAddr string) Frame {
	return Frame{Type: TypeRegister, Name: name, LocalAddr: localAddr}
}
func Registered(name string, remotePort int) Frame {
	return Frame{Type: TypeRegistered, Name: name, RemotePort: remotePort}
}
func Connect(id, name string, deadlineMs int64) Frame {
	return Frame{Type: TypeConnect, ID: id, Name: name, DeadlineMs: deadlineMs}
}
func Data(id string) Frame { return Frame{Type: TypeData, ID: id} }
func Ping() Frame         { return Frame{Type: TypePing} }
func Pong() Frame         { return Frame{Type: TypePong} }
