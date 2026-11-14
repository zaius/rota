package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zaius/rota/core/internal/models"
)

// transportCache caches *http.Transport per proxy to avoid creating a new
// transport (and a new connection pool) for every request.
var transportCache sync.Map

// transportCacheKey builds the cache key for a proxy. Credentials are part of
// the key so that two proxies sharing a host:port but authenticating differently
// do not collide, and so rotating a credential yields a different key rather
// than silently reusing a transport that still carries the old one.
func transportCacheKey(p *models.Proxy) string {
	user := ""
	if p.Username != nil {
		user = *p.Username
	}
	pass := ""
	if p.Password != nil {
		pass = *p.Password
	}
	return fmt.Sprintf("%s://%s|%s:%s", p.Protocol, p.Address, user, pass)
}

// transportCacheKeyPrefix matches every cached key for a proxy endpoint,
// whatever credentials it was cached under.
func transportCacheKeyPrefix(p *models.Proxy) string {
	return p.Protocol + "://" + p.Address + "|"
}

// GetOrCreateTransport returns a cached transport for the given proxy,
// or creates and caches a new one.
func GetOrCreateTransport(p *models.Proxy) (*http.Transport, error) {
	key := transportCacheKey(p)
	if t, ok := transportCache.Load(key); ok {
		return t.(*http.Transport), nil
	}
	t, err := CreateProxyTransport(p)
	if err != nil {
		return nil, err
	}
	actual, _ := transportCache.LoadOrStore(key, t)
	return actual.(*http.Transport), nil
}

// dropTransport removes a cache entry and releases the keep-alive connections
// its transport is still holding open.
func dropTransport(key any) {
	if t, ok := transportCache.LoadAndDelete(key); ok {
		if tr, ok := t.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
}

// InvalidateTransport drops every cached transport for a proxy endpoint,
// whatever credentials it was cached under. Call this when a proxy is updated
// or deleted so the next request rebuilds from current settings.
func InvalidateTransport(p *models.Proxy) {
	prefix := transportCacheKeyPrefix(p)
	transportCache.Range(func(k, _ any) bool {
		if ks, ok := k.(string); ok && strings.HasPrefix(ks, prefix) {
			dropTransport(ks)
		}
		return true
	})
}

// ClearTransportCache drops all cached transports. Used after bulk mutations,
// where tracking individual endpoints is not worth the bookkeeping.
func ClearTransportCache() {
	transportCache.Range(func(k, _ any) bool {
		dropTransport(k)
		return true
	})
}

// ConfigureCheckTLS prepares a proxy transport for a health check. Strict mode
// verifies the target's certificate chain, so a proxy that intercepts TLS with
// an expired or forged certificate fails; otherwise any certificate passes.
// Either way checks use Go's default TLS versions and cipher suites, not the
// TLS 1.0 minimum proxied traffic keeps for legacy upstreams.
//
// net/http applies TLSClientConfig to the hop to an https proxy as well as to
// the target, so this dials an https proxy's own hop itself, with the lenient
// handshake proxied traffic uses, and the check settings reach the target only.
func ConfigureCheckTLS(t *http.Transport, p *models.Proxy, strict bool) {
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{}
	}
	c := t.TLSClientConfig
	c.MinVersion = 0
	c.MaxVersion = 0
	c.CipherSuites = nil
	c.InsecureSkipVerify = !strict
	c.VerifyPeerCertificate = nil

	if p.Protocol != "https" {
		return
	}
	u := proxyURL(p)
	u.Scheme = "http"
	t.Proxy = http.ProxyURL(u)
	// Every request goes through the proxy, so every dial is to the proxy.
	t.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return dialProxy(ctx, p) }
}

// proxyURL is p's address as a URL carrying its credentials.
func proxyURL(p *models.Proxy) *url.URL {
	u := &url.URL{Scheme: p.Protocol, Host: p.Address}
	if p.Username != nil && *p.Username != "" {
		pass := ""
		if p.Password != nil {
			pass = *p.Password
		}
		u.User = url.UserPassword(*p.Username, pass)
	}
	return u
}

// CreateProxyTransport creates an HTTP transport configured for the given proxy
// This is shared between proxy handler and health checker
func CreateProxyTransport(p *models.Proxy) (*http.Transport, error) {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,             // Skip certificate verification for proxy connections
			MinVersion:         tls.VersionTLS10, // Support older TLS versions for compatibility
			MaxVersion:         0,                // Allow all TLS versions
			// Don't specify CipherSuites to accept all available ciphers for maximum compatibility
			// This is acceptable since InsecureSkipVerify is already true
		},
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 10 * time.Second,
	}

	switch p.Protocol {
	case "http", "https":
		// The transport dials the proxy with DialContext, then speaks to it as
		// Proxy says, credentials included.
		transport.Proxy = http.ProxyURL(proxyURL(p))
		transport.DialContext = proxyDial.DialContext
	case "socks4", "socks4a", "socks5":
		transport.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialTarget(ctx, p, addr)
		}
	default:
		return nil, fmt.Errorf("unsupported proxy protocol: %s", p.Protocol)
	}

	return transport, nil
}
