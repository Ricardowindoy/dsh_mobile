package forwarder

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Duration is a JSON-friendly duration ("5s", "30s", "1m").
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

// PortSpec is one port the server exposes. The name must match a registration
// from the client; RemotePort is a server-owned resource (never client-chosen).
type PortSpec struct {
	Name       string `json:"name"`
	RemotePort int    `json:"remotePort"`
}

// ForwardSpec is one local port the client exposes under a server-owned name.
type ForwardSpec struct {
	Name      string `json:"name"`
	LocalAddr string `json:"localAddr"`
}

// RelayConfig mirrors the existing cmd/relay flags that the unified binary
// needs. It is a deliberate copy: the relay's own CLI keeps working unchanged.
type RelayConfig struct {
	PairingsFile string `json:"pairingsFile"`
	// DefaultNodeFile holds the id of the device root-path requests route to
	// when several devices are paired (managed by the remote-access plugin).
	// Optional; empty keeps the single-device heuristic only.
	DefaultNodeFile string `json:"defaultNodeFile,omitempty"`
	AccessTokenFile string `json:"accessTokenFile"`
	// PhoneDevicesFile enables per-device phone credentials (QR pairing,
	// hash-only roster, per-device revocation). Empty = legacy static-token
	// behaviour only. The AccessTokenFile secret doubles as the management
	// credential for pair mint/revoke/list when this is set.
	PhoneDevicesFile string `json:"phoneDevicesFile,omitempty"`
	// NotifyEnabled turns on the approval/question push plane (requires
	// PhoneDevicesFile + accessTokenFile). Default off.
	NotifyEnabled bool     `json:"notifyEnabled,omitempty"`
	UpdatesDir    string   `json:"updatesDir"`
	LogFile       string   `json:"logFile,omitempty"`
	Heartbeat     Duration `json:"heartbeat,omitempty"`
	MaxStreams    int      `json:"maxStreams,omitempty"`
	StreamBuffer  int      `json:"streamBuffer,omitempty"`
	// StreamRecvLimit is the hard per-stream receive-queue cap in bytes.
	// 0 keeps the relay default (streamBuffer*8). Public relays must raise it
	// so multi-MB bundles are not reset while a slow phone drains them.
	StreamRecvLimit int `json:"streamRecvLimit,omitempty"`
	// StreamRecvTotal bounds the mux-wide receive queue (0 = unlimited).
	StreamRecvTotal int      `json:"streamRecvTotal,omitempty"`
	IdleTimeout     Duration `json:"idleTimeout,omitempty"`
}

// TLSConfig describes one TLS-terminating public entrance.
type TLSConfig struct {
	Listen   string `json:"listen"`
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
	// StripRefererPaths lists request paths whose Referer header must be
	// removed before forwarding. This preserves the behaviour nginx provided
	// for /api/bili-duty (the plugin's same-origin fallback compares Referer
	// against the loopback Host and would otherwise 403).
	StripRefererPaths []string `json:"stripRefererPaths,omitempty"`
}

// ServerConfig is the ECS-side configuration for the unified forwarder.
type ServerConfig struct {
	// RelayTLS is the existing private-CA entrance (phone App + tunnel).
	RelayTLS TLSConfig `json:"relayTLS"`
	// PublicTLS is the new Let's-Encrypt entrance. When empty, only RelayTLS
	// is served (useful for a staged rollout).
	PublicTLS *TLSConfig  `json:"publicTLS,omitempty"`
	Relay     RelayConfig `json:"relay"`
	// PlainHTTPListen optionally keeps a loopback plain-HTTP listener so an
	// existing nginx vhost can be retired gradually. Empty = disabled.
	PlainHTTPListen string                  `json:"plainHTTPListen,omitempty"`
	PortForward     PortForwardServerConfig `json:"portForward"`
}

