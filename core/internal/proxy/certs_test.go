package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestCA generates a self-signed CA, writes it to a temp directory as PEM
// and returns the (certFile, keyFile) paths.
func writeTestCA(t *testing.T, isCA bool) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Rota Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}
	if !isCA {
		tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal CA key: %v", err)
	}

	dir := t.TempDir()
	certFile := filepath.Join(dir, "ca.crt")
	keyFile := filepath.Join(dir, "ca.key")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write CA key: %v", err)
	}
	return certFile, keyFile
}

// testCertAuthority returns a CertAuthority backed by a fresh self-signed CA.
func testCertAuthority(t *testing.T) *CertAuthority {
	t.Helper()
	certFile, keyFile := writeTestCA(t, true)
	ca, err := LoadCertAuthority(certFile, keyFile)
	if err != nil {
		t.Fatalf("LoadCertAuthority: %v", err)
	}
	return ca
}

func TestLoadCertAuthority_RejectsNonCA(t *testing.T) {
	certFile, keyFile := writeTestCA(t, false)

	// A leaf signed by a non-CA certificate is rejected by every client that
	// checks basic constraints, so failing at load beats failing per-request
	// at runtime with an opaque handshake error.
	if _, err := LoadCertAuthority(certFile, keyFile); err == nil {
		t.Fatal("LoadCertAuthority accepted a non-CA certificate")
	}
}

func TestLoadCertAuthority_MissingFiles(t *testing.T) {
	if _, err := LoadCertAuthority("/nonexistent/ca.crt", "/nonexistent/ca.key"); err == nil {
		t.Fatal("LoadCertAuthority accepted missing files")
	}
}

func TestCertAuthority_MintsVerifiableLeaf(t *testing.T) {
	ca := testCertAuthority(t)

	leaf, err := ca.CertFor("www.airbnb.com")
	if err != nil {
		t.Fatalf("CertFor: %v", err)
	}
	if leaf.Leaf == nil {
		t.Fatal("minted certificate has no parsed leaf")
	}

	// The client's whole trust decision is "does this chain to the CA I was
	// told to trust, for the host I asked for" — verify exactly that.
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{
		DNSName: "www.airbnb.com",
		Roots:   roots,
	}); err != nil {
		t.Fatalf("minted leaf does not verify against the CA: %v", err)
	}

	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{
		DNSName: "www.vrbo.com",
		Roots:   roots,
	}); err == nil {
		t.Error("leaf for www.airbnb.com also validated for www.vrbo.com")
	}
}

func TestCertAuthority_MintsIPCertificate(t *testing.T) {
	ca := testCertAuthority(t)

	leaf, err := ca.CertFor("127.0.0.1")
	if err != nil {
		t.Fatalf("CertFor: %v", err)
	}
	// A bare-IP target needs an IP SAN; a DNS SAN of "127.0.0.1" would not
	// satisfy a client connecting by address.
	if len(leaf.Leaf.IPAddresses) != 1 || !leaf.Leaf.IPAddresses[0].Equal([]byte{127, 0, 0, 1}) {
		t.Errorf("want a single 127.0.0.1 IP SAN, got IPs=%v DNS=%v",
			leaf.Leaf.IPAddresses, leaf.Leaf.DNSNames)
	}
	if len(leaf.Leaf.DNSNames) != 0 {
		t.Errorf("IP certificate should carry no DNS SANs, got %v", leaf.Leaf.DNSNames)
	}
}

func TestCertAuthority_CachesPerHost(t *testing.T) {
	ca := testCertAuthority(t)

	first, err := ca.CertFor("www.booking.com")
	if err != nil {
		t.Fatalf("CertFor: %v", err)
	}
	second, err := ca.CertFor("www.booking.com")
	if err != nil {
		t.Fatalf("CertFor (cached): %v", err)
	}
	// Signing is the expensive part of an intercepted handshake; a cache miss
	// per request would show up as latency on every connection.
	if first != second {
		t.Error("CertFor minted a second certificate for a cached host")
	}

	other, err := ca.CertFor("www.expedia.com")
	if err != nil {
		t.Fatalf("CertFor (other host): %v", err)
	}
	if other == first {
		t.Error("CertFor returned the same certificate for a different host")
	}
}

func TestCertAuthority_CacheIsBounded(t *testing.T) {
	ca := testCertAuthority(t)

	// A scraper walking a large host set must not grow the cache without
	// limit. Minting is slow, so exceed the bound by a small margin only.
	for i := 0; i < leafCacheSize+8; i++ {
		host := "host" + string(rune('a'+i%26)) + "-" + big.NewInt(int64(i)).String() + ".example.com"
		if _, err := ca.CertFor(host); err != nil {
			t.Fatalf("CertFor(%s): %v", host, err)
		}
	}

	ca.mu.Lock()
	size := len(ca.cache)
	order := len(ca.inOrder)
	ca.mu.Unlock()

	if size > leafCacheSize {
		t.Errorf("cache holds %d entries, want at most %d", size, leafCacheSize)
	}
	if order != size {
		t.Errorf("eviction order list (%d) drifted from cache (%d)", order, size)
	}
}
