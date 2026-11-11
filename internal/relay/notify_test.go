package relay

// WP4a tests: notification plane gates, one-shot answers, SSE delivery,
// presence window. Mutation notes name the defect each test catches.

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func notifyRelay(t *testing.T) *Relay {
	t.Helper()
	r := quietRelay(t, Config{
		AccessToken:      "admin-sekrit",
		PhoneDevicesFile: t.TempDir() + "/phones.json",
		NotifyEnabled:    true,
	})
	return r
}

// Mutation: drop the admin gate on push -> red (a forged allow-notification
// from an anonymous caller is the attack).
func TestNotifyPushGate(t *testing.T) {
	r := notifyRelay(t)
	h := r.Handler()
	body := `{"deviceId":"ph_x","message":{"kind":"approval","id":"a1","toolName":"bash","reason":"rm -rf"}}`
	for _, hdr := range []map[string]string{nil, {"X-DSH-Relay-Token": "wrong"}} {
		w := do(h, "POST", "/__dsh_tunnel__/notify/push", strings.NewReader(body), hdr)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous/wrong push = %d, want 401", w.Code)
		}
	}
	// Device token is NOT the management credential.
	cookie := loginWithCode(t, h, mintCode(t, h))
	w := do(h, "POST", "/__dsh_tunnel__/notify/push", strings.NewReader(body), nil, cookie)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("device-token push = %d, want 401 (phones cannot push cards)", w.Code)
	}
	// Admin passes; unknown kind refused; bounded reason.
	w = do(h, "POST", "/__dsh_tunnel__/notify/push", strings.NewReader(body), map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	if w.Code != http.StatusOK {
		t.Fatalf("admin push = %d %q", w.Code, w.Body.String())
	}
	w = do(h, "POST", "/__dsh_tunnel__/notify/push",
		strings.NewReader(`{"deviceId":"ph_x","message":{"kind":"exec","id":"a2"}}`),
		map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown kind = %d, want 400", w.Code)
	}
}

