package forwarder

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// ---------- frame encoding ----------

func TestFrameRoundTrip(t *testing.T) {
	cases := []Frame{
		Hello("tok", "dev-1"),
		OK(),
		Err("bad token"),
		Register("hindsight", "127.0.0.1:8888"),
		Registered("hindsight", 18888),
		Connect("abc123", "hindsight", 10000),
		Data("abc123"),
		Ping(), Pong(),
	}
	for _, want := range cases {
		data, err := Encode(want)
		if err != nil {
			t.Fatalf("Encode(%s): %v", want.Type, err)
		}
		got, err := Decode(data)
		if err != nil {
			t.Fatalf("Decode(%s): %v", want.Type, err)
		}
		if got != want {
			t.Errorf("round trip: got %+v want %+v", got, want)
		}
	}
}

func TestEncodeRejectsUnknownType(t *testing.T) {
	if _, err := Encode(Frame{Type: "explode"}); err == nil {
		t.Error("Encode must reject unknown type")
	}
	if _, err := Decode([]byte(`{"t":"explode"}`)); err == nil {
		t.Error("Decode must reject unknown type")
	}
	if _, err := Decode([]byte(`not json`)); err == nil {
		t.Error("Decode must reject malformed JSON")
	}
}

func TestWriteReadFrame(t *testing.T) {
	var buf bytes.Buffer
	want := Connect("deadbeef", "gw", 5000)
	if err := WriteFrame(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %+v want %+v", got, want)
	}
	if _, err := ReadFrame(&buf); err != io.EOF {
		t.Errorf("expected EOF at clean boundary, got %v", err)
	}
}

func TestReadFrameRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	var head [4]byte
	binary.BigEndian.PutUint32(head[:], MaxFrameBytes+1)
	buf.Write(head[:])
	if _, err := ReadFrame(&buf); err == nil || !strings.Contains(err.Error(), "exceeds cap") {
		t.Errorf("oversize frame must fail before allocating, got %v", err)
	}
}

func TestReadFrameRejectsTruncated(t *testing.T) {
	var buf bytes.Buffer
	var head [4]byte
	binary.BigEndian.PutUint32(head[:], 10)
	buf.Write(head[:])
	buf.WriteString("{}")
	if _, err := ReadFrame(&buf); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Errorf("truncated frame must error (not EOF), got %v", err)
	}
}

func TestReadDataID(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteFrame(&buf, Data("id-1"))
	if id, err := ReadDataID(&buf); err != nil || id != "id-1" {
		t.Fatalf("ReadDataID = %q, %v", id, err)
	}
	buf.Reset()
	_ = WriteFrame(&buf, Ping())
	if _, err := ReadDataID(&buf); err == nil {
		t.Error("ReadDataID must reject non-data frame")
	}
	buf.Reset()
	_ = WriteFrame(&buf, Frame{Type: TypeData}) // missing id
	if _, err := ReadDataID(&buf); err == nil {
		t.Error("ReadDataID must reject empty id")
	}
}

// ---------- config validation ----------

func TestServerConfigValidation(t *testing.T) {
	valid := ServerConfig{
		RelayTLS:    TLSConfig{Listen: ":24000", CertFile: "c", KeyFile: "k"},
		Relay:       RelayConfig{PairingsFile: "p"},
		PortForward: PortForwardServerConfig{ControlListen: ":7782", Token: "t", Ports: []PortSpec{{Name: "a", RemotePort: 18888}}},
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	mutations := []struct {
		name string
		fn   func(*ServerConfig)
	}{
		{"no relay listen", func(c *ServerConfig) { c.RelayTLS.Listen = "" }},
		{"no cert", func(c *ServerConfig) { c.RelayTLS.CertFile = "" }},
		{"no pairings", func(c *ServerConfig) { c.Relay.PairingsFile = "" }},
		{"no control listen", func(c *ServerConfig) { c.PortForward.ControlListen = "" }},
		{"no token", func(c *ServerConfig) { c.PortForward.Token = "" }},
		{"no ports", func(c *ServerConfig) { c.PortForward.Ports = nil }},
		{"bad port", func(c *ServerConfig) { c.PortForward.Ports[0].RemotePort = 70000 }},
		{"empty name", func(c *ServerConfig) { c.PortForward.Ports[0].Name = "" }},
		{"duplicate port", func(c *ServerConfig) {
			c.PortForward.Ports = append(c.PortForward.Ports, PortSpec{Name: "b", RemotePort: 18888})
		}},
		{"duplicate name", func(c *ServerConfig) {
			c.PortForward.Ports = append(c.PortForward.Ports, PortSpec{Name: "a", RemotePort: 18889})
		}},
		{"publicTLS incomplete", func(c *ServerConfig) {
			c.PublicTLS = &TLSConfig{Listen: ":24010"}
		}},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			c := valid
			m.fn(&c)
			if err := c.validate(); err == nil {
				t.Error("invalid config was accepted")
			}
		})
	}
}

func TestClientConfigValidation(t *testing.T) {
	valid := ClientConfig{
		ServerAddr: "relay.example.com:7782", Token: "t", ClientID: "home",
		Forwards: []ForwardSpec{{Name: "a", LocalAddr: "127.0.0.1:8888"}},
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for _, m := range []struct {
		name string
		fn   func(*ClientConfig)
	}{
		{"no server", func(c *ClientConfig) { c.ServerAddr = "" }},
		{"no token", func(c *ClientConfig) { c.Token = "" }},
		{"no clientId", func(c *ClientConfig) { c.ClientID = "" }},
		{"no forwards", func(c *ClientConfig) { c.Forwards = nil }},
		{"empty local", func(c *ClientConfig) { c.Forwards[0].LocalAddr = "" }},
		{"dup name", func(c *ClientConfig) {
			c.Forwards = append(c.Forwards, ForwardSpec{Name: "a", LocalAddr: "x"})
		}},
	} {
		t.Run(m.name, func(t *testing.T) {
			c := valid
			m.fn(&c)
			if err := c.validate(); err == nil {
				t.Error("invalid config was accepted")
			}
		})
	}
}

func TestDurationJSON(t *testing.T) {
	var c PortForwardServerConfig
	data := []byte(`{"controlListen":":1","token":"t","ports":[{"name":"a","remotePort":1}],"connectDeadline":"7s"}`)
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if c.ConnectDeadline.D() != 7*time.Second {
		t.Errorf("duration = %v, want 7s", c.ConnectDeadline.D())
	}
	bad := []byte(`{"controlListen":":1","token":"t","ports":[{"name":"a","remotePort":1}],"connectDeadline":"nope"}`)
	if err := json.Unmarshal(bad, &c); err == nil {
		t.Error("invalid duration must fail")
	}
}
