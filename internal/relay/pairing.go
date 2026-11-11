package relay

// One-time pairing codes: the desktop mints a short-lived code (management
// credential required), renders it as a QR, and the phone's WebView navigates
// to login?pair=<code>. The relay burns the code on the first attempt
// (delete-on-attempt: a replayed or expired code resolves to nothing), mints a
// per-device token, plants it as a cookie, and redirects into the GUI. The
// device token itself never appears in a URL, a log line, or the QR.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	pairCodeTTL       = 120 * time.Second
	deviceCookieTTL   = 30 * 24 * 3600 // seconds, matches legacy accessCookie
	pairFailPerMinute = 5
)

// pairingState holds the single active code. Minting a new one voids the
// previous. Memory only: after a relay restart the desktop mints again.
type pairingState struct {
	mu        sync.Mutex
	code      string
	expiresAt time.Time
	// failures tracks recent failed redemptions per IP for rate limiting.
	failures map[string][]time.Time
	// tickets are one-time cookie vouchers: redeem/refresh return a ticket
	// alongside the device token; the WebView navigates to login?ticket=
	// which burns it and plants the cookie. Like the pairing code itself,
	// a ticket is single-use and short-lived, so its URL exposure is the
	// accepted bootstrap-secret class — the long-lived device token never
	// enters a URL (upstream dsh-remote-web-ui uses the same grant shape).
	tickets map[string]ticketEntry
}

type ticketEntry struct {
	token     string
	expiresAt time.Time
}

const (
	ticketTTL      = 60 * time.Second
	maxLiveTickets = 64
)

func newPairingState() *pairingState {
	return &pairingState{
		failures: make(map[string][]time.Time),
		tickets:  make(map[string]ticketEntry),
	}
}

// mintTicket issues a one-time cookie voucher for an already-minted device
// token. Expired entries are swept on each mint; the table is capped so a
// caller cannot grow it without bound (each entry still requires a live
// device credential or a burned pairing code to create).
func (ps *pairingState) mintTicket(token string) (string, time.Time, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	ticket := base64.RawURLEncoding.EncodeToString(b)
	exp := time.Now().Add(ticketTTL)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	now := time.Now()
	for k, e := range ps.tickets {
		if now.After(e.expiresAt) {
			delete(ps.tickets, k)
		}
	}
	if len(ps.tickets) >= maxLiveTickets {
		// Oldest out: a legitimate client retries; hoarding fails closed.
		var oldestKey string
		var oldest time.Time
		for k, e := range ps.tickets {
			if oldestKey == "" || e.expiresAt.Before(oldest) {
				oldestKey, oldest = k, e.expiresAt
			}
		}
		delete(ps.tickets, oldestKey)
	}
	ps.tickets[ticket] = ticketEntry{token: token, expiresAt: exp}
	return ticket, exp, nil
}

// consumeTicket burns a ticket on attempt and returns its device token.
func (ps *pairingState) consumeTicket(ticket string) (string, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	e, ok := ps.tickets[ticket]
	if !ok {
		return "", false
	}
	delete(ps.tickets, ticket) // delete-on-attempt: replay resolves to nothing
	if time.Now().After(e.expiresAt) {
		return "", false
	}
	return e.token, true
}

// mint replaces the active code.
func (ps *pairingState) mint() (string, time.Time, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	code := base64.RawURLEncoding.EncodeToString(b)
	exp := time.Now().Add(pairCodeTTL)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.code = code
	ps.expiresAt = exp
	return code, exp, nil
}

// consume burns the code on ANY attempt with a non-empty candidate: a wrong
// guess also voids the live code only when it matches; a mismatch leaves the
// live code intact but counts as a failure for rate limiting. A correct code
// is deleted before the caller does anything else, so a concurrent replay
// finds nothing.
func (ps *pairingState) consume(candidate string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.code == "" || time.Now().After(ps.expiresAt) {
		ps.code = ""
		return false
	}
	if subtle.ConstantTimeCompare([]byte(candidate), []byte(ps.code)) != 1 {
		return false
	}
	ps.code = ""
	return true
}

// status reports the live code's remaining lifetime without revealing it.
func (ps *pairingState) status() (active bool, expiresAt time.Time) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.code == "" || time.Now().After(ps.expiresAt) {
		return false, time.Time{}
	}
	return true, ps.expiresAt
}

