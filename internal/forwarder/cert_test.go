package forwarder

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCertPair generates a self-signed cert for tests and returns file paths.
func writeCertPair(t *testing.T, dir, commonName string, notAfter time.Time) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{commonName},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certFile := filepath.Join(dir, commonName+"-fullchain.pem")
	keyFile := filepath.Join(dir, commonName+"-privkey.pem")
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestCertSourceLoadsValidPair(t *testing.T) {
	dir := t.TempDir()
	c, k := writeCertPair(t, dir, "dsh.example.com", time.Now().Add(24*time.Hour))
	cs, err := NewCertSource(c, k, time.Hour, slog.Default())
	if err != nil {
		t.Fatalf("valid pair rejected: %v", err)
	}
	defer cs.Close()
	got, err := cs.GetCertificate(nil)
	if err != nil || got == nil || len(got.Certificate) == 0 {
		t.Errorf("GetCertificate = %v, %v", got, err)
	}
	cfg := cs.TLSConfig()
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want TLS1.2", cfg.MinVersion)
	}
	if cfg.CipherSuites != nil {
		t.Error("CipherSuites must stay nil (Go safe defaults)")
	}
	if cfg.GetCertificate == nil {
		t.Error("GetCertificate must be set")
	}
}

func TestCertSourceRejectsBadInitialPair(t *testing.T) {
	dir := t.TempDir()
	c, _ := writeCertPair(t, dir, "a.example.com", time.Now().Add(time.Hour))
	_, k2 := writeCertPair(t, dir, "b.example.com", time.Now().Add(time.Hour))
	// New fullchain with a DIFFERENT key's privkey must fail at load time.
	if _, err := NewCertSource(c, k2, time.Hour, slog.Default()); err == nil {
		t.Error("mismatched cert/key must fail loudly (no silent fallback)")
	}
	if _, err := NewCertSource(filepath.Join(dir, "missing.pem"), k2, time.Hour, slog.Default()); err == nil {
		t.Error("missing cert file must fail")
	}
	if _, err := NewCertSource(c, filepath.Join(dir, "missing.pem"), time.Hour, slog.Default()); err == nil {
		t.Error("missing key file must fail")
	}
}

func TestCertSourceHotReload(t *testing.T) {
	dir := t.TempDir()
	c, k := writeCertPair(t, dir, "hot.example.com", time.Now().Add(time.Hour))
	cs, err := NewCertSource(c, k, 20*time.Millisecond, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	first, _ := cs.GetCertificate(nil)

	// Replace BOTH files atomically (as certbot does via symlink rename).
	// The new cert has a different serial, so equalCert must detect the swap.
	time.Sleep(30 * time.Millisecond)
	c2, k2 := writeCertPair(t, dir, "hot2.example.com", time.Now().Add(48*time.Hour))
	if err := os.Rename(c2, c); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(k2, k); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		second, _ := cs.GetCertificate(nil)
		if !equalCert(first, second) {
			return // hot reload worked without restart
		}
		if time.Now().After(deadline) {
			t.Fatal("certificate was not hot-reloaded")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCertSourceKeepsOldCertOnBadReload(t *testing.T) {
	dir := t.TempDir()
	c, k := writeCertPair(t, dir, "keep.example.com", time.Now().Add(time.Hour))
	cs, err := NewCertSource(c, k, 20*time.Millisecond, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	before, _ := cs.GetCertificate(nil)

	// Corrupt only the cert file: reload must fail and KEEP the old pair.
	if err := os.WriteFile(c, []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	after, err := cs.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate failed after bad reload: %v", err)
	}
	if !equalCert(before, after) {
		t.Error("bad reload must keep the previous certificate, not swap in junk")
	}
}
