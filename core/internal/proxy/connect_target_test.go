package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/pkg/logger"
)

type targetResolverFunc func(context.Context, string, string) ([]net.IP, error)

func (f targetResolverFunc) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	return f(ctx, network, host)
}

type ipv4TargetResolver struct{}

func (ipv4TargetResolver) LookupIP(context.Context, string, string) ([]net.IP, error) {
	return []net.IP{net.ParseIP("192.0.2.1")}, nil
}

func TestValidateConnectTarget(t *testing.T) {
	notFound := &net.DNSError{Err: "no such host", IsNotFound: true}
	for _, tc := range []struct {
		name      string
		authority string
		a         []net.IP
		aErr      error
		aaaa      []net.IP
		aaaaErr   error
		lookups   []string
		reason    string // empty when the target passes
	}{
		{name: "A", authority: "target.example:443", a: []net.IP{net.ParseIP("192.0.2.1")}, lookups: []string{"ip4"}},
		{name: "no A or AAAA", authority: "target.example:443", aaaaErr: notFound, lookups: []string{"ip4", "ip6"}, reason: "target_dns_not_found"},
		{name: "NXDOMAIN", authority: "target.example:443", aErr: notFound, aaaaErr: notFound, lookups: []string{"ip4", "ip6"}, reason: "target_dns_not_found"},
		{name: "NODATA", authority: "target.example:443", aErr: &net.DNSError{Err: "no records of requested type", IsNotFound: true}, aaaaErr: notFound, lookups: []string{"ip4", "ip6"}, reason: "target_dns_not_found"},
		{name: "AAAA only", authority: "target.example:443", aErr: notFound, aaaa: []net.IP{net.ParseIP("2001:db8::1")}, lookups: []string{"ip4", "ip6"}, reason: "target_no_ipv4"},
		{name: "no A, AAAA timeout", authority: "target.example:443", aErr: notFound, aaaaErr: &net.DNSError{Err: "timeout", IsTimeout: true}, lookups: []string{"ip4", "ip6"}, reason: "target_no_ipv4"},
		{name: "A answer without IPv4", authority: "target.example:443", a: []net.IP{net.ParseIP("2001:db8::1")}, aaaa: []net.IP{net.ParseIP("2001:db8::1")}, lookups: []string{"ip4", "ip6"}, reason: "target_no_ipv4"},
		{name: "DNS timeout", authority: "target.example:443", aErr: &net.DNSError{Err: "timeout", IsTimeout: true}, lookups: []string{"ip4"}},
		{name: "SERVFAIL", authority: "target.example:443", aErr: &net.DNSError{Err: "server misbehaving", IsTemporary: true}, lookups: []string{"ip4"}},
		{name: "lookup deadline", authority: "target.example:443", aErr: context.DeadlineExceeded, lookups: []string{"ip4"}},
		{name: "resolver error", authority: "target.example:443", aErr: errors.New("resolver unavailable"), lookups: []string{"ip4"}},
		{name: "IPv4 literal", authority: "192.0.2.1:443"},
		{name: "IPv4 mapped literal", authority: "[::ffff:192.0.2.1]:443"},
		{name: "IPv6 literal", authority: "[2001:db8::1]:443", reason: "target_no_ipv4"},
		{name: "missing port", authority: "target.example", reason: "target_invalid"},
		{name: "missing host", authority: ":443", reason: "target_invalid"},
		{name: "named port", authority: "target.example:https", reason: "target_invalid"},
		{name: "port zero", authority: "target.example:0", reason: "target_invalid"},
		{name: "port out of range", authority: "target.example:65536", reason: "target_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lookups []string
			chain := &PoolChain{targetResolver: targetResolverFunc(func(ctx context.Context, network, host string) ([]net.IP, error) {
				lookups = append(lookups, network)
				if host != "target.example" {
					t.Fatalf("lookup %q, want target.example", host)
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > connectTargetLookupTimeout {
					t.Fatal("target lookup is not bounded")
				}
				switch network {
				case "ip4":
					return tc.a, tc.aErr
				case "ip6":
					return tc.aaaa, tc.aaaaErr
				}
				t.Fatalf("unexpected lookup network %q", network)
				return nil, nil
			})}
			err := chain.validateConnectTarget(context.Background(), tc.authority)
			if fmt.Sprint(lookups) != fmt.Sprint(tc.lookups) {
				t.Fatalf("lookups = %v, want %v", lookups, tc.lookups)
			}
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("valid target rejected: %v", err)
				}
				return
			}
			if !isTargetConnectFailure(err) || forwardingReason(err) != tc.reason {
				t.Fatalf("got %q (%v), want %s", forwardingReason(err), err, tc.reason)
			}
			if tc.reason == "target_dns_not_found" && !errors.Is(err, tc.aaaaErr) {
				t.Fatalf("lost resolver cause: %v", err)
			}
		})
	}
}

