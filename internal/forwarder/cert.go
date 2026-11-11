package forwarder

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// CertSource holds one TLS certificate pair and hot-reloads it on a timer.
//
// Why a timer and not fsnotify: certbot's live/*.pem files are symlinks into
// archive/, and renewal replaces them by atomic rename. Watching the file
// itself loses the watch permanently (fsnotify FAQ). A periodic reload that
// re-reads BOTH files together, validates the pair, and swaps atomically is
// both simpler and more reliable.
//
// The tls.Config is never modified after publication: GetCertificate loads
// from an atomic pointer on every handshake, per Go's documented contract.
type CertSource struct {
	certFile string
	keyFile  string
	log      *slog.Logger
	current  atomic.Pointer[tls.Certificate]
	stop     chan struct{}
	done     chan struct{}
}

// NewCertSource loads the initial pair and starts the reload loop. It fails
// loudly on a missing/invalid pair (no silent fallback to plaintext).
func NewCertSource(certFile, keyFile string, interval time.Duration, log *slog.Logger) (*CertSource, error) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	cs := &CertSource{certFile: certFile, keyFile: keyFile, log: log, stop: make(chan struct{}), done: make(chan struct{})}
	cert, err := loadPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("forwarder: initial TLS cert %q/%q: %w", certFile, keyFile, err)
	}
	cs.current.Store(&cert)
	go cs.loop(interval)
	return cs, nil
}

// loadPair reads both files in one pass and requires LoadX509KeyPair to accept
// them. A new fullchain paired with an old privkey therefore fails here rather
// than producing a broken handshake.
func loadPair(certFile, keyFile string) (tls.Certificate, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read key: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse key pair: %w", err)
	}
	if len(cert.Certificate) == 0 {
		return tls.Certificate{}, fmt.Errorf("empty certificate chain")
	}
	return cert, nil
}

func (cs *CertSource) loop(interval time.Duration) {
	defer close(cs.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-cs.stop:
			return
		case <-t.C:
			cs.reload()
		}
	}
}

// reload swaps in a new pair only when it parses cleanly. On failure the old
// pair stays live and a warning is logged (never a silent stale cert).
func (cs *CertSource) reload() {
	cert, err := loadPair(cs.certFile, cs.keyFile)
	if err != nil {
		cs.log.Warn("forwarder: cert reload failed, keeping previous certificate", "err", err)
		return
	}
	old := cs.current.Swap(&cert)
	if old == nil || !equalCert(old, &cert) {
		cs.log.Info("forwarder: TLS certificate reloaded", "cert", cs.certFile)
	}
}

func equalCert(a, b *tls.Certificate) bool {
	if len(a.Certificate) != len(b.Certificate) {
		return false
	}
	for i := range a.Certificate {
		if string(a.Certificate[i]) != string(b.Certificate[i]) {
			return false
		}
	}
	return true
}

// GetCertificate is the tls.Config callback used by every handshake.
func (cs *CertSource) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c := cs.current.Load()
	if c == nil {
		return nil, fmt.Errorf("forwarder: no certificate loaded")
	}
	return c, nil
}

// TLSConfig builds the TLS config for one entrance. Security baseline is left
// to Go's safe defaults (nil CipherSuites, TLS 1.3 when available); we only
// pin the minimum version explicitly.
func (cs *CertSource) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: cs.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}
}

// Close stops the reload loop. It does not invalidate already-issued configs.
func (cs *CertSource) Close() {
	select {
	case <-cs.stop:
	default:
		close(cs.stop)
	}
	<-cs.done
}
