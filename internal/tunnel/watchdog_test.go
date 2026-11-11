package tunnel

// Watchdog tests drive the escalation ladder deterministically through the
// injected seams — no network, no real process exit. Each test names the
// mutation that turns it red.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func ladderRecorder() (logf func(string, string, ...any), events *[]string) {
	var ev []string
	return func(event, msg string, kv ...any) { ev = append(ev, event) }, &ev
}

// Mutation: treat 401/403 as dead (the pre-#1723 bug) -> TestWatchdogAuthWallAlive red.
func TestWatchdogAuthWallAlive(t *testing.T) {
	for _, code := range []int{401, 403} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		cfg := defaultsWatchdog(WatchdogConfig{PublicProbeURL: srv.URL, Timeout: 2 * time.Second})
		alive, detail := httpProber(cfg)(context.Background())
		srv.Close()
		if !alive {
			t.Fatalf("http %d judged dead: %s (an auth wall means the link IS alive)", code, detail)
		}
	}
}

// Mutation: accept any 200 without the probe marker -> red (a captive portal
// or wrong service answering 200 is not the relay).
func TestWatchdogProbeShape(t *testing.T) {
	cases := []struct {
		body  string
		alive bool
	}{
		{`{"ok":true,"deviceOnline":true}`, true},
		{`<html>captive portal</html>`, false},
		{`{}`, false},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(c.body))
		}))
		cfg := defaultsWatchdog(WatchdogConfig{PublicProbeURL: srv.URL, Timeout: 2 * time.Second})
		alive, _ := httpProber(cfg)(context.Background())
		srv.Close()
		if alive != c.alive {
			t.Fatalf("body %q judged alive=%v want %v", c.body, alive, c.alive)
		}
	}
}

// Mutation: never switch, or never exit -> the ladder assertions go red.
func TestWatchdogLadder(t *testing.T) {
	var switches, reconnects atomic.Int32
	w := &watchdogState{
		cfg:            defaultsWatchdog(WatchdogConfig{PublicProbeURL: "http://x", FailuresBeforeSwitch: 2, FailuresBeforeExit: 2}),
		switchHost:     func() (string, bool) { switches.Add(1); return "wss://backup", true },
		forceReconnect: func() { reconnects.Add(1) },
	}
	logf, events := ladderRecorder()

	// One failure: nothing happens yet.
	if w.step(false, "dial timeout", logf) {
		t.Fatal("exited after a single failure")
	}
	if switches.Load() != 0 {
		t.Fatal("switched after a single failure")
	}
	// Second failure: switch + forced reconnect, no exit.
	if w.step(false, "dial timeout", logf) {
		t.Fatal("exited at the switch step")
	}
	if switches.Load() != 1 || reconnects.Load() != 1 {
		t.Fatalf("switch=%d reconnect=%d, want 1/1", switches.Load(), reconnects.Load())
	}
	// One post-switch failure: still alive budget.
	if w.step(false, "dns dead", logf) {
		t.Fatal("exited one failure after the switch")
	}
	// Second post-switch failure: exit for the supervisor.
	if !w.step(false, "dns dead", logf) {
		t.Fatal("did not exit after the post-switch budget")
	}
	if !strings.Contains(strings.Join(*events, ","), diag_PrefixTunnelLost()) {
		t.Fatalf("no lost record in %v", *events)
	}
}

// diag prefix helper keeps the import surface small in tests.
func diag_PrefixTunnelLost() string { return "tunnel-lost" }

// Mutation: recovery does not reset the ladder -> red (a healed link would
// still exit on the next blip).
func TestWatchdogRecoveryResets(t *testing.T) {
	w := &watchdogState{
		cfg:            defaultsWatchdog(WatchdogConfig{PublicProbeURL: "http://x", FailuresBeforeSwitch: 2, FailuresBeforeExit: 1}),
		switchHost:     func() (string, bool) { return "wss://backup", true },
		forceReconnect: func() {},
	}
	logf, _ := ladderRecorder()
	w.step(false, "x", logf)
	w.step(false, "x", logf)  // switch
	w.step(true, "200", logf) // recovery resets everything
	// After recovery the ladder must be fully reset: the next two failures
	// buy a SWITCH, not an exit. If recovery left switched=true behind, the
	// very next failure would exit (budget 1) and these assertions catch it.
	if w.step(false, "x", logf) {
		t.Fatal("first failure after recovery must not exit (stale switched flag)")
	}
	if w.step(false, "x", logf) {
		t.Fatal("second failure after recovery should switch, not exit")
	}
	// Post-switch budget is 1: the FIRST failure after this second switch
	// must exit.
	if !w.step(false, "x", logf) {
		t.Fatal("first post-switch failure should exit with budget 1")
	}
}

// Single-endpoint deployment: no switch available, the ladder still forces a
// reconnect first and exits on the second budget.
func TestWatchdogSingleEndpoint(t *testing.T) {
	var reconnects atomic.Int32
	w := &watchdogState{
		cfg:            defaultsWatchdog(WatchdogConfig{PublicProbeURL: "http://x"}),
		switchHost:     func() (string, bool) { return "", false },
		forceReconnect: func() { reconnects.Add(1) },
	}
	logf, _ := ladderRecorder()
	w.step(false, "x", logf)
	if w.step(false, "x", logf) {
		t.Fatal("exited before forcing a reconnect")
	}
	if reconnects.Load() != 1 {
		t.Fatal("no forced reconnect on single endpoint")
	}
	w.step(false, "x", logf)
	if !w.step(false, "x", logf) {
		t.Fatal("single endpoint never exits")
	}
}