func TestConnectTargetLookupHonorsContext(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if canceled {
				cancel()
			}
			chain, selector := chainWithProxy(7)
			chain.targetResolver = targetResolverFunc(func(lookupCtx context.Context, _, _ string) ([]net.IP, error) {
				deadline, _ := ctx.Deadline()
				lookupDeadline, _ := lookupCtx.Deadline()
				if !deadline.Equal(lookupDeadline) {
					t.Fatal("lookup did not inherit the earlier client deadline")
				}
				<-lookupCtx.Done()
				return nil, lookupCtx.Err()
			})
			_, _, err := chain.ConnectWithRetry("target.example:443", ctx, nil, logger.New("error"))
			if !errors.Is(err, ctx.Err()) || isTargetConnectFailure(err) {
				t.Fatalf("context error = %v", err)
			}
			if proxyCount(selector) != 1 || len(chain.failCounts) != 0 {
				t.Fatal("canceled request attempted or charged a proxy")
			}
		})
	}
}

func TestConnectTargetLookupFailureFallsThroughToUpstream(t *testing.T) {
	for _, lookupErr := range []error{
		&net.DNSError{Err: "timeout", IsTimeout: true},
		&net.DNSError{Err: "server misbehaving", IsTemporary: true},
		context.DeadlineExceeded,
	} {
		t.Run(lookupErr.Error(), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host != "target.example:443" {
					t.Errorf("lost target hostname: %s", r.Host)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()
			chain, selector := chainWithProxy(7)
			selector.proxies[0].Address = upstream.Listener.Addr().String()
			chain.targetResolver = targetResolverFunc(func(context.Context, string, string) ([]net.IP, error) {
				return nil, lookupErr
			})
			conn, binding, err := chain.ConnectWithRetry("target.example:443", context.Background(), nil, logger.New("error"))
			if err != nil {
				t.Fatalf("local DNS error prevented upstream CONNECT: %v", err)
			}
			defer conn.Close()
			if binding.ProxyID != 7 || len(chain.failCounts) != 0 {
				t.Fatalf("unexpected binding or proxy strike: %+v", binding)
			}
		})
	}
}

func TestConnectTargetFailureDoesNotSelectProxy(t *testing.T) {
	for _, tc := range []struct {
		authority string
		aaaa      []net.IP
		reason    string
	}{
		{authority: "target.example:443", reason: "target_dns_not_found"},
		{authority: "target.example:443", aaaa: []net.IP{net.ParseIP("2001:db8::1")}, reason: "target_no_ipv4"},
		{authority: "[2001:db8::1]:443", reason: "target_no_ipv4"},
		{authority: "target.example:0", reason: "target_invalid"},
	} {
		for _, method := range []string{"roundrobin", "session"} {
			t.Run(tc.authority+"/"+tc.reason+"/"+method, func(t *testing.T) {
				sm := NewSessionManager()
				defer sm.Stop()
				selector := newSessionSelector(sm, 1, 2)
				selector.method = method
				chain := &PoolChain{
					selectors: []*PoolSelector{selector},
					tracker:   &UsageTracker{}, // Any attempt to record or update health is a bug.
					targetResolver: targetResolverFunc(func(_ context.Context, network, _ string) ([]net.IP, error) {
						if network == "ip6" && tc.aaaa != nil {
							return tc.aaaa, nil
						}
						return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
					}),
					failCounts: map[int]int{1: 2, 2: 2},
				}
				req := httptest.NewRequest(http.MethodConnect, tc.authority, nil)
				req = req.WithContext(context.WithValue(ctxWithToken("test"), UserChainContextKey, chain))
				w := httptest.NewRecorder()
				NewUpstreamProxyHandler(nil, nil, logger.New("error")).HandleConnectRequest(w, req)
				if w.Code != 592 || w.Header().Get(ProxyErrorHeader) != tc.reason || w.Header().Get(UpstreamStatusHeader) != "" {
					t.Fatalf("response = %d %v", w.Code, w.Header())
				}
				if selector.rrIdx != 0 || len(sm.List()) != 0 {
					t.Fatal("target failure consumed a rotation slot or session reservation")
				}
				if proxyCount(selector) != 2 || chain.failCounts[1] != 2 || chain.failCounts[2] != 2 {
					t.Fatal("target failure changed proxy health")
				}
			})
		}
	}
}
