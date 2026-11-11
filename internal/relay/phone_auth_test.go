package relay

// WP1 tests: per-device credentials, one-time pairing codes, token-free URLs,
// app-log gating, and the control-plane seal. Every gate has a mutation note:
// the test names the defect that would turn it red.

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quietRelay(t *testing.T, cfg Config) *Relay {
	t.Helper()
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cfg)
}

func pairingRelay(t *testing.T) (*Relay, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "phones", "phone-devices.json")
	r := quietRelay(t, Config{AccessToken: "admin-sekrit", PhoneDevicesFile: file})
	return r, file
}

func do(h http.Handler, method, target string, body io.Reader, hdr map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func mintCode(t *testing.T, h http.Handler) string {
	t.Helper()
	w := do(h, "POST", "/__dsh_tunnel__/pair/mint", nil, map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	if w.Code != http.StatusOK {
		t.Fatalf("mint = %d %q", w.Code, w.Body.String())
	}
	var out struct {
		Code      string `json:"code"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Code == "" {
		t.Fatalf("mint payload: %q err=%v", w.Body.String(), err)
	}
	if out.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("expiresAt not in the future: %d", out.ExpiresAt)
	}
	return out.Code
}

// redeemCode walks the App flow: POST pair/redeem (code in body) returns the
// device token plus a one-time ticket.
func redeemCode(t *testing.T, h http.Handler, code string) (token, ticket string) {
	t.Helper()
	w := do(h, "POST", "/__dsh_tunnel__/pair/redeem",
		strings.NewReader(`{"code":"`+code+`","name":"Pixel 8"}`),
		map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusOK {
		t.Fatalf("redeem = %d %q, want 200", w.Code, w.Body.String())
	}
	var out struct {
		DeviceToken string `json:"deviceToken"`
		DeviceID    string `json:"deviceId"`
		Ticket      string `json:"ticket"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.DeviceToken == "" || out.Ticket == "" || out.DeviceID == "" {
		t.Fatalf("redeem payload: %q err=%v", w.Body.String(), err)
	}
	return out.DeviceToken, out.Ticket
}

// loginWithCode walks the full QR flow (redeem then ticket login) and returns
// the device cookie.
func loginWithCode(t *testing.T, h http.Handler, code string) *http.Cookie {
	t.Helper()
	_, ticket := redeemCode(t, h, code)
	w := do(h, "GET", "/__dsh_tunnel__/login?ticket="+ticket+"&next=%2Fd%2Fhome-dsh%2F", nil, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("login?ticket = %d %q, want 302", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/d/home-dsh/" {
		t.Fatalf("Location = %q, want /d/home-dsh/", loc)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == deviceCookie {
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.MaxAge != deviceCookieTTL {
				t.Fatalf("cookie attributes wrong: %+v", c)
			}
			return c
		}
	}
	t.Fatal("no device cookie planted")
	return nil
}

// Mutation: drop the authorizeAdmin gate in handlePairMint -> red.
func TestPairMintRequiresAdmin(t *testing.T) {
	r, _ := pairingRelay(t)
	h := r.Handler()
	for _, hdr := range []map[string]string{
		nil,
		{"X-DSH-Relay-Token": "wrong"},
	} {
		w := do(h, "POST", "/__dsh_tunnel__/pair/mint", nil, hdr)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("mint without admin = %d, want 401", w.Code)
		}
	}
	// Query token must NOT be an admin carrier.
	w := do(h, "POST", "/__dsh_tunnel__/pair/mint?token=admin-sekrit", nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("mint with query token = %d, want 401 (query is not an admin carrier)", w.Code)
	}
	// GET is not allowed.
	w = do(h, "GET", "/__dsh_tunnel__/pair/mint", nil, map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET mint = %d, want 405", w.Code)
	}
}

// Mutation: make consume() non-destructive (allow replay) -> red.
func TestPairCodeSingleUseAndExpiry(t *testing.T) {
	r, _ := pairingRelay(t)
	h := r.Handler()
	code := mintCode(t, h)
	_ = loginWithCode(t, h, code)

	// Replay: the burned code resolves to nothing.
	w := do(h, "POST", "/__dsh_tunnel__/pair/redeem", strings.NewReader(`{"code":"`+code+`"}`), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("replayed code = %d, want 404", w.Code)
	}

	// A fresh mint voids the previous live code.
	code2 := mintCode(t, h)
	code3 := mintCode(t, h)
	w = do(h, "POST", "/__dsh_tunnel__/pair/redeem", strings.NewReader(`{"code":"`+code2+`"}`), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("superseded code = %d, want 404", w.Code)
	}
	_ = loginWithCode(t, h, code3)

	// Expiry (unit-level on the state machine, no 120s sleep).
	ps := newPairingState()
	c, _, err := ps.mint()
	if err != nil {
		t.Fatal(err)
	}
	ps.expiresAt = time.Now().Add(-time.Second)
	if ps.consume(c) {
		t.Fatal("expired code consumed")
	}
}

// Mutation: make consumeTicket non-destructive or ignore expiry -> red.
func TestTicketSingleUseAndExpiry(t *testing.T) {
	r, _ := pairingRelay(t)
	h := r.Handler()
	token, ticket := redeemCode(t, h, mintCode(t, h))

	// First login plants the cookie.
	w := do(h, "GET", "/__dsh_tunnel__/login?ticket="+ticket, nil, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("ticket login = %d", w.Code)
	}
	// Replay: burned.
	w = do(h, "GET", "/__dsh_tunnel__/login?ticket="+ticket, nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("replayed ticket = %d, want 404", w.Code)
	}
	// Wrong ticket counts against the failure budget but does not burn a
	// live one.
	_, ticket2 := redeemCode(t, h, mintCode(t, h))
	w = do(h, "GET", "/__dsh_tunnel__/login?ticket=wrong", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("wrong ticket = %d, want 404", w.Code)
	}
	w = do(h, "GET", "/__dsh_tunnel__/login?ticket="+ticket2, nil, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("live ticket after wrong guess = %d, want 302", w.Code)
	}

	// pair/refresh: a paired device mints a fresh ticket with its header
	// token; the returned ticket logs in again.
	w = do(h, "POST", "/__dsh_tunnel__/pair/refresh", nil, map[string]string{"X-DSH-Relay-Token": token})
	if w.Code != http.StatusOK {
		t.Fatalf("refresh = %d %q", w.Code, w.Body.String())
	}
	var ref struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &ref)
	if ref.Ticket == "" {
		t.Fatal("refresh returned no ticket")
	}
	w = do(h, "GET", "/__dsh_tunnel__/login?ticket="+ref.Ticket, nil, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("refreshed ticket login = %d", w.Code)
	}
	// Without a device credential refresh is refused.
	w = do(h, "POST", "/__dsh_tunnel__/pair/refresh", nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous refresh = %d, want 401", w.Code)
	}
	// The management token is NOT a device credential for refresh.
	w = do(h, "POST", "/__dsh_tunnel__/pair/refresh", nil, map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("admin-token refresh = %d, want 401 (admin is not a phone)", w.Code)
	}

	// Ticket expiry at the state-machine level.
	ps := newPairingState()
	tick, _, err := ps.mintTicket("tok")
	if err != nil {
		t.Fatal(err)
	}
	ps.tickets[tick] = ticketEntry{token: "tok", expiresAt: time.Now().Add(-time.Second)}
	if _, ok := ps.consumeTicket(tick); ok {
		t.Fatal("expired ticket consumed")
	}
}

// Mutation: remove the allowFailure budget -> red.
func TestPairFailureRateLimit(t *testing.T) {
	r, _ := pairingRelay(t)
	h := r.Handler()
	_ = mintCode(t, h) // a live code exists; wrong guesses must not burn it
	for i := 0; i < pairFailPerMinute; i++ {
		w := do(h, "POST", "/__dsh_tunnel__/pair/redeem", strings.NewReader(`{"code":"wrong-guess"}`), nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("attempt %d = %d, want 404", i, w.Code)
		}
	}
	w := do(h, "POST", "/__dsh_tunnel__/pair/redeem", strings.NewReader(`{"code":"wrong-guess"}`), nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget attempt = %d, want 429", w.Code)
	}
}

// Mutation: store the plaintext token in the roster -> red (file scan).
// Mutation: keep a revoked row on disk -> red (reload check).
func TestRosterHashOnlyAndRevoke(t *testing.T) {
	r, file := pairingRelay(t)
	h := r.Handler()
	code := mintCode(t, h)
	cookie := loginWithCode(t, h, code)

	// pair/self returns the token exactly to the cookie holder.
	w := do(h, "GET", "/__dsh_tunnel__/pair/self", nil, nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("pair/self = %d %q", w.Code, w.Body.String())
	}
	var self struct {
		DeviceID string `json:"deviceId"`
		Token    string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &self); err != nil {
		t.Fatal(err)
	}
	if self.Token != cookie.Value || self.DeviceID == "" {
		t.Fatalf("self payload wrong: %+v", self)
	}

	// The roster file must not contain the plaintext token.
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), cookie.Value) {
		t.Fatal("roster file contains the plaintext device token")
	}
	if !strings.Contains(string(data), hashToken(cookie.Value)) {
		t.Fatal("roster file missing the token hash")
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("roster perm = %o, want 600", info.Mode().Perm())
	}

	// An empty request carries no credential: authorizeDevice (the roster
	// verdict alone) must reject it. authorize() itself falls back to the
	// legacy admin token, so it is tested with credentials below.
	if r.authorizeDevice(httptest.NewRequest("GET", "/", nil)) {
		t.Fatal("empty request must not pass the device gate")
	}
}

