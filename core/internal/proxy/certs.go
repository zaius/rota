package proxy

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"os"
	"sync"
	"time"
)

// leafValidity is how long a generated leaf certificate is good for. Short,
// because these certs are minted on demand and only ever presented to a client
// that already trusts the CA — a long life buys nothing and widens the window
// if one leaks.
const leafValidity = 24 * time.Hour

// leafCacheSize bounds the generated-certificate cache. A scraper hitting a
// large host set would otherwise grow the map without limit; eviction is FIFO,
// which for this workload behaves like "drop the hosts we stopped visiting".
const leafCacheSize = 512

// CertAuthority mints short-lived leaf certificates for intercepted hosts,
// signed by a locally configured CA.
//
// The CA private key never leaves this process, and the certificates it signs
// are only trusted by clients that were explicitly configured to trust it —
// which is the entire security boundary of TLS interception. Nothing here is
// safe to point at a CA whose key is shared with anything else.
type CertAuthority struct {
	cert *x509.Certificate
	key  crypto.Signer

	mu      sync.Mutex
	cache   map[string]*tls.Certificate
	inOrder []string // insertion order, for FIFO eviction
}

// LoadCertAuthority reads a PEM-encoded CA certificate and private key from
// disk. The certificate must be a CA (BasicConstraints CA:TRUE with the
// certificate-signing key usage) or the leaves it signs will be rejected by
// every client that checks.
func LoadCertAuthority(certFile, keyFile string) (*CertAuthority, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("read CA certificate %s: %w", certFile, err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read CA key %s: %w", keyFile, err)
	}

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse CA keypair: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return nil, fmt.Errorf("CA certificate %s contains no certificate", certFile)
	}

	caCert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	if !caCert.IsCA {
		return nil, fmt.Errorf("certificate %s is not a CA (BasicConstraints CA:FALSE)", certFile)
	}
	if caCert.KeyUsage != 0 && caCert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, fmt.Errorf("CA certificate %s lacks the certificate-signing key usage", certFile)
	}

	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("CA key %s is not usable for signing", keyFile)
	}

	return &CertAuthority{
		cert:  caCert,
		key:   signer,
		cache: make(map[string]*tls.Certificate, leafCacheSize),
	}, nil
}

// Subject returns the CA's subject common name, for logging.
func (a *CertAuthority) Subject() string { return a.cert.Subject.CommonName }

// NotAfter returns the CA certificate's expiry.
func (a *CertAuthority) NotAfter() time.Time { return a.cert.NotAfter }

// CertFor returns a certificate valid for host, minting and caching one if
// needed. host is a bare hostname or IP — no port.
func (a *CertAuthority) CertFor(host string) (*tls.Certificate, error) {
	a.mu.Lock()
	if cached, ok := a.cache[host]; ok {
		// A cached leaf outliving its validity would be served until eviction,
		// so re-mint rather than hand back something the client will reject.
		if cached.Leaf == nil || time.Now().Before(cached.Leaf.NotAfter.Add(-time.Minute)) {
			a.mu.Unlock()
			return cached, nil
		}
		delete(a.cache, host)
	}
	a.mu.Unlock()

	leaf, err := a.mint(host)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	// Another goroutine may have minted the same host concurrently; either
	// certificate is equally valid, so keep whichever landed first.
	if existing, ok := a.cache[host]; ok {
		return existing, nil
	}
	if len(a.cache) >= leafCacheSize && len(a.inOrder) > 0 {
		oldest := a.inOrder[0]
		a.inOrder = a.inOrder[1:]
		delete(a.cache, oldest)
	}
	a.cache[host] = leaf
	a.inOrder = append(a.inOrder, host)
	return leaf, nil
}

// mint signs a fresh leaf certificate for host.
func (a *CertAuthority) mint(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		// Backdated a minute so a client whose clock runs slightly behind
		// does not reject a just-minted certificate as not-yet-valid.
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(leafValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, key.Public(), a.key)
	if err != nil {
		return nil, fmt.Errorf("sign leaf certificate for %s: %w", host, err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse minted leaf for %s: %w", host, err)
	}

	return &tls.Certificate{
		// The CA certificate is included so a client that trusts a root above
		// it can still build a chain; a self-signed CA makes it redundant but
		// harmless.
		Certificate: [][]byte{der, a.cert.Raw},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}
