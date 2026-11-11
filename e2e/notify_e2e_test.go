package e2e

// WP4 end-to-end: real relay (notify plane on) + real approval-push plugin
// semantics exercised over the wire. The plugin itself is JS and tested in
// its own package; here we prove the RELAY side of the full loop with a real
// SSE subscriber: push → SSE delivery → device answer → management long-poll
// → one-shot replay refusal. Combined with the plugin's mock-ctx tests
// (claim/fail-closed/retract), the chain is covered end to end.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNotifyEndToEnd(t *testing.T) {
	dsh, _ := startFakeDsh(t)
	roster := t.TempDir() + "/phones.json"
	base, ws := startNotifyRelay(t, map[string]string{"tt": "dev-1"}, "admin-sekrit", roster)
	startTunnel(t, ws, "dev-1", "tt", strings.TrimPrefix(dsh.URL, "http://"))

	// Pair a phone (full QR flow).
	jar, deviceToken, deviceID := pairFlow(t, base, "admin-sekrit")
	_ = jar

	// Subscribe SSE with the device token header.
	req, _ := http.NewRequest("GET", base+"/__dsh_tunnel__/notify/watch?deviceId="+deviceID, nil)
	req.Header.Set("X-DSH-Relay-Token", deviceToken)
	sub := &http.Client{Timeout: 30 * time.Second}
	resp, err := sub.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("watch = %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	// readCard skips SSE comment/keepalive lines (": ping") and blanks.
	readCard := func() map[string]any {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("SSE read: %v", err)
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
				t.Fatalf("SSE json: %v", err)
			}
			return m
		}
		t.Fatal("no SSE card within deadline")
		return nil
	}

	// Presence must now read online (the plugin's claim gate).
	presence := getJSON(t, base+"/__dsh_tunnel__/notify/online", "admin-sekrit")
	if presence["online"] != true {
		t.Fatalf("presence = %v, want online after subscribe", presence)
	}

	// Plugin pushes an approval card (management credential).
	cardID := "s1/c1"
	pushBody := fmt.Sprintf(`{"deviceId":%q,"message":{"kind":"approval","id":%q,"sessionId":"s1","toolName":"bash","reason":"rm -rf /tmp/x"}}`, deviceID, cardID)
	pw := postJSON(t, base+"/__dsh_tunnel__/notify/push", "admin-sekrit", pushBody)
	if pw["delivered"].(float64) < 1 {
		t.Fatalf("push delivered=%v, want >=1 (SSE subscriber attached)", pw)
	}

	// The phone receives the card over SSE (ring backlog replays first).
	msg := readCard()
	if msg["id"] != cardID || msg["kind"] != "approval" || msg["toolName"] != "bash" {
		t.Fatalf("card = %v", msg)
	}

	// Phone answers (device credential, one-shot).
	ansBody := fmt.Sprintf(`{"id":%q,"decision":"allowed-once"}`, cardID)
	req, _ = http.NewRequest("POST", base+"/__dsh_tunnel__/notify/answer", strings.NewReader(ansBody))
	req.Header.Set("X-DSH-Relay-Token", deviceToken)
	req.Header.Set("Content-Type", "application/json")
	ar, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	ar.Body.Close()
	if ar.StatusCode != 200 {
		t.Fatalf("answer = %d", ar.StatusCode)
	}
	// Replay → 410.
	req, _ = http.NewRequest("POST", base+"/__dsh_tunnel__/notify/answer", strings.NewReader(ansBody))
	req.Header.Set("X-DSH-Relay-Token", deviceToken)
	ar2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	ar2.Body.Close()
	if ar2.StatusCode != 410 {
		t.Fatalf("replayed answer = %d, want 410", ar2.StatusCode)
	}

	// Plugin long-poll picks the answer up.
	got := getJSON(t, base+"/__dsh_tunnel__/notify/answers?wait=0", "admin-sekrit")
	answers, _ := got["answers"].([]any)
	if len(answers) != 1 {
		t.Fatalf("answers = %v", got)
	}
	a := answers[0].(map[string]any)
	if a["id"] != cardID || a["decision"] != "allowed-once" || a["deviceId"] != deviceID {
		t.Fatalf("answer = %v", a)
	}

	// Retract flow: push retract, phone sees it.
	retract := fmt.Sprintf(`{"deviceId":%q,"message":{"kind":"retract","id":%q}}`, deviceID, cardID)
	postJSON(t, base+"/__dsh_tunnel__/notify/push", "admin-sekrit", retract)
	rmsg := readCard()
	if rmsg["kind"] != "retract" || rmsg["id"] != cardID {
		t.Fatalf("retract = %v", rmsg)
	}
}

func getJSON(t *testing.T, url, adminToken string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("X-DSH-Relay-Token", adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s = %d %q", url, resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("GET %s bad json %q", url, body)
	}
	return out
}

func postJSON(t *testing.T, url, adminToken, body string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("X-DSH-Relay-Token", adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("POST %s = %d %q", url, resp.StatusCode, rb)
	}
	var out map[string]any
	if err := json.Unmarshal(rb, &out); err != nil {
		t.Fatalf("POST %s bad json %q", url, rb)
	}
	return out
}
