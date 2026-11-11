package tunnel

// In-process discovery of dsh web's per-process launch token, replacing the
// external dsh-token-refresh service + file round-trip. dsh web mints a fresh
// token on every start and prints it only to its startup log, so the sources
// are (in order): the unit journal, then a heap scan of the dsh process.
// Every candidate is probed against the local server (GET /?token= must 303)
// before it is trusted — a wrong token in the injection path turns 200 into 401.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	tokenCandidateLen = 43 // dsh launch tokens are 43-char base64url
	tokenProbeTimeout = 3 * time.Second
	discoverInterval  = 5 * time.Second
)

var (
	tokenJournalRe = regexp.MustCompile(`token=([A-Za-z0-9_-]{43})`)
)

// guiTokenState caches the discovered token; injection reads it memory-first.
type guiTokenState struct {
	mu    sync.Mutex
	value string
}

func (s *guiTokenState) get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value
}

func (s *guiTokenState) set(v string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v == "" || v == s.value {
		return false
	}
	s.value = v
	return true
}

// discoverGUIToken runs until ctx done: seed from file, then poll journal →
// heap → probe → cache (and keep the file in sync for backward compatibility).
//
// Three cost gates keep this loop cheap for the process lifetime (the
// original loop ran the FULL journal+heap+probe path every 5s forever —
// measured ~40% of one core and, with whole-heap buffering, hundreds of MB
// of pinned RSS on a long-lived tunnel):
//   - a cached token is RE-PROBED each tick (one 3s-bounded loopback GET) and
//     discovery only runs when it went stale — dsh mints a new token on every
//     restart, so "valid once" is not "valid forever";
//   - a negative cache (rejected candidates, TTL) stops the loop from
//     re-probing the same heap garbage every tick;
//   - when nothing valid is discoverable (e.g. the token lives in V8's mmap
//     heap, not the brk [heap] this scanner can see), discovery backs off
//     instead of re-reading ~200MB every 5s. An external refresher writing
//     the token file still wakes the loop on the next tick.
func (t *Tunnel) discoverGUIToken(ctx context.Context) {
	defer t.wg.Done()
	if t.cfg.GUIAuthTokenFile != "" {
		if b, err := os.ReadFile(t.cfg.GUIAuthTokenFile); err == nil {
			tok := strings.TrimSpace(string(b))
			if tok != "" && t.probeGUIToken(tok) {
				t.guiToken.set(tok)
				t.log.Info("guitoken: seeded from file", "len", len(tok))
			}
		}
	}
	var nextDiscovery time.Time // zero = due now
	for {
		// File source first: an external refresher (dsh-token-refresh) or the
		// operator maintains it; probe before trusting. journal/heap are
		// best-effort enhancements for setups without a refresher.
		if t.cfg.GUIAuthTokenFile != "" {
			if b, err := os.ReadFile(t.cfg.GUIAuthTokenFile); err == nil {
				tok := strings.TrimSpace(string(b))
				if tok != "" && tok != t.guiToken.get() && !t.rejected(tok) {
					if t.probeGUIToken(tok) {
						t.guiToken.set(tok)
						nextDiscovery = time.Time{} // file delivered: reset backoff
						t.log.Info("guitoken: refreshed from file", "len", len(tok))
					} else {
						t.reject(tok)
					}
				}
			}
		}
		// Cached token still valid? Then no discovery this tick: journalctl
		// spawn + full heap scan are the expensive path and only needed when
		// the token actually went stale.
		cached := t.guiToken.get()
		if cached != "" && t.probeGUIToken(cached) {
			t.validTicks.Add(1)
			nextDiscovery = time.Time{}
		} else if time.Now().After(nextDiscovery) {
			if cached != "" {
				t.guiToken.set("") // stale: stop injecting a dead token
				t.reject(cached)
			}
			t.discoverRuns.Add(1)
			if tok, from := t.findGUIToken(); tok != "" && t.guiToken.set(tok) {
				t.log.Info("guitoken: discovered", "from", from, "len", len(tok))
				nextDiscovery = time.Time{}
				if t.cfg.GUIAuthTokenFile != "" {
					_ = os.WriteFile(t.cfg.GUIAuthTokenFile, []byte(tok), 0o600)
				}
			} else {
				// Nothing valid out there. Back off: 30s → 60s → … capped 5m.
				// Without this, a token that is fundamentally undiscoverable
				// (V8 mmap heap) costs a full ~200MB heap scan every 5s forever.
				t.discoverBackoff = min(t.discoverBackoff*2, 5*time.Minute)
				if t.discoverBackoff < 30*time.Second {
					t.discoverBackoff = 30 * time.Second
				}
				nextDiscovery = time.Now().Add(t.discoverBackoff)
				t.log.Debug("guitoken: no valid token found, backing off", "next_in", t.discoverBackoff.String())
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(t.tick()):
		}
	}
}

// tick is the discovery loop period (tests compress it via Config.DiscoverInterval).
func (t *Tunnel) tick() time.Duration {
	if t.cfg.DiscoverInterval > 0 {
		return t.cfg.DiscoverInterval
	}
	return discoverInterval
}

// rejectedTTL bounds how long a failed candidate is skipped. Long enough to
// make the steady-state cost ~zero, short enough that a token which becomes
// valid later (dsh restart) is retried within a minute.
const rejectedTTL = 60 * time.Second

func (t *Tunnel) rejected(cand string) bool {
	t.rejectMu.Lock()
	defer t.rejectMu.Unlock()
	at, ok := t.rejectUntil[cand]
	return ok && time.Now().Before(at)
}

func (t *Tunnel) reject(cand string) {
	t.rejectMu.Lock()
	defer t.rejectMu.Unlock()
	if t.rejectUntil == nil {
		t.rejectUntil = make(map[string]time.Time)
	}
	// Opportunistic prune so a long-lived process cannot accumulate entries.
	if len(t.rejectUntil) > 256 {
		now := time.Now()
		for k, at := range t.rejectUntil {
			if now.After(at) {
				delete(t.rejectUntil, k)
			}
		}
	}
	t.rejectUntil[cand] = time.Now().Add(rejectedTTL)
}

func (t *Tunnel) findGUIToken() (string, string) {
	for _, src := range []struct {
		name   string
		getter func() []string
	}{
		{"journal", t.tokenCandidatesFromJournal},
		{"heap", t.tokenCandidatesFromHeap},
	} {
		for _, cand := range src.getter() {
			if t.rejected(cand) {
				continue
			}
			if t.probeGUIToken(cand) {
				return cand, src.name
			}
			t.reject(cand)
		}
	}
	return "", ""
}

// tokenCandidatesFromJournal scrapes the configured unit's recent log.
func (t *Tunnel) tokenCandidatesFromJournal() []string {
	unit := t.cfg.GUITokenJournalUnit
	if unit == "" {
		return nil
	}
	args := []string{}
	if t.cfg.GUITokenJournalUser {
		args = append(args, "--user")
	}
	args = append(args, "-u", unit, "--no-pager", "-n", "800")
	out, err := exec.Command("journalctl", args...).Output()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var cands []string
	for _, m := range tokenJournalRe.FindAllStringSubmatch(string(out), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			cands = append(cands, m[1])
		}
	}
	for i, j := 0, len(cands)-1; i < j; i, j = i+1, j-1 {
		cands[i], cands[j] = cands[j], cands[i]
	}
	return cands
}