// PortForwardServerConfig configures the port-forward control + data plane.
type PortForwardServerConfig struct {
	ControlListen string     `json:"controlListen"`
	Token         string     `json:"token"`
	Ports         []PortSpec `json:"ports"`
	// ConnectDeadline bounds how long a pending public connection waits for
	// the client to open its data connection.
	ConnectDeadline Duration `json:"connectDeadline,omitempty"`
	// ReloadInterval bounds certificate reloading; defaults to 10 minutes.
	ReloadInterval Duration `json:"reloadInterval,omitempty"`
}

// ClientConfig is the in-house (behind-NAT) side configuration.
type ClientConfig struct {
	ServerAddr  string        `json:"serverAddr"`
	Token       string        `json:"token"`
	ClientID    string        `json:"clientId"`
	Forwards    []ForwardSpec `json:"forwards"`
	DialTimeout Duration      `json:"dialTimeout,omitempty"`
	RetryMin    Duration      `json:"retryMin,omitempty"`
	RetryMax    Duration      `json:"retryMax,omitempty"`
	TLSCertFile string        `json:"tlsCertFile,omitempty"` // optional private-CA pinning
}

// LoadServerConfig reads and validates a server config. All failures are hard
// errors: a misconfigured public entrance must never start half-open.
func LoadServerConfig(path string) (ServerConfig, error) {
	var c ServerConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read server config: %w", err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("parse server config: %w", err)
	}
	if err := c.validate(); err != nil {
		return c, err
	}
	return c, nil
}

func (c *ServerConfig) validate() error {
	if c.RelayTLS.Listen == "" {
		return fmt.Errorf("relayTLS.listen is required")
	}
	if c.RelayTLS.CertFile == "" || c.RelayTLS.KeyFile == "" {
		return fmt.Errorf("relayTLS cert/key are required")
	}
	if c.Relay.PairingsFile == "" {
		return fmt.Errorf("relay.pairingsFile is required")
	}
	if c.PortForward.ControlListen == "" {
		return fmt.Errorf("portForward.controlListen is required")
	}
	if c.PortForward.Token == "" {
		return fmt.Errorf("portForward.token is required")
	}
	if len(c.PortForward.Ports) == 0 {
		return fmt.Errorf("portForward.ports must not be empty")
	}
	seenName := map[string]bool{}
	seenPort := map[int]bool{}
	for _, p := range c.PortForward.Ports {
		if p.Name == "" {
			return fmt.Errorf("portForward.ports has an empty name")
		}
		if p.RemotePort <= 0 || p.RemotePort > 65535 {
			return fmt.Errorf("port %q remotePort %d out of range", p.Name, p.RemotePort)
		}
		if seenName[p.Name] {
			return fmt.Errorf("duplicate port name %q", p.Name)
		}
		if seenPort[p.RemotePort] {
			return fmt.Errorf("duplicate remotePort %d", p.RemotePort)
		}
		seenName[p.Name] = true
		seenPort[p.RemotePort] = true
	}
	if c.PublicTLS != nil {
		if c.PublicTLS.Listen == "" || c.PublicTLS.CertFile == "" || c.PublicTLS.KeyFile == "" {
			return fmt.Errorf("publicTLS requires listen, certFile and keyFile")
		}
	}
	return nil
}

// LoadClientConfig reads and validates a client config.
func LoadClientConfig(path string) (ClientConfig, error) {
	var c ClientConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read client config: %w", err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("parse client config: %w", err)
	}
	if err := c.validate(); err != nil {
		return c, err
	}
	return c, nil
}

func (c *ClientConfig) validate() error {
	if c.ServerAddr == "" {
		return fmt.Errorf("serverAddr is required")
	}
	if c.Token == "" {
		return fmt.Errorf("token is required")
	}
	if c.ClientID == "" {
		return fmt.Errorf("clientId is required")
	}
	if len(c.Forwards) == 0 {
		return fmt.Errorf("forwards must not be empty")
	}
	seen := map[string]bool{}
	for _, f := range c.Forwards {
		if f.Name == "" || f.LocalAddr == "" {
			return fmt.Errorf("forward requires name and localAddr")
		}
		if seen[f.Name] {
			return fmt.Errorf("duplicate forward name %q", f.Name)
		}
		seen[f.Name] = true
	}
	return nil
}