// Mutation: accept any decision string -> red (the closed set is the safety).
// Mutation: allow replay -> red (double-allow on double-tap).
func TestNotifyAnswerClosedSetAndOneShot(t *testing.T) {
	r := notifyRelay(t)
	h := r.Handler()
	cookie := loginWithCode(t, h, mintCode(t, h))

	w := do(h, "POST", "/__dsh_tunnel__/notify/answer", strings.NewReader(`{"id":"a1","decision":"yes"}`), nil, cookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("rogue decision = %d, want 400", w.Code)
	}
	w = do(h, "POST", "/__dsh_tunnel__/notify/answer", strings.NewReader(`{"id":"a1","decision":"allowed-once"}`), nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("first answer = %d %q", w.Code, w.Body.String())
	}
	w = do(h, "POST", "/__dsh_tunnel__/notify/answer", strings.NewReader(`{"id":"a1","decision":"allowed-once"}`), nil, cookie)
	if w.Code != http.StatusGone {
		t.Fatalf("replayed answer = %d, want 410", w.Code)
	}
	// Anonymous answer refused.
	w = do(h, "POST", "/__dsh_tunnel__/notify/answer", strings.NewReader(`{"id":"a2","decision":"rejected"}`), nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous answer = %d, want 401", w.Code)
	}
	// The answer reached the management queue.
	w = do(h, "GET", "/__dsh_tunnel__/notify/answers?wait=0", nil, map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	if w.Code != http.StatusOK {
		t.Fatalf("answers = %d", w.Code)
	}
	var out struct {
		Answers []notifyAnswer `json:"answers"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Answers) != 1 || out.Answers[0].Decision != "allowed-once" || out.Answers[0].ID != "a1" {
		t.Fatalf("answers payload = %q", w.Body.String())
	}
	// Queue drained.
	w = do(h, "GET", "/__dsh_tunnel__/notify/answers?wait=0", nil, map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Answers) != 0 {
		t.Fatal("answers queue not drained")
	}
}

// Mutation: anyOnline always true -> red (the plugin would claim approvals
// with no phone attached and stall them).
func TestNotifyPresence(t *testing.T) {
	r := notifyRelay(t)
	if r.notify.anyOnline() {
		t.Fatal("no subscriber yet online=true")
	}
	r.notify.mu.Lock()
	d := r.notify.device("ph_x")
	d.lastSeen = time.Now()
	r.notify.mu.Unlock()
	if !r.notify.anyOnline() {
		t.Fatal("fresh subscriber online=false")
	}
	r.notify.mu.Lock()
	d.lastSeen = time.Now().Add(-2 * notifyOnlineWindow)
	r.notify.mu.Unlock()
	if r.notify.anyOnline() {
		t.Fatal("stale subscriber counted online")
	}
}

// SSE: a subscribed phone receives pushed cards; the ring replays to a late
// subscriber; a phone cannot watch another device's stream.
func TestNotifyWatchSSE(t *testing.T) {
	r := notifyRelay(t)
	h := r.Handler()
	token, _ := redeemCode(t, h, mintCode(t, h))
	// find the deviceId via self after login
	cookie := loginWithCode(t, h, mintCode(t, h))
	_ = token
	w := do(h, "GET", "/__dsh_tunnel__/pair/self", nil, nil, cookie)
	var self struct {
		DeviceID string `json:"deviceId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &self)

	srv := httptest.NewServer(h)
	defer srv.Close()

	// Push one card BEFORE subscribing: the ring must replay it.
	pushBody := `{"deviceId":"` + self.DeviceID + `","message":{"kind":"approval","id":"a9","toolName":"bash","reason":"test"}}`
	pw := do(h, "POST", "/__dsh_tunnel__/notify/push", strings.NewReader(pushBody), map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	if pw.Code != 200 {
		t.Fatalf("push = %d %q", pw.Code, pw.Body.String())
	}

	req, _ := http.NewRequest("GET", srv.URL+"/__dsh_tunnel__/notify/watch?deviceId="+self.DeviceID, nil)
	req.Header.Set("X-DSH-Relay-Token", cookie.Value)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("watch = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(resp.Body)
	// Skip the ": connected" primer and any keepalive comments.
	var line string
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatalf("SSE read: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			break
		}
	}
	var msg NotifyMessage
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &msg); err != nil || msg.ID != "a9" {
		t.Fatalf("replayed card = %+v err=%v", msg, err)
	}

	// Wrong-device watch refused.
	req2, _ := http.NewRequest("GET", srv.URL+"/__dsh_tunnel__/notify/watch?deviceId=ph_other", nil)
	req2.Header.Set("X-DSH-Relay-Token", cookie.Value)
	resp2, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-device watch = %d, want 403", resp2.StatusCode)
	}
	// Anonymous watch refused.
	resp3, err := srv.Client().Get(srv.URL + "/__dsh_tunnel__/notify/watch")
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous watch = %d, want 401", resp3.StatusCode)
	}
}

// Long poll: an answer arriving during the wait wakes the poller promptly.
func TestNotifyAnswersLongPoll(t *testing.T) {
	r := notifyRelay(t)
	h := r.Handler()
	cookie := loginWithCode(t, h, mintCode(t, h))
	srv := httptest.NewServer(h)
	defer srv.Close()

	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("GET", srv.URL+"/__dsh_tunnel__/notify/answers?wait=25", nil)
		req.Header.Set("X-DSH-Relay-Token", "admin-sekrit")
		resp, err := srv.Client().Do(req)
		if err != nil {
			done <- -1
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var out struct {
			Answers []notifyAnswer `json:"answers"`
		}
		_ = json.Unmarshal(body, &out)
		done <- len(out.Answers)
	}()
	time.Sleep(50 * time.Millisecond) // let the poller park
	w := do(h, "POST", "/__dsh_tunnel__/notify/answer", strings.NewReader(`{"id":"a5","decision":"rejected"}`), nil, cookie)
	if w.Code != 200 {
		t.Fatalf("answer = %d", w.Code)
	}
	select {
	case n := <-done:
		if n != 1 {
			t.Fatalf("long poll returned %d answers, want 1", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long poll did not wake on answer")
	}
}

// Disabled plane answers 501, not a crash or a silent 404.
func TestNotifyDisabled(t *testing.T) {
	r := quietRelay(t, Config{AccessToken: "x", PhoneDevicesFile: t.TempDir() + "/p.json"})
	h := r.Handler()
	w := do(h, "POST", "/__dsh_tunnel__/notify/push", strings.NewReader(`{}`), map[string]string{"X-DSH-Relay-Token": "x"})
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("disabled push = %d, want 501", w.Code)
	}
	_ = slog.Default()
}
