package proxy

import (
	"testing"

	"github.com/zaius/rota/core/internal/models"
)

func strptr(s string) *string { return &s }

// A credential rotation must not keep serving the transport built with the old
// credentials.
func TestGetOrCreateTransport_CredentialChangeYieldsNewTransport(t *testing.T) {
	ClearTransportCache()

	old := &models.Proxy{ID: 1, Address: "127.0.0.1:8081", Protocol: "http", Username: strptr("u"), Password: strptr("old")}
	rotated := &models.Proxy{ID: 1, Address: "127.0.0.1:8081", Protocol: "http", Username: strptr("u"), Password: strptr("new")}

	t1, err := GetOrCreateTransport(old)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	t2, err := GetOrCreateTransport(rotated)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if t1 == t2 {
		t.Fatal("expected a rotated credential to produce a distinct transport")
	}
}

func TestInvalidateTransport_DropsEveryCredentialForEndpoint(t *testing.T) {
	ClearTransportCache()

	old := &models.Proxy{ID: 1, Address: "127.0.0.1:8083", Protocol: "http", Username: strptr("u"), Password: strptr("old")}
	rotated := &models.Proxy{ID: 1, Address: "127.0.0.1:8083", Protocol: "http", Username: strptr("u"), Password: strptr("new")}

	if _, err := GetOrCreateTransport(old); err != nil {
		t.Fatalf("cache old: %v", err)
	}
	if _, err := GetOrCreateTransport(rotated); err != nil {
		t.Fatalf("cache rotated: %v", err)
	}

	InvalidateTransport(rotated)

	for _, p := range []*models.Proxy{old, rotated} {
		if _, ok := transportCache.Load(transportCacheKey(p)); ok {
			t.Fatalf("expected transport for %q to be evicted", transportCacheKey(p))
		}
	}
}