// tokenCandidatesFromHeap scans the dsh-web process heap for token-shaped
// strings; covers setups where the startup log is not in any journal.
func (t *Tunnel) tokenCandidatesFromHeap() []string {
	pid := t.findDSHWebPID()
	if t.heapPidOverride != 0 { // test seam: scan a controlled region
		pid = t.heapPidOverride
	}
	if pid == 0 {
		return nil
	}
	region := heapRegion
	if t.heapRegionOverride != nil {
		region = t.heapRegionOverride
	}
	start, end, err := region(pid)
	if err != nil {
		return nil
	}
	f, err := os.Open(fmt.Sprintf("/proc/%d/mem", pid))
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(int64(start), 0); err != nil {
		return nil
	}
	size := end - start
	if size <= 0 || size > 512<<20 {
		return nil
	}
	return scanTokenCandidatesStream(io.LimitReader(f, int64(size)), int64(size))
}

// tokenScanChunkSize bounds the resident buffer of a heap scan. The scan runs
// every discoverInterval (5s) for the process lifetime; buffering the whole
// heap (up to the 512MB cap) in ONE allocation pinned a huge span in the Go
// heap forever: Go only returns memory to the OS in whole 8KB-aligned spans,
// and a multi-hundred-MB live object keeps its span alive across every GC
// cycle. Measured in production: dsh-tunnel RSS ~500MB flat with a ~100%
// single-core burst every 5s — the scan's alloc/read/scan cycle, with the RSS
// never coming back down. 1MB chunks keep the transient footprint flat.
const tokenScanChunkSize = 1 << 20