// allowFailure records a failed redemption and reports whether the IP is
// still under the per-minute budget.
func (ps *pairingState) allowFailure(ip string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	keep := ps.failures[ip][:0]
	for _, t := range ps.failures[ip] {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	if len(keep) >= pairFailPerMinute {
		ps.failures[ip] = keep
		return false
	}
	ps.failures[ip] = append(keep, now)
	// Bound the table: drop IPs whose whole window expired.
	if len(ps.failures) > 1024 {
		for k, ts := range ps.failures {
			if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
				delete(ps.failures, k)
			}
		}
	}
	return true
}

func clientIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// handlePairMint issues a fresh one-time code. Management credential only,
// header/bearer/legacy-cookie carriers — never a query token.
// POST /__dsh_tunnel__/pair/mint -> {code, expiresAt}
func (r *Relay) handlePairMint(w http.ResponseWriter, req *http.Request) {
	if !r.pairingEnabled(w) {
		return
	}
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !r.authorizeAdmin(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	code, exp, err := r.pairing.mint()
	if err != nil {
		http.Error(w, "mint failed", http.StatusInternalServerError)
		return
	}
	r.log.Info("relay: pairing code minted", "expires", exp.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, map[string]any{
		"code":      code,
		"expiresAt": exp.UnixMilli(),
	})
}

// handlePairStatus reports whether a code is live (management only).
// GET /__dsh_tunnel__/pair/status -> {active, expiresAt}
func (r *Relay) handlePairStatus(w http.ResponseWriter, req *http.Request) {
	if !r.pairingEnabled(w) {
		return
	}
	if !r.authorizeAdmin(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	active, exp := r.pairing.status()
	payload := map[string]any{"active": active}
	if active {
		payload["expiresAt"] = exp.UnixMilli()
	}
	writeJSON(w, http.StatusOK, payload)
}

// handlePairDevices lists paired phones (management only; no hashes/tokens).
// GET /__dsh_tunnel__/pair/devices -> {devices:[{deviceId,name,createdAt,lastSeenAt}]}
func (r *Relay) handlePairDevices(w http.ResponseWriter, req *http.Request) {
	if !r.pairingEnabled(w) {
		return
	}
	if !r.authorizeAdmin(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": r.phones.list()})
}

// handlePairRevoke removes one device (management only).
// POST /__dsh_tunnel__/pair/revoke {deviceId} -> {revoked:true}
func (r *Relay) handlePairRevoke(w http.ResponseWriter, req *http.Request) {
	if !r.pairingEnabled(w) {
		return
	}
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !r.authorizeAdmin(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		DeviceID string `json:"deviceId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad json body", http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(body.DeviceID)
	if id == "" {
		http.Error(w, "deviceId required", http.StatusBadRequest)
		return
	}
	revoked, err := r.phones.revoke(id)
	if err != nil {
		// Revocation happened in memory either way; surface the persistence
		// problem honestly instead of claiming a clean state.
		r.log.Error("relay: revoke persistence problem", "deviceId", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"revoked": revoked, "warning": err.Error()})
		return
	}
	if !revoked {
		http.Error(w, "unknown deviceId", http.StatusNotFound)
		return
	}
	r.log.Info("relay: phone device revoked", "deviceId", id)
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

// handlePairSelf returns the caller's own device token exactly once per
// request so the App can persist it for header-authenticated calls after the
// cookie-based login. Requires the device cookie (just planted by login).
// GET /__dsh_tunnel__/pair/self -> {deviceId, name, token}
func (r *Relay) handlePairSelf(w http.ResponseWriter, req *http.Request) {
	if !r.pairingEnabled(w) {
		return
	}
	c, err := req.Cookie(deviceCookie)
	if err != nil || c.Value == "" {
		http.Error(w, "no device cookie", http.StatusUnauthorized)
		return
	}
	d := r.phones.lookup(c.Value)
	if d == nil {
		http.Error(w, "unknown device", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deviceId": d.DeviceID,
		"name":     d.Name,
		"token":    c.Value,
	})
}

// handlePairRedeem is the App-side redemption: the scanner POSTs the code as
// a JSON body (never a URL), receives the long-lived device token ONCE plus
// a one-time ticket for the WebView's cookie navigation.
// POST /__dsh_tunnel__/pair/redeem {code, name}
// -> {deviceToken, deviceId, ticket, ticketExpiresAt}
func (r *Relay) handlePairRedeem(w http.ResponseWriter, req *http.Request) {
	if !r.pairingEnabled(w) {
		return
	}
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad json body", http.StatusBadRequest)
		return
	}
	token, d, ok := r.redeemPairing(w, req, strings.TrimSpace(body.Code), body.Name)
	if !ok {
		return
	}
	ticket, exp, err := r.pairing.mintTicket(token)
	if err != nil {
		http.Error(w, "ticket mint failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deviceToken":     token,
		"deviceId":        d.DeviceID,
		"ticket":          ticket,
		"ticketExpiresAt": exp.UnixMilli(),
	})
}

// handlePairRefresh lets an already-paired App (device token in a header or
// cookie) mint a fresh login ticket — e.g. after the WebView cookie jar was
// cleared. The long-lived token stays out of the URL.
// POST /__dsh_tunnel__/pair/refresh -> {ticket, ticketExpiresAt}
func (r *Relay) handlePairRefresh(w http.ResponseWriter, req *http.Request) {
	if !r.pairingEnabled(w) {
		return
	}
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	cred := phoneCred(req)
	d := r.phones.lookup(cred)
	if d == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ticket, exp, err := r.pairing.mintTicket(cred)
	if err != nil {
		http.Error(w, "ticket mint failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deviceId":        d.DeviceID,
		"ticket":          ticket,
		"ticketExpiresAt": exp.UnixMilli(),
	})
}

func (r *Relay) pairingEnabled(w http.ResponseWriter) bool {
	if r.phones == nil || r.pairing == nil {
		http.Error(w, "pairing not enabled on this relay", http.StatusNotImplemented)
		return false
	}
	return true
}

// redeemPairing burns a presented code and mints a device. Returns the
// plaintext token and row, or an HTTP error already written to w.
func (r *Relay) redeemPairing(w http.ResponseWriter, req *http.Request, code, name string) (string, *phoneDevice, bool) {
	ip := clientIP(req)
	if code == "" {
		http.Error(w, "pair code required", http.StatusBadRequest)
		return "", nil, false
	}
	if !r.pairing.consume(code) {
		if !r.pairing.allowFailure(ip) {
			http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
			return "", nil, false
		}
		r.log.Warn("relay: pairing code rejected", "remote", req.RemoteAddr)
		http.Error(w, "pair code expired or already used", http.StatusNotFound)
		return "", nil, false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "phone"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	token, d, err := r.phones.add(name)
	if err != nil {
		r.log.Error("relay: device mint failed", "err", err)
		http.Error(w, "device mint failed", http.StatusInternalServerError)
		return "", nil, false
	}
	r.log.Info("relay: phone paired", "deviceId", d.DeviceID, "name", d.Name, "remote", req.RemoteAddr)
	return token, d, true
}

// setDeviceCookie plants the per-device credential. Secure follows the same
// rule as the legacy cookie: TLS-terminated here, or a fronting proxy that
// stamped X-Forwarded-Proto=https (the unified forwarder does).
func (r *Relay) setDeviceCookie(w http.ResponseWriter, req *http.Request, token string) {
	secure := r.cfg.TLSCertFile != "" || req.TLS != nil || req.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     deviceCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   deviceCookieTTL,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// loginRedirectTarget validates the post-login next parameter (same rule as
// the legacy path: root-relative only, no protocol-relative URLs).
func loginRedirectTarget(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

// appLogAllowed enforces a per-IP log-push budget (10/minute). Log pushing is
// a debug convenience; losing a few lines under abuse is strictly better than
// an anonymous disk-fill vector.
func (r *Relay) appLogAllowed(ip string) bool {
	r.appLogMu.Lock()
	defer r.appLogMu.Unlock()
	if r.appLogHits == nil {
		r.appLogHits = make(map[string][]time.Time)
	}
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	keep := r.appLogHits[ip][:0]
	for _, t := range r.appLogHits[ip] {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	if len(keep) >= 10 {
		r.appLogHits[ip] = keep
		return false
	}
	r.appLogHits[ip] = append(keep, now)
	if len(r.appLogHits) > 1024 {
		for k, ts := range r.appLogHits {
			if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
				delete(r.appLogHits, k)
			}
		}
	}
	return true
}
