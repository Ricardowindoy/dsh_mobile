package relay

// Phone-device credentials: per-device revocable tokens, stored as SHA-256
// hashes only. The plaintext token exists exactly once — in the response that
// mints it (login?pair= sets it as a cookie; pair/self reads it back for the
// App's header calls). The roster file therefore is not a credential store:
// stealing it yields no usable token.
//
// This layer is additive: when Config.PhoneDevicesFile is empty the whole
// device-credential surface is off and authorize() keeps its legacy
// static-AccessToken behaviour byte for byte.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// deviceCookie carries the per-device token. It is separate from the legacy
// accessCookie (whose value is the shared AccessToken) so both can coexist
// during the migration window.
const deviceCookie = "dsh_relay_device"

// phoneDevice is one roster row. TokenHash is hex(SHA-256(token)); the token
// itself is never persisted.
type phoneDevice struct {
	DeviceID   string `json:"deviceId"`
	Name       string `json:"name"`
	TokenHash  string `json:"tokenHash"`
	CreatedAt  int64  `json:"createdAt"`  // unix ms
	LastSeenAt int64  `json:"lastSeenAt"` // unix ms
}

// phoneRoster is the in-memory device table plus its atomic on-disk mirror.
type phoneRoster struct {
	mu       sync.Mutex
	file     string
	byHash   map[string]*phoneDevice
	dirty    bool
	lastSave time.Time
}

type rosterFile struct {
	Version int           `json:"version"`
	Devices []phoneDevice `json:"devices"`
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// loadRoster reads the roster file. A missing file is an empty roster; a file
// that does not parse is moved aside (.corrupt-<ts>) rather than treated as
// empty, so an operator notices instead of silently losing every pairing.
func loadRoster(file string) (*phoneRoster, error) {
	pr := &phoneRoster{file: file, byHash: make(map[string]*phoneDevice)}
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return pr, nil
		}
		return nil, err
	}
	var rf rosterFile
	if err := json.Unmarshal(data, &rf); err != nil || rf.Version != 1 {
		aside := fmt.Sprintf("%s.corrupt-%d", file, time.Now().Unix())
		if renErr := os.Rename(file, aside); renErr != nil {
			return nil, fmt.Errorf("roster corrupt and cannot move aside: %w (parse: %v)", renErr, err)
		}
		return pr, nil
	}
	for i := range rf.Devices {
		d := rf.Devices[i]
		if d.TokenHash == "" || d.DeviceID == "" {
			continue // a row without identity or hash authorizes nothing
		}
		pr.byHash[d.TokenHash] = &d
	}
	return pr, nil
}

// saveLocked writes the roster atomically (temp file + rename, 0600/0700).
// Caller holds pr.mu.
func (pr *phoneRoster) saveLocked() error {
	rf := rosterFile{Version: 1, Devices: make([]phoneDevice, 0, len(pr.byHash))}
	for _, d := range pr.byHash {
		rf.Devices = append(rf.Devices, *d)
	}
	sort.Slice(rf.Devices, func(i, j int) bool { return rf.Devices[i].DeviceID < rf.Devices[j].DeviceID })
	data, err := json.MarshalIndent(&rf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pr.file), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(pr.file), ".roster-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, pr.file); err != nil {
		return err
	}
	pr.dirty = false
	pr.lastSave = time.Now()
	return nil
}

// lookup resolves a presented token to its roster row, refreshing lastSeen.
// The disk write is throttled (>=60s since the previous save) so a busy phone
// does not hammer the file; a revocation always forces an immediate save.
func (pr *phoneRoster) lookup(token string) *phoneDevice {
	if token == "" {
		return nil
	}
	h := hashToken(token)
	pr.mu.Lock()
	defer pr.mu.Unlock()
	d, ok := pr.byHash[h]
	if !ok {
		return nil
	}
	now := time.Now()
	d.LastSeenAt = now.UnixMilli()
	pr.dirty = true
	if now.Sub(pr.lastSave) >= time.Minute {
		// Best-effort: a failed lastSeen write must not deny the request.
		_ = pr.saveLocked()
	}
	return d
}

// add mints a new device row and persists it. Returns the plaintext token
// (the only place it ever exists) and the row.
func (pr *phoneRoster) add(name string) (string, *phoneDevice, error) {
	token, err := randomToken()
	if err != nil {
		return "", nil, err
	}
	idb := make([]byte, 6)
	if _, err := rand.Read(idb); err != nil {
		return "", nil, err
	}
	now := time.Now().UnixMilli()
	d := &phoneDevice{
		DeviceID:   "ph_" + base64.RawURLEncoding.EncodeToString(idb),
		Name:       name,
		TokenHash:  hashToken(token),
		CreatedAt:  now,
		LastSeenAt: now,
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.byHash[d.TokenHash] = d
	if err := pr.saveLocked(); err != nil {
		delete(pr.byHash, d.TokenHash)
		return "", nil, err
	}
	return token, d, nil
}

// revoke removes one device. If the removal cannot be persisted, the whole
// roster file is deleted instead of leaving a revoked credential on disk that
// the next start would restore — every device then re-pairs, which is safe;
// a silently surviving revocation is not.
func (pr *phoneRoster) revoke(deviceID string) (bool, error) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	var found string
	for h, d := range pr.byHash {
		if d.DeviceID == deviceID {
			found = h
			break
		}
	}
	if found == "" {
		return false, nil
	}
	delete(pr.byHash, found)
	if err := pr.saveLocked(); err != nil {
		// In-memory removal stands; make the disk side lose the credential too.
		if rmErr := os.Remove(pr.file); rmErr != nil && !os.IsNotExist(rmErr) {
			return true, fmt.Errorf("revoke persisted in memory only; disk removal failed: %w (save: %v)", rmErr, err)
		}
		pr.byHash = make(map[string]*phoneDevice)
		pr.dirty = false
		return true, fmt.Errorf("roster save failed (%v); roster file removed, all devices must re-pair", err)
	}
	return true, nil
}

// list returns display rows (no hashes, no tokens).
func (pr *phoneRoster) list() []phoneDevice {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	out := make([]phoneDevice, 0, len(pr.byHash))
	for _, d := range pr.byHash {
		row := *d
		row.TokenHash = ""
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// phoneCred extracts the device-token candidate from a request: header first,
// then the device cookie. The legacy shared-token cookie is NOT a device
// credential and is handled by authorize() separately.
func phoneCred(req *http.Request) string {
	if t := req.Header.Get("X-DSH-Relay-Token"); t != "" {
		return t
	}
	if t := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "); t != req.Header.Get("Authorization") && t != "" {
		return t
	}
	if c, err := req.Cookie(deviceCookie); err == nil {
		return c.Value
	}
	return ""
}

// authorizeDevice reports whether the request carries a live device token.
// It also refreshes lastSeen as a side effect of lookup.
func (r *Relay) authorizeDevice(req *http.Request) bool {
	if r.phones == nil {
		return false
	}
	return r.phones.lookup(phoneCred(req)) != nil
}

// authorizeAdmin checks the static management AccessToken only (mint, revoke,
// list, notify push). Device tokens deliberately do NOT pass this gate.
func (r *Relay) authorizeAdmin(req *http.Request) bool {
	if r.cfg.AccessToken == "" {
		return false // management surface requires an explicitly configured token
	}
	token := req.Header.Get("X-DSH-Relay-Token")
	if token == "" {
		token = strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	}
	if token == "" {
		if c, err := req.Cookie(accessCookie); err == nil {
			token = c.Value
		}
	}
	return token == r.cfg.AccessToken
}
