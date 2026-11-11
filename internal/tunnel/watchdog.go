package tunnel

// Link-readiness watchdog: "the mux is up" is not "the phone can reach the
// GUI". A tunnel process can hold a live mux while the public entrance in
// front of the relay is dead (DNS gone, TLS cert expired, forwarder wedged) —
// the exact failure class the upstream project hit in issue #1723, and the
// same class as our own forwarder "fake-alive" deadlock. The watchdog probes
// the REAL path on a cadence and escalates:
//
//	tick: GET <publicBase>/__dsh_tunnel__/probe   (entrance → relay)
//	      expect HTTP 200 with {"ok":true}; 401/403 count as ALIVE (the
//	      request reached an authenticating server — the link works).
//	2 consecutive failures: switch to the next configured relay endpoint
//	      (the dial loop picks it up on the next reconnect) and force a
//	      reconnect by closing the current mux.
//	2 more consecutive failures after the switch: exit the process non-zero
//	      and let systemd Restart=always bring it back — a wedged process
//	      must not be respawned in-place; one supervisor owns the lifecycle.
//
// Everything is injectable (prober, clock cadence, exit func) so tests drive
// the whole ladder without network or process suicide.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"dsh_mobile/internal/diag"
)

// loadCAPEM reads a PEM bundle into a cert pool (per-watchdog explicit pin;
// the process-wide alternative is SSL_CERT_FILE, which Go's default pool
// already honours).
func loadCAPEM(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no PEM certificates in %s", path)
	}
	return pool, nil
}

// defaultWatchdogExit terminates the process so systemd Restart=always owns
// the respawn. A variable so tests never actually exit.
var defaultWatchdogExit = func(code int) { os.Exit(code) }

// WatchdogConfig tunes the watchdog. Zero values keep it OFF: the tunnel's
// behaviour is byte-for-byte unchanged unless a PublicProbeURL is set.
type WatchdogConfig struct {
	// PublicProbeURL is the full URL of the relay probe THROUGH the public
	// entrance, e.g. https://dsh.example.com/__dsh_tunnel__/probe. Empty
	// disables the watchdog.
	PublicProbeURL string
	// Interval between probes (default 60s).
	Interval time.Duration
	// Timeout per probe (default 10s).
	Timeout time.Duration
	// FailuresBeforeSwitch / FailuresBeforeExit are the escalation steps
	// (defaults 2 / 2 more after the switch).
	FailuresBeforeSwitch int
	FailuresBeforeExit   int
	// CAFile pins a private CA for an IP-literal entrance (SSL_CERT_FILE is
	// the process-wide alternative; this is per-watchdog explicit).
	CAFile string
}

// watchdogState is the running ladder; one goroutine owns it.
type watchdogState struct {
	cfg            WatchdogConfig
	probe          func(ctx context.Context) (alive bool, detail string)
	switchHost     func() (string, bool)
	forceReconnect func()
	exit           func(code int)
	consecutive    int
	switched       bool
}

func defaultsWatchdog(c WatchdogConfig) WatchdogConfig {
	if c.Interval <= 0 {
		c.Interval = 60 * time.Second
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Second
	}
	if c.FailuresBeforeSwitch <= 0 {
		c.FailuresBeforeSwitch = 2
	}
	if c.FailuresBeforeExit <= 0 {
		c.FailuresBeforeExit = 2
	}
	return c
}

// httpProber builds the production prober. TLS follows the same rule as the
// tunnel dialer: an explicit CA file pins the private root; otherwise the
// system pool (the deployment exports SSL_CERT_FILE for the IP entrance).
func httpProber(cfg WatchdogConfig) func(context.Context) (bool, string) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		pool, err := loadCAPEM(cfg.CAFile)
		if err != nil {
			return func(context.Context) (bool, string) {
				return false, "ca load: " + err.Error()
			}
		}
		tlsCfg.RootCAs = pool
	}
	client := &http.Client{
		Timeout: cfg.Timeout,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
			// A probe must not ride the environment proxy: the point is to
			// test the public entrance the phone uses, from this machine's
			// real egress.
			Proxy: nil,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return func(ctx context.Context) (bool, string) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.PublicProbeURL, nil)
		if err != nil {
			return false, "request: " + err.Error()
		}
		resp, err := client.Do(req)
		if err != nil {
			return false, "dial: " + err.Error()
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			// Reached an authenticating server: the LINK is alive even though
			// this probe carried no credential (upstream #1723 semantics).
			return true, fmt.Sprintf("http %d (auth wall, link alive)", resp.StatusCode)
		case resp.StatusCode == http.StatusOK && strings.Contains(string(body), `"ok"`):
			return true, "http 200 probe ok"
		default:
			return false, fmt.Sprintf("http %d body %.80s", resp.StatusCode, body)
		}
	}
}

// step advances the ladder by one probe result. Split from the loop so tests
// drive it deterministically. Returns true when the process should exit.
func (w *watchdogState) step(alive bool, detail string, logf func(event, msg string, kv ...any)) bool {
	if alive {
		if w.consecutive > 0 {
			logf(diag.PrefixTunnelRecover, "watchdog: public entrance answering again",
				"detail", detail, "after_failures", w.consecutive)
		}
		w.consecutive = 0
		w.switched = false
		return false
	}
	w.consecutive++
	logf(diag.PrefixTunnelRetry, "watchdog: public entrance probe failed",
		"detail", detail, "consecutive", w.consecutive, "switched", w.switched)
	if !w.switched && w.consecutive >= w.cfg.FailuresBeforeSwitch {
		if next, ok := w.switchHost(); ok {
			w.switched = true
			w.consecutive = 0
			logf(diag.PrefixTunnelRetry, "watchdog: switching relay endpoint", "next", next)
			w.forceReconnect()
			return false
		}
		// Single-endpoint deployment: no switch available, fall through to
		// the exit ladder with the same budget.
		w.switched = true
		w.consecutive = 0
		w.forceReconnect()
		return false
	}
	if w.switched && w.consecutive >= w.cfg.FailuresBeforeExit {
		logf(diag.PrefixTunnelLost, "watchdog: entrance still dead after switch; exiting for supervisor restart",
			"detail", detail)
		return true
	}
	return false
}

// runWatchdog is the cadence loop; it exits with the tunnel context.
func (t *Tunnel) runWatchdog(ctx context.Context) {
	defer t.wg.Done()
	cfg := defaultsWatchdog(t.cfg.Watchdog)
	w := &watchdogState{
		cfg:        cfg,
		probe:      httpProber(cfg),
		switchHost: t.advanceRelayHost,
		forceReconnect: func() {
			t.mu.Lock()
			m := t.mux
			t.mu.Unlock()
			if m != nil {
				m.SetReason("watchdog: public entrance dead")
				_ = m.Close()
			}
		},
		exit: t.cfg.WatchdogExit,
	}
	if w.exit == nil {
		w.exit = defaultWatchdogExit
	}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
			alive, detail := w.probe(pctx)
			cancel()
			if w.step(alive, detail, func(event, msg string, kv ...any) {
				t.upstream.Records(event, msg, kv...)
			}) {
				w.exit(1)
				return
			}
		}
	}
}
