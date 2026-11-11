package relay

// Notification plane for approval/question push cards (WP4).
//
// Topology: the host-side approval plugin POSTs notifications to the relay
// over the management credential (loopback socket + admin token, double
// gate — a forged "allow this" notification from the public internet is the
// attack this plane must not enable). Phones subscribe per-device over SSE
// with their device credential and answer with a one-shot POST. The plugin
// long-polls the answer queue with the management credential.
//
// Nothing persists: notifications are transient UI nudges. A phone that was
// offline reconciles through the plugin's pending table on reconnect (the
// plugin replays unsettled asks; the relay itself keeps only a small ring
// per device for late subscribers).

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	notifyRingPerDevice = 32
	// A new subscriber (no `after` cursor) only receives cards younger than
	// this; older ring entries are history, not something to pop on the phone.
	notifyBacklogMaxAge = 10 * time.Minute
	notifyAnswerTTL     = 2 * time.Minute
	notifyOnlineWindow  = 90 * time.Second
	notifyLongPollMax   = 25 * time.Second
)

// NotifyMessage is one push card. Kind is a closed set; Reason is bounded by
// the plugin before it gets here (defense in depth: re-bound at the gate).
//
// Question cards carry the question text and its option labels so the phone
// can offer one-tap answer actions (v1.0.15). Title carries the session title
// for completion cards. Both are display-only: the decision still rides the
// phone's device credential, and the card itself confers no authority.
type NotifyMessage struct {
	Kind      string `json:"kind"` // approval | question | done | retract
	ID        string `json:"id"`
	SessionID string `json:"sessionId,omitempty"`
	ToolName  string `json:"toolName,omitempty"`
	Reason    string `json:"reason,omitempty"`
	TS        int64  `json:"ts"`
	// Title is the session title shown on question/done cards.
	Title string `json:"title,omitempty"`
	// QuestionID/Question/Options describe the single one-tap-answerable
	// question. Option actions answer with {id: QuestionID, selected:[label]}.
	QuestionID  string   `json:"questionId,omitempty"`
	Question    string   `json:"question,omitempty"`
	Options     []string `json:"options,omitempty"`
	MultiSelect bool     `json:"multiSelect,omitempty"`
	// Preview is the clipped last assistant text on a completion card.
	Preview string `json:"preview,omitempty"`
	// Status distinguishes a normal completion from an interrupted/failed
	// turn on the same `done` kind: completed | interrupted | error | blocked.
	Status string `json:"status,omitempty"`
	// Seq is the relay's per-instance monotonic sequence assigned on publish.
	// The phone sends the last one it saw as `?after=` so a reconnect only
	// replays what it actually missed (v1.0.19). Without it every reconnect
	// replayed the whole ring — the "old notifications suddenly flooded in"
	// regression.
	Seq uint64 `json:"seq,omitempty"`
}

// QuestionAnswerItem mirrors the harness's AskUserQuestionAnswerItem shape:
// selected option labels plus an optional free-text answer.
type QuestionAnswerItem struct {
	ID       string   `json:"id"`
	Selected []string `json:"selected"`
	Custom   string   `json:"custom,omitempty"`
}

type notifyAnswer struct {
	ID       string `json:"id"`
	Decision string `json:"decision,omitempty"` // allowed-once | rejected (approval)
	// Answers carries structured question answers (question cards). Exactly
	// one of Decision/Answers is set per answer.
	Answers  []QuestionAnswerItem `json:"answers,omitempty"`
	DeviceID string               `json:"deviceId"`
	TS       int64                `json:"ts"`
}

type deviceNotify struct {
	ring     []NotifyMessage
	watchers map[chan NotifyMessage]struct{}
	lastSeen time.Time // SSE activity = presence
}

type notifyHub struct {
	mu           sync.Mutex
	seq          uint64
	devices      map[string]*deviceNotify
	answers      []notifyAnswer
	answerCh     chan struct{} // broadcast: new answer available
	answeredOnce map[string]time.Time
}

func newNotifyHub() *notifyHub {
	return &notifyHub{
		devices:  make(map[string]*deviceNotify),
		answerCh: make(chan struct{}, 1),
	}
}