// scanTokenCandidatesStream extracts the same candidates as
// scanTokenCandidates over the WHOLE input, but never holds more than one
// chunk (+43-byte boundary overlap) in memory. The overlap carries the bytes
// that could be the tail of a token straddling the chunk boundary; the left
// boundary byte comes along so run-boundary decisions are identical to a
// single-buffer scan. Dedup is global across chunks.
//
// total bounds how much of r is consumed (the heap region size); a short read
// (heap shrank between maps and mem reads) simply ends the scan.
func scanTokenCandidatesStream(r io.Reader, total int64) []string {
	seen := map[string]bool{}
	var cands []string
	buf := make([]byte, tokenScanChunkSize+tokenCandidateLen)
	carry := 0
	read := int64(0)
	for read < total {
		want := int64(len(buf) - carry)
		if rem := total - read; rem < want {
			want = rem
		}
		n, err := io.ReadFull(r, buf[carry:carry+int(want)])
		if n == 0 {
			break // EOF or read error: nothing new to scan
		}
		read += int64(n)
		window := buf[:carry+n]
		// A full-size chunk may cut a token mid-run: skip starts whose run
		// could extend past the window (unless this is the final chunk) and
		// let the next chunk, with the overlap, decide them.
		limit := len(window) - tokenCandidateLen
		final := err != nil || read >= total
		for i := 0; i <= limit; i++ {
			if !final && i > len(window)-2*tokenCandidateLen {
				break // candidate's right boundary byte lies beyond this chunk
			}
			if tok, ok := candidateAt(window, i); ok && !seen[tok] {
				seen[tok] = true
				cands = append(cands, tok)
			}
		}
		if final {
			break
		}
		// Carry the last 2*len-1 bytes: any run straddling the boundary needs
		// its left context byte plus a full candidate window to be re-judged.
		carry = 2*tokenCandidateLen - 1
		if carry > len(window) {
			carry = len(window)
		}
		copy(buf[:carry], window[len(window)-carry:])
	}
	return cands
}

// candidateAt reports whether a tokenCandidateLen run starts at i with
// non-token bytes on both sides (identical rule to scanTokenCandidates).
func candidateAt(b []byte, i int) (string, bool) {
	if i+tokenCandidateLen > len(b) {
		return "", false
	}
	if i > 0 && isTokenByte(b[i-1]) {
		return "", false
	}
	if i+tokenCandidateLen < len(b) && isTokenByte(b[i+tokenCandidateLen]) {
		return "", false
	}
	for j := i; j < i+tokenCandidateLen; j++ {
		if !isTokenByte(b[j]) {
			return "", false
		}
	}
	return string(b[i : i+tokenCandidateLen]), true
}

func isTokenByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// scanTokenCandidates extracts 43-char base64url runs with non-token bytes on
// both sides (RE2 has no lookaround, so boundaries are checked manually).
func scanTokenCandidates(b []byte) []string {
	seen := map[string]bool{}
	var cands []string
	for i := 0; i+tokenCandidateLen <= len(b); i++ {
		if tok, ok := candidateAt(b, i); ok && !seen[tok] {
			seen[tok] = true
			cands = append(cands, tok)
		}
	}
	return cands
}

func heapRegion(pid int) (int, int, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "[heap]") {
			continue
		}
		bounds := strings.SplitN(strings.SplitN(line, " ", 2)[0], "-", 2)
		if len(bounds) != 2 {
			continue
		}
		start, e1 := strconv.ParseInt(bounds[0], 16, 64)
		end, e2 := strconv.ParseInt(bounds[1], 16, 64)
		if e1 != nil || e2 != nil {
			continue
		}
		return int(start), int(end), nil
	}
	return 0, 0, fmt.Errorf("no [heap] mapping for pid %d", pid)
}

// findDSHWebPID locates the dsh web process listening on the --local port.
func (t *Tunnel) findDSHWebPID() int {
	_, port, ok := strings.Cut(t.cfg.LocalAddr, ":")
	if !ok || port == "" {
		return 0
	}
	best := 0
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		cmd := strings.ReplaceAll(string(cmdline), "\x00", " ")
		if !strings.Contains(cmd, " web ") && !strings.HasSuffix(cmd, " web") {
			continue
		}
		if !strings.Contains(cmd, "--port "+port) {
			continue
		}
		if pid > best {
			best = pid
		}
	}
	return best
}

// probeGUIToken validates a candidate against the local GUI.
//
// Criterion (2026-11-09, measured on DSH 0.2.2-rc.1): since 0.2.2 the token
// gate answers 200 for BOTH valid and invalid tokens (no more 303), so the
// status code alone cannot decide. The only reliable signal is that a VALID
// token mints the session cookie: Set-Cookie contains "dsh-auth-". Accepting
// bare 200 let 43-char heap garbage (e.g. the node CLI flag
// "--force-node-api-uncaught-exceptions-policy") pass as the token and get
// written into the token file. 303 is still honored for older dsh versions.
func (t *Tunnel) probeGUIToken(cand string) bool {
	if len(cand) != tokenCandidateLen {
		return false
	}
	client := &http.Client{
		Timeout: tokenProbeTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get("http://" + t.cfg.LocalAddr + "/?token=" + cand)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusFound {
		return true // pre-0.2.2: mint + redirect
	}
	for _, v := range resp.Header.Values("Set-Cookie") {
		if strings.Contains(v, "dsh-auth-") {
			return true
		}
	}
	return false
}
