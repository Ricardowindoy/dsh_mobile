package relay

// WP4a tests: notification plane gates, one-shot answers, SSE delivery,
// presence window. Mutation notes name the defect each test catches.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
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

	req, _ := http.NewRequest("GET", srv.URL+"/__dsh_tunnel__/notify/watch?deviceId="+self.DeviceID+"&after=0", nil)
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

// Question cards carry the question text + option labels and a session title;
// completion cards carry the session title + preview. Both are new closed-set
// kinds introduced with one-tap answers (v1.0.15).
// Mutation: drop "done" from the kind set -> red; drop question fields from
// the struct -> the phone would render an answerless card.
func TestNotifyPushQuestionAndDoneCards(t *testing.T) {
	r := notifyRelay(t)
	h := r.Handler()
	cookie := loginWithCode(t, h, mintCode(t, h))
	w := do(h, "GET", "/__dsh_tunnel__/pair/self", nil, nil, cookie)
	var self struct {
		DeviceID string `json:"deviceId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &self)

	questionCard := `{"deviceId":"` + self.DeviceID + `","message":{"kind":"question","id":"q1",` +
		`"title":"重构登录页","questionId":"scope","question":"选哪个方案？",` +
		`"options":["方案 A","方案 B"],"multiSelect":false}}`
	pw := do(h, "POST", "/__dsh_tunnel__/notify/push", strings.NewReader(questionCard),
		map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	if pw.Code != http.StatusOK {
		t.Fatalf("question push = %d %q", pw.Code, pw.Body.String())
	}

	doneCard := `{"deviceId":"` + self.DeviceID + `","message":{"kind":"done","id":"d1",` +
		`"title":"重构登录页","preview":"改完了，测试全绿"}}`
	pw = do(h, "POST", "/__dsh_tunnel__/notify/push", strings.NewReader(doneCard),
		map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	if pw.Code != http.StatusOK {
		t.Fatalf("done push = %d %q", pw.Code, pw.Body.String())
	}

	// The ring replays both cards with their payload intact.
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/__dsh_tunnel__/notify/watch?deviceId="+self.DeviceID+"&after=0", nil)
	req.Header.Set("X-DSH-Relay-Token", cookie.Value)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	var cards []NotifyMessage
	for len(cards) < 2 {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("SSE read: %v", err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var msg NotifyMessage
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &msg); err != nil {
			t.Fatalf("card decode: %v", err)
		}
		cards = append(cards, msg)
	}
	if cards[0].Kind != "question" || cards[0].Question != "选哪个方案？" ||
		len(cards[0].Options) != 2 || cards[0].Title != "重构登录页" || cards[0].QuestionID != "scope" {
		t.Fatalf("question card payload = %+v", cards[0])
	}
	if cards[1].Kind != "done" || cards[1].Title != "重构登录页" || cards[1].Preview == "" {
		t.Fatalf("done card payload = %+v", cards[1])
	}
}

// A question answer carries structured answers instead of a decision.
// Mutation: accept both shapes at once -> red (a question card could be
// answered as an approval); accept neither -> red (an empty answer would
// resolve a pending question as "skipped").
func TestNotifyAnswerStructured(t *testing.T) {
	r := notifyRelay(t)
	h := r.Handler()
	cookie := loginWithCode(t, h, mintCode(t, h))

	// Both shapes at once refused.
	w := do(h, "POST", "/__dsh_tunnel__/notify/answer",
		strings.NewReader(`{"id":"q1","decision":"rejected","answers":[{"id":"scope","selected":["方案 A"]}]}`), nil, cookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("both shapes = %d, want 400", w.Code)
	}
	// Neither shape refused.
	w = do(h, "POST", "/__dsh_tunnel__/notify/answer", strings.NewReader(`{"id":"q1"}`), nil, cookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("no shape = %d, want 400", w.Code)
	}
	// Malformed answers refused.
	for _, body := range []string{
		`{"id":"q1","answers":[]}`,
		`{"id":"q1","answers":[{"id":"","selected":["a"]}]}`,
		`{"id":"q1","answers":[{"id":"scope","selected":null}]}`,
	} {
		w = do(h, "POST", "/__dsh_tunnel__/notify/answer", strings.NewReader(body), nil, cookie)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", body, w.Code)
		}
	}
	// Valid structured answer accepted, and drained by the management poll.
	w = do(h, "POST", "/__dsh_tunnel__/notify/answer",
		strings.NewReader(`{"id":"q1","answers":[{"id":"scope","selected":["方案 B"],"custom":"补充说明"}]}`), nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("structured answer = %d %q", w.Code, w.Body.String())
	}
	aw := do(h, "GET", "/__dsh_tunnel__/notify/answers?wait=1", nil, map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	var out struct {
		Answers []notifyAnswer `json:"answers"`
	}
	_ = json.Unmarshal(aw.Body.Bytes(), &out)
	if len(out.Answers) != 1 || len(out.Answers[0].Answers) != 1 ||
		out.Answers[0].Answers[0].ID != "scope" ||
		out.Answers[0].Answers[0].Selected[0] != "方案 B" ||
		out.Answers[0].Answers[0].Custom != "补充说明" {
		t.Fatalf("drained answer = %+v", out.Answers)
	}
	// Replay of the same id is still one-shot.
	w = do(h, "POST", "/__dsh_tunnel__/notify/answer",
		strings.NewReader(`{"id":"q1","answers":[{"id":"scope","selected":["方案 A"]}]}`), nil, cookie)
	if w.Code != http.StatusGone {
		t.Fatalf("replayed structured answer = %d, want 410", w.Code)
	}
}

// v1.0.19: a reconnect must replay only what the phone missed.
// Mutation: ignore `after` → red (every reconnect re-delivered the whole ring,
// which surfaced hours-old cards as a burst on the phone).
func TestNotifyWatchResumeCursor(t *testing.T) {
	r := notifyRelay(t)
	h := r.Handler()
	cookie := loginWithCode(t, h, mintCode(t, h))
	w := do(h, "GET", "/__dsh_tunnel__/pair/self", nil, nil, cookie)
	var self struct {
		DeviceID string `json:"deviceId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &self)

	push := func(id string) {
		body := `{"deviceId":"` + self.DeviceID + `","message":{"kind":"approval","id":"` + id + `"}}`
		pw := do(h, "POST", "/__dsh_tunnel__/notify/push", strings.NewReader(body),
			map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
		if pw.Code != 200 {
			t.Fatalf("push %s = %d", id, pw.Code)
		}
	}
	push("c1")
	push("c2")

	readCards := func(query string, want int) []NotifyMessage {
		t.Helper()
		srv := httptest.NewServer(h)
		defer srv.Close()
		req, _ := http.NewRequest("GET", srv.URL+"/__dsh_tunnel__/notify/watch?deviceId="+self.DeviceID+query, nil)
		req.Header.Set("X-DSH-Relay-Token", cookie.Value)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		reader := bufio.NewReader(resp.Body)
		out := make([]NotifyMessage, 0, want)
		for len(out) < want {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("SSE read: %v", err)
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m NotifyMessage
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
				t.Fatal(err)
			}
			out = append(out, m)
		}
		return out
	}

	first := readCards("&after=0", 2)
	if first[0].ID != "c1" || first[1].ID != "c2" || first[0].Seq == 0 || first[1].Seq <= first[0].Seq {
		t.Fatalf("initial replay = %+v", first)
	}
	// Resume after c1: only c2 comes back.
	second := readCards("&after="+strconv.FormatUint(first[0].Seq, 10), 1)
	if second[0].ID != "c2" {
		t.Fatalf("resume replay = %+v, want c2", second)
	}
}

func TestNotifyOnlineWatching(t *testing.T) {
	r := notifyRelay(t)
	h := r.Handler()
	admin := map[string]string{"X-DSH-Relay-Token": "admin-sekrit"}

	read := func() (online, watching bool) {
		w := do(h, "GET", "/__dsh_tunnel__/notify/online", nil, admin)
		var out struct {
			Online   bool `json:"online"`
			Watching bool `json:"watching"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out.Online, out.Watching
	}

	if on, watch := read(); on || watch {
		t.Fatalf("empty hub: online=%v watching=%v", on, watch)
	}
	// Presence without a live channel: online=true (90s grace), watching=false.
	r.notify.mu.Lock()
	r.notify.device("ph_x").lastSeen = time.Now()
	r.notify.mu.Unlock()
	if on, watch := read(); !on || watch {
		t.Fatalf("presence-only: online=%v watching=%v, want true/false", on, watch)
	}
	// A live watcher flips both.
	r.notify.mu.Lock()
	d := r.notify.device("ph_x")
	ch := make(chan NotifyMessage, 1)
	d.watchers[ch] = struct{}{}
	r.notify.mu.Unlock()
	if on, watch := read(); !on || !watch {
		t.Fatalf("live watcher: online=%v watching=%v, want true/true", on, watch)
	}
}

// v2 semantics: a connection without a cursor gets NO history at all.
// Mutation: replay the ring when `after` is absent → red (old cards flood in).
func TestNotifyWatchNoReplayWithoutCursor(t *testing.T) {
	r := notifyRelay(t)
	h := r.Handler()
	cookie := loginWithCode(t, h, mintCode(t, h))
	w := do(h, "GET", "/__dsh_tunnel__/pair/self", nil, nil, cookie)
	var self struct {
		DeviceID string `json:"deviceId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &self)

	body := `{"deviceId":"` + self.DeviceID + `","message":{"kind":"done","id":"d-old","slot":"done:s"}}`
	if pw := do(h, "POST", "/__dsh_tunnel__/notify/push", strings.NewReader(body),
		map[string]string{"X-DSH-Relay-Token": "admin-sekrit"}); pw.Code != 200 {
		t.Fatalf("push = %d", pw.Code)
	}

	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/__dsh_tunnel__/notify/watch?deviceId="+self.DeviceID, nil)
	req.Header.Set("X-DSH-Relay-Token", cookie.Value)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return // deadline: no data frame, pass
		}
		if strings.HasPrefix(line, "data: ") {
			t.Fatalf("history replayed without a cursor: %s", line)
		}
	}
}

// The sequence must survive a relay restart. A fresh hub over the same file
// continues above the previous value.
// Mutation: keep seq in memory only → red.
func TestNotifySeqPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	seqFile := dir + "/notify-seq"
	r1 := quietRelay(t, Config{
		AccessToken: "admin-sekrit", PhoneDevicesFile: dir + "/phones.json",
		NotifyEnabled: true, NotifySeqFile: seqFile,
	})
	h1 := r1.Handler()
	cookie := loginWithCode(t, h1, mintCode(t, h1))
	w := do(h1, "GET", "/__dsh_tunnel__/pair/self", nil, nil, cookie)
	var self struct {
		DeviceID string `json:"deviceId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &self)
	pushBody := `{"deviceId":"` + self.DeviceID + `","message":{"kind":"done","id":"a","slot":"s"}}`
	if pw := do(h1, "POST", "/__dsh_tunnel__/notify/push", strings.NewReader(pushBody),
		map[string]string{"X-DSH-Relay-Token": "admin-sekrit"}); pw.Code != 200 {
		t.Fatalf("push = %d", pw.Code)
	}
	first := r1.notify.seq
	if first == 0 {
		t.Fatal("seq not assigned")
	}

	r2 := quietRelay(t, Config{
		AccessToken: "admin-sekrit", PhoneDevicesFile: dir + "/phones.json",
		NotifyEnabled: true, NotifySeqFile: seqFile,
	})
	if r2.notify.seq < first {
		t.Fatalf("seq went backwards after restart: %d < %d", r2.notify.seq, first)
	}
	if pw := do(r2.Handler(), "POST", "/__dsh_tunnel__/notify/push", strings.NewReader(pushBody),
		map[string]string{"X-DSH-Relay-Token": "admin-sekrit"}); pw.Code != 200 {
		t.Fatalf("push after restart = %d", pw.Code)
	}
	if r2.notify.seq <= first {
		t.Fatalf("seq did not advance after restart: %d <= %d", r2.notify.seq, first)
	}
}

// Acking a card removes it from later replays.
// Mutation: ignore the ack table → red (acked cards replay forever).
func TestNotifyAckStopsReplay(t *testing.T) {
	r := notifyRelay(t)
	h := r.Handler()
	cookie := loginWithCode(t, h, mintCode(t, h))
	w := do(h, "GET", "/__dsh_tunnel__/pair/self", nil, nil, cookie)
	var self struct {
		DeviceID string `json:"deviceId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &self)

	pushBody := `{"deviceId":"` + self.DeviceID + `","message":{"kind":"done","id":"ack-1","slot":"s"}}`
	if pw := do(h, "POST", "/__dsh_tunnel__/notify/push", strings.NewReader(pushBody),
		map[string]string{"X-DSH-Relay-Token": "admin-sekrit"}); pw.Code != 200 {
		t.Fatalf("push = %d", pw.Code)
	}
	// Ack with the device credential.
	aw := do(h, "POST", "/__dsh_tunnel__/notify/ack", strings.NewReader(`{"ids":["ack-1"]}`), nil, cookie)
	if aw.Code != 200 {
		t.Fatalf("ack = %d %q", aw.Code, aw.Body.String())
	}
	// Reconnect with the cursor that would otherwise replay it.
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/__dsh_tunnel__/notify/watch?deviceId="+self.DeviceID+"&after=0", nil)
	req.Header.Set("X-DSH-Relay-Token", cookie.Value)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		if strings.HasPrefix(line, "data: ") {
			t.Fatalf("acked card replayed: %s", line)
		}
	}
}