func (h *notifyHub) device(id string) *deviceNotify {
	d, ok := h.devices[id]
	if !ok {
		d = &deviceNotify{watchers: make(map[chan NotifyMessage]struct{})}
		h.devices[id] = d
	}
	return d
}

// publish fans one message to the device's ring and live watchers.
func (h *notifyHub) publish(deviceID string, msg NotifyMessage) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.device(deviceID)
	h.seq++
	msg.Seq = h.seq
	d.ring = append(d.ring, msg)
	if len(d.ring) > notifyRingPerDevice {
		d.ring = d.ring[len(d.ring)-notifyRingPerDevice:]
	}
	n := 0
	for ch := range d.watchers {
		select {
		case ch <- msg:
			n++
		default: // slow subscriber drops the event, never blocks the pusher
		}
	}
	return n
}

// anyOnline reports whether ANY paired phone subscribed recently. The plugin
// asks this before claiming an approval: no subscriber → leave the ask to the
// GUI answerer instead of stalling it.
func (h *notifyHub) anyOnline() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := time.Now().Add(-notifyOnlineWindow)
	for _, d := range h.devices {
		if len(d.watchers) > 0 || d.lastSeen.After(cutoff) {
			return true
		}
	}
	return false
}

// anyWatching reports whether a phone currently has a live SSE channel. This
// is stricter than anyOnline(): presence keeps a 90s grace window, but a card
// pushed into that window reaches nobody. The claim gate must use this one —
// claiming an approval for a phone that is not actually listening turns into
// an automatic rejection after the timeout.
func (h *notifyHub) anyWatching() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, d := range h.devices {
		if len(d.watchers) > 0 {
			return true
		}
	}
	return false
}

// enqueueAnswer records a decision once. A replayed/unknown id is refused by
// the plugin's pending table; here the gate is the closed decision set and
// the queue bound.
func (h *notifyHub) enqueueAnswer(a notifyAnswer) {
	h.mu.Lock()
	h.answers = append(h.answers, a)
	if len(h.answers) > 256 {
		h.answers = h.answers[len(h.answers)-256:]
	}
	h.mu.Unlock()
	select {
	case h.answerCh <- struct{}{}:
	default:
	}
}

// drainAnswers takes the queue (long-poll helper).
func (h *notifyHub) drainAnswers() []notifyAnswer {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.answers
	h.answers = nil
	return out
}

