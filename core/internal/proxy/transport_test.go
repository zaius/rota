package proxy

import (
	"net/http"
	"testing"

	"github.com/alpkeskin/rota/core/internal/models"
)

func TestGetOrCreateTransport_CachesPerProxy(t *testing.T) {
	p := &models.Proxy{
		ID:       1,
		Address:  "127.0.0.1:8080",
		Protocol: "http",
	}

	t1, err := GetOrCreateTransport(p)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	t2, err := GetOrCreateTransport(p)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if t1 != t2 {
		t.Fatal("expected same transport instance from cache")
	}
}

func TestGetOrCreateTransport_DifferentProxies(t *testing.T) {
	p1 := &models.Proxy{
		ID:       1,
		Address:  "127.0.0.1:8081",
		Protocol: "http",
	}
	p2 := &models.Proxy{
		ID:       2,
		Address:  "127.0.0.1:8082",
		Protocol: "http",
	}

	t1, err := GetOrCreateTransport(p1)
	if err != nil {
		t.Fatalf("proxy1: %v", err)
	}

	t2, err := GetOrCreateTransport(p2)
	if err != nil {
		t.Fatalf("proxy2: %v", err)
	}

	if t1 == t2 {
		t.Fatal("expected different transport instances for different proxies")
	}
}

func TestCreateProxyTransport_HTTP(t *testing.T) {
	p := &models.Proxy{
		Address:  "127.0.0.1:3128",
		Protocol: "http",
	}
	tr, err := CreateProxyTransport(p)
	if err != nil {
		t.Fatalf("CreateProxyTransport: %v", err)
	}
	if tr.Proxy == nil {
		t.Fatal("HTTP proxy transport should have Proxy function set")
	}
}

func TestCreateProxyTransport_SOCKS5(t *testing.T) {
	p := &models.Proxy{
		Address:  "127.0.0.1:1080",
		Protocol: "socks5",
	}
	tr, err := CreateProxyTransport(p)
	if err != nil {
		t.Fatalf("CreateProxyTransport: %v", err)
	}
	if tr.Dial == nil {
		t.Fatal("SOCKS5 transport should have Dial function set")
	}
}

func TestCreateProxyTransport_UnsupportedProtocol(t *testing.T) {
	p := &models.Proxy{
		Address:  "127.0.0.1:9999",
		Protocol: "ftp",
	}
	_, err := CreateProxyTransport(p)
	if err == nil {
		t.Fatal("expected error for unsupported protocol")
	}
}

func TestCreateProxyTransport_WithAuth(t *testing.T) {
	user := "myuser"
	pass := "mypass"
	p := &models.Proxy{
		Address:  "127.0.0.1:3128",
		Protocol: "http",
		Username: &user,
		Password: &pass,
	}
	tr, err := CreateProxyTransport(p)
	if err != nil {
		t.Fatalf("CreateProxyTransport with auth: %v", err)
	}
	if tr.Proxy == nil {
		t.Fatal("should have Proxy set")
	}
}

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

// Two proxies on the same host:port with different credentials must not share
// a transport.
func TestGetOrCreateTransport_DistinctCredentialsDoNotCollide(t *testing.T) {
	ClearTransportCache()

	a := &models.Proxy{ID: 1, Address: "127.0.0.1:8082", Protocol: "http", Username: strptr("alice"), Password: strptr("pw")}
	b := &models.Proxy{ID: 2, Address: "127.0.0.1:8082", Protocol: "http", Username: strptr("bob"), Password: strptr("pw")}

	ta, err := GetOrCreateTransport(a)
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	tb, err := GetOrCreateTransport(b)
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	if ta == tb {
		t.Fatal("expected different credentials to key different transports")
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

// Invalidating one endpoint must leave other proxies' warm pools alone.
func TestInvalidateTransport_LeavesOtherEndpointsCached(t *testing.T) {
	ClearTransportCache()

	target := &models.Proxy{ID: 1, Address: "127.0.0.1:8084", Protocol: "http"}
	other := &models.Proxy{ID: 2, Address: "127.0.0.1:8085", Protocol: "http"}

	if _, err := GetOrCreateTransport(target); err != nil {
		t.Fatalf("cache target: %v", err)
	}
	kept, err := GetOrCreateTransport(other)
	if err != nil {
		t.Fatalf("cache other: %v", err)
	}

	InvalidateTransport(target)

	if _, ok := transportCache.Load(transportCacheKey(target)); ok {
		t.Fatal("expected the target transport to be evicted")
	}
	got, ok := transportCache.Load(transportCacheKey(other))
	if !ok || got.(*http.Transport) != kept {
		t.Fatal("expected the untouched endpoint to keep its cached transport")
	}
}

func TestClearTransportCache(t *testing.T) {
	ClearTransportCache()

	p := &models.Proxy{ID: 1, Address: "127.0.0.1:8086", Protocol: "http"}
	if _, err := GetOrCreateTransport(p); err != nil {
		t.Fatalf("cache: %v", err)
	}

	ClearTransportCache()

	if _, ok := transportCache.Load(transportCacheKey(p)); ok {
		t.Fatal("expected the cache to be empty")
	}
}