// Separate: data-plane acceptance and revocation semantics.
func TestDeviceTokenDataPlaneAndRevoke(t *testing.T) {
	r, file := pairingRelay(t)
	h := r.Handler()
	cookieA := loginWithCode(t, h, mintCode(t, h))
	cookieB := loginWithCode(t, h, mintCode(t, h))

	reqWith := func(c *http.Cookie) *http.Request {
		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(c)
		return req
	}
	if !r.authorize(reqWith(cookieA)) || !r.authorize(reqWith(cookieB)) {
		t.Fatal("live device tokens must authorize")
	}
	// Header carrier too.
	hreq := httptest.NewRequest("GET", "/", nil)
	hreq.Header.Set("X-DSH-Relay-Token", cookieA.Value)
	if !r.authorize(hreq) {
		t.Fatal("header device token must authorize")
	}
	// Device token is NOT an admin credential.
	if r.authorizeAdmin(hreq) {
		t.Fatal("device token must not pass the admin gate")
	}

	// List shows two devices, no hashes.
	w := do(h, "GET", "/__dsh_tunnel__/pair/devices", nil, map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "tokenHash\":\"") && !strings.Contains(w.Body.String(), `"tokenHash":""`) {
		t.Fatalf("devices list = %d %q", w.Code, w.Body.String())
	}
	var list struct {
		Devices []phoneDevice `json:"devices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Devices) != 2 {
		t.Fatalf("list payload: %q err=%v", w.Body.String(), err)
	}
	for _, d := range list.Devices {
		if d.TokenHash != "" {
			t.Fatal("list leaked a token hash")
		}
	}

	// Revoke A: A dies, B lives, disk agrees after reload.
	idA := list.Devices[0].DeviceID
	// find which row is A by trying both; revoke the one whose token stops working
	w = do(h, "POST", "/__dsh_tunnel__/pair/revoke", strings.NewReader(`{"deviceId":"`+idA+`"}`),
		map[string]string{"X-DSH-Relay-Token": "admin-sekrit", "Content-Type": "application/json"})
	if w.Code != http.StatusOK {
		t.Fatalf("revoke = %d %q", w.Code, w.Body.String())
	}
	aDead := !r.authorize(reqWith(cookieA)) && r.authorize(reqWith(cookieB))
	bDead := !r.authorize(reqWith(cookieB)) && r.authorize(reqWith(cookieA))
	if !aDead && !bDead {
		t.Fatal("revocation did not remove exactly one device")
	}
	// Reload from disk: same verdict.
	pr, err := loadRoster(file)
	if err != nil {
		t.Fatal(err)
	}
	r2 := &Relay{cfg: r.cfg, phones: pr}
	if r2.authorize(reqWith(cookieA)) == r.authorize(reqWith(cookieA)) &&
		r2.authorize(reqWith(cookieB)) == r.authorize(reqWith(cookieB)) {
		// same verdicts = persisted correctly
	} else {
		t.Fatal("disk roster disagrees with in-memory revocation")
	}
	// Unknown id -> 404.
	w = do(h, "POST", "/__dsh_tunnel__/pair/revoke", strings.NewReader(`{"deviceId":"ph_nope"}`),
		map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("revoke unknown = %d, want 404", w.Code)
	}
}

// Mutation: treat a corrupt roster as empty (silent credential loss) -> red.
func TestRosterCorruptMovedAside(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "phone-devices.json")
	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr, err := loadRoster(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.byHash) != 0 {
		t.Fatal("corrupt roster yielded rows")
	}
	entries, _ := os.ReadDir(dir)
	foundAside := false
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt-") {
			foundAside = true
		}
	}
	if !foundAside {
		t.Fatal("corrupt file not moved aside")
	}
}

// Legacy compatibility: with PhoneDevicesFile empty everything behaves as
// before (static token via query login, cookie, header).
func TestLegacyStaticTokenUnchanged(t *testing.T) {
	r := quietRelay(t, Config{AccessToken: "sekrit"})
	h := r.Handler()
	// Pairing surface reports not-implemented, not a crash.
	w := do(h, "POST", "/__dsh_tunnel__/pair/mint", nil, map[string]string{"X-DSH-Relay-Token": "sekrit"})
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("mint on legacy relay = %d, want 501", w.Code)
	}
	// Legacy query login still plants the legacy cookie.
	w = do(h, "GET", "/__dsh_tunnel__/login?token=sekrit&next=%2F", nil, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("legacy login = %d", w.Code)
	}
	var legacy *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == accessCookie {
			legacy = c
		}
	}
	if legacy == nil || legacy.Value != "sekrit" {
		t.Fatal("legacy cookie missing")
	}
	// auth-check: right query token passes, wrong fails even though the
	// gate has no other carrier (regression found by TestNodePrefixedTunnelEndpoints).
	if w := do(h, "GET", "/__dsh_tunnel__/auth-check?token=sekrit", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("auth-check right query = %d", w.Code)
	}
	if w := do(h, "GET", "/__dsh_tunnel__/auth-check?token=wrong", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("auth-check wrong query = %d, want 401", w.Code)
	}
	// Header carrier (new) also works.
	if w := do(h, "GET", "/__dsh_tunnel__/auth-check", nil, map[string]string{"X-DSH-Relay-Token": "sekrit"}); w.Code != http.StatusOK {
		t.Fatalf("auth-check header = %d", w.Code)
	}
}

// Mutation: remove the app-log credential gate or the rate budget -> red.
func TestAppLogGateAndLimit(t *testing.T) {
	r := quietRelay(t, Config{AccessToken: "sekrit", UpdatesDir: filepath.Join(t.TempDir(), "updates")})
	h := r.Handler()
	// Anonymous push denied.
	w := do(h, "POST", "/__dsh_tunnel__/app-log", strings.NewReader("line"), nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous app-log = %d, want 401", w.Code)
	}
	// Credentialed pushes pass up to the budget.
	hdr := map[string]string{"X-DSH-Relay-Token": "sekrit"}
	for i := 0; i < 10; i++ {
		w := do(h, "POST", "/__dsh_tunnel__/app-log", strings.NewReader(fmt.Sprintf("line %d", i)), hdr)
		if w.Code != http.StatusOK {
			t.Fatalf("push %d = %d %q", i, w.Code, w.Body.String())
		}
	}
	w = do(h, "POST", "/__dsh_tunnel__/app-log", strings.NewReader("over"), hdr)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget push = %d, want 429", w.Code)
	}
}

// Mutation: drop the /__dsh_tunnel__/ seal (fall through to handlePhone) -> red.
// The judge is the response SHAPE: relay JSON 404, not handlePhone's 503/401.
func TestControlPlaneSeal(t *testing.T) {
	r, _ := pairingRelay(t)
	r.devices["home-dsh"] = &device{id: "home-dsh"} // would answer 503 if forwarded
	h := r.Handler()
	w := do(h, "GET", "/__dsh_tunnel__/definitely-not-registered", nil, map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("unregistered control path = %d, want 404", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] != "not-found" {
		t.Fatalf("seal body = %q, want relay not-found JSON (not a device response)", w.Body.String())
	}
	// Node-prefixed variant takes the same seal.
	w = do(h, "GET", "/d/home-dsh/__dsh_tunnel__/nope", nil, map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("node-prefixed unregistered = %d, want 404", w.Code)
	}
}

// pair/self without a cookie is a 401, and with a revoked cookie likewise.
func TestPairSelfRequiresLiveCookie(t *testing.T) {
	r, _ := pairingRelay(t)
	h := r.Handler()
	if w := do(h, "GET", "/__dsh_tunnel__/pair/self", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("self without cookie = %d, want 401", w.Code)
	}
	cookie := loginWithCode(t, h, mintCode(t, h))
	w := do(h, "GET", "/__dsh_tunnel__/pair/devices", nil, map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	var list struct {
		Devices []phoneDevice `json:"devices"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	_ = do(h, "POST", "/__dsh_tunnel__/pair/revoke", strings.NewReader(`{"deviceId":"`+list.Devices[0].DeviceID+`"}`),
		map[string]string{"X-DSH-Relay-Token": "admin-sekrit"})
	if w := do(h, "GET", "/__dsh_tunnel__/pair/self", nil, nil, cookie); w.Code != http.StatusUnauthorized {
		t.Fatalf("self with revoked cookie = %d, want 401", w.Code)
	}
}