// isLoopbackSocket reports whether the request's TCP peer is this machine.
// Forwarding headers are deliberately ignored: a public request proxied to
// loopback still carries a loopback socket, so the ADMIN TOKEN is the second
// condition and the socket test alone never grants anything.
func isLoopbackSocket(req *http.Request) bool {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// authorizeNotifyPush gates the management side of the notification plane.
// The credential is the static admin AccessToken (256-bit secret from a
// file), carried in a header — never a query. The payload is bounded and
// non-authoritative: a notification nudges the phone, but the DECISION still
// rides the phone's own device credential, and the plugin resolves nothing
// the phone did not answer. A forged notification is spam, not authority.
//
// Where the socket fact is meaningful (a loopback plain-HTTP listener), the
// deployment can additionally firewall that listener; the relay itself does
// not weaken the token gate based on socket shape, because a proxied public
// request arrives with a loopback socket (the exact confusion the upstream
// project documented in issue #1906).
func (r *Relay) authorizeNotifyPush(req *http.Request) bool {
	return r.authorizeAdmin(req)
}

func (r *Relay) notifyEnabled(w http.ResponseWriter) bool {
	if r.notify == nil {
		http.Error(w, "notify not enabled on this relay", http.StatusNotImplemented)
		return false
	}
	return true
}

// handleNotifyPush accepts one card from the plugin (management credential).
// POST /__dsh_tunnel__/notify/push {deviceId, message:{kind,id,sessionId,toolName,reason}}
func (r *Relay) handleNotifyPush(w http.ResponseWriter, req *http.Request) {
	if !r.notifyEnabled(w) {
		return
	}
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !r.authorizeNotifyPush(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		DeviceID string        `json:"deviceId"`
		Message  NotifyMessage `json:"message"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&body); err != nil {
		http.Error(w, "bad json body", http.StatusBadRequest)
		return
	}
	if body.DeviceID == "" {
		http.Error(w, "deviceId required", http.StatusBadRequest)
		return
	}
	switch body.Message.Kind {
	case "approval", "question", "done", "retract":
	default:
		http.Error(w, "unknown kind", http.StatusBadRequest)
		return
	}
	if body.Message.ID == "" || len(body.Message.ID) > 128 {
		http.Error(w, "message id required (<=128)", http.StatusBadRequest)
		return
	}
	if len(body.Message.Reason) > 400 {
		body.Message.Reason = body.Message.Reason[:400]
	}
	if body.Message.TS == 0 {
		body.Message.TS = time.Now().UnixMilli()
	}
	n := r.notify.publish(body.DeviceID, body.Message)
	writeJSON(w, http.StatusOK, map[string]any{"delivered": n})
}

// handleNotifyWatch is the phone's SSE subscription (device credential).
// GET /__dsh_tunnel__/notify/watch?deviceId=<ph_...>
func (r *Relay) handleNotifyWatch(w http.ResponseWriter, req *http.Request) {
	if !r.notifyEnabled(w) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// The device credential resolves the identity; the query deviceId must
	// match it (a phone may only watch its own stream).
	d := r.phones.lookup(phoneCred(req))
	if d == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if q := req.URL.Query().Get("deviceId"); q != "" && q != d.DeviceID {
		http.Error(w, "deviceId mismatch", http.StatusForbidden)
		return
	}
	// Resume cursor: only replay cards the phone has not seen. Without `after`
	// (fresh install / first start) replay only fresh cards, so hours-old ring
	// entries never surface as a burst of stale notifications.
	var after uint64
	hasAfter := false
	if raw := req.URL.Query().Get("after"); raw != "" {
		if v, err := strconv.ParseUint(raw, 10, 64); err == nil {
			after = v
			hasAfter = true
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Prime the stream: Go's response writer buffers headers+body until the
	// first flush, so without this the client's Do() waits for the first ping
	// (measured: 20s stall in e2e). A comment line is inert to SSE parsers.
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	ch := make(chan NotifyMessage, 16)
	r.notify.mu.Lock()
	dev := r.notify.device(d.DeviceID)
	dev.watchers[ch] = struct{}{}
	dev.lastSeen = time.Now()
	// A cursor from a previous relay generation is larger than anything this
	// process has issued (seq is in-memory and restarts at 0). Treat it as no
	// cursor: otherwise `m.Seq > after` is never true and the phone would
	// silently receive nothing until the app restarts.
	if hasAfter && after > r.notify.seq {
		hasAfter = false
	}
	backlog := make([]NotifyMessage, 0, len(dev.ring))
	cutoff := time.Now().Add(-notifyBacklogMaxAge)
	for _, m := range dev.ring {
		if hasAfter {
			if m.Seq > after {
				backlog = append(backlog, m)
			}
			continue
		}
		if time.UnixMilli(m.TS).After(cutoff) {
			backlog = append(backlog, m)
		}
	}
	r.notify.mu.Unlock()
	defer func() {
		r.notify.mu.Lock()
		delete(dev.watchers, ch)
		dev.lastSeen = time.Now()
		r.notify.mu.Unlock()
	}()

	send := func(msg NotifyMessage) bool {
		data, err := json.Marshal(msg)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	// Ring backlog first (a reconnecting phone sees recent cards), then live.
	for _, msg := range backlog {
		if !send(msg) {
			return
		}
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	ctx := req.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			if !send(msg) {
				return
			}
			r.notify.mu.Lock()
			dev.lastSeen = time.Now()
			r.notify.mu.Unlock()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
			r.notify.mu.Lock()
			dev.lastSeen = time.Now()
			r.notify.mu.Unlock()
		}
	}
}

// handleNotifyAnswer is the phone's decision (device credential, one-shot).
// POST /__dsh_tunnel__/notify/answer {id, decision}
func (r *Relay) handleNotifyAnswer(w http.ResponseWriter, req *http.Request) {
	if !r.notifyEnabled(w) {
		return
	}
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	d := r.phones.lookup(phoneCred(req))
	if d == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		ID       string               `json:"id"`
		Decision string               `json:"decision"`
		Answers  []QuestionAnswerItem `json:"answers"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 8192)).Decode(&body); err != nil {
		http.Error(w, "bad json body", http.StatusBadRequest)
		return
	}
	if body.ID == "" || len(body.ID) > 128 {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	// Exactly one answer shape: an approval decision from the closed set, or
	// structured question answers. A card that carries both is refused rather
	// than silently preferring one (a mismatch here would let a question card
	// be answered as an approval).
	hasDecision := body.Decision != ""
	hasAnswers := body.Answers != nil
	if hasDecision == hasAnswers {
		http.Error(w, "exactly one of decision/answers required", http.StatusBadRequest)
		return
	}
	if hasDecision && body.Decision != "allowed-once" && body.Decision != "rejected" {
		http.Error(w, "decision must be allowed-once or rejected", http.StatusBadRequest)
		return
	}
	if hasAnswers {
		if len(body.Answers) == 0 || len(body.Answers) > 16 {
			http.Error(w, "answers must contain 1..16 items", http.StatusBadRequest)
			return
		}
		for _, a := range body.Answers {
			if a.ID == "" || len(a.ID) > 128 {
				http.Error(w, "answer id required", http.StatusBadRequest)
				return
			}
			if a.Selected == nil {
				http.Error(w, "answer selected must be an array", http.StatusBadRequest)
				return
			}
			if len(a.Selected) > 16 || len(a.Custom) > 2000 {
				http.Error(w, "answer too large", http.StatusBadRequest)
				return
			}
		}
	}
	// One-shot: the same id from the same device is accepted once; a replay
	// answers 410 so the phone can mark the card handled/expired.
	key := d.DeviceID + "/" + body.ID
	r.notify.mu.Lock()
	if r.notify.answeredOnce == nil {
		r.notify.answeredOnce = make(map[string]time.Time)
	}
	now := time.Now()
	for k, t := range r.notify.answeredOnce {
		if now.Sub(t) > notifyAnswerTTL {
			delete(r.notify.answeredOnce, k)
		}
	}
	_, seen := r.notify.answeredOnce[key]
	if !seen {
		r.notify.answeredOnce[key] = now
	}
	r.notify.mu.Unlock()
	if seen {
		http.Error(w, "already answered", http.StatusGone)
		return
	}
	r.notify.enqueueAnswer(notifyAnswer{
		ID:       body.ID,
		Decision: body.Decision,
		Answers:  body.Answers,
		DeviceID: d.DeviceID,
		TS:       now.UnixMilli(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
}

// handleNotifyAnswers is the plugin's long poll (management credential).
// GET /__dsh_tunnel__/notify/answers?wait=25
func (r *Relay) handleNotifyAnswers(w http.ResponseWriter, req *http.Request) {
	if !r.notifyEnabled(w) {
		return
	}
	if !r.authorizeNotifyPush(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	wait := notifyLongPollMax
	if v := req.URL.Query().Get("wait"); v != "" {
		if d, err := time.ParseDuration(v + "s"); err == nil && d >= 0 && d <= notifyLongPollMax {
			wait = d
		}
	}
	if answers := r.notify.drainAnswers(); len(answers) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{"answers": answers})
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-req.Context().Done():
	case <-timer.C:
	case <-r.notify.answerCh:
	}
	answers := r.notify.drainAnswers()
	if answers == nil {
		answers = []notifyAnswer{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"answers": answers})
}

// handleNotifyOnline reports whether any phone is subscribed (management
// credential; the plugin's claim gate).
// GET /__dsh_tunnel__/notify/online -> {online:bool}
func (r *Relay) handleNotifyOnline(w http.ResponseWriter, req *http.Request) {
	if !r.notifyEnabled(w) {
		return
	}
	if !r.authorizeNotifyPush(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"online":   r.notify.anyOnline(),
		"watching": r.notify.anyWatching(),
	})
}
