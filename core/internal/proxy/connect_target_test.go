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
	for _, tc := range []struct {
		name      string
		authority string
		ips       []net.IP
		err       error
		lookup    bool
		rejected  bool
	}{
		{name: "A", authority: "target.example:443", ips: []net.IP{net.ParseIP("192.0.2.1")}, lookup: true},
		{name: "no A", authority: "target.example:443", lookup: true, rejected: true},
		{name: "AAAA only", authority: "target.example:443", ips: []net.IP{net.ParseIP("2001:db8::1")}, lookup: true, rejected: true},
		{name: "NXDOMAIN", authority: "target.example:443", err: &net.DNSError{Err: "no such host", IsNotFound: true}, lookup: true, rejected: true},
		{name: "DNS timeout", authority: "target.example:443", err: &net.DNSError{Err: "timeout", IsTimeout: true}, lookup: true, rejected: true},
		{name: "resolver error", authority: "target.example:443", err: errors.New("resolver unavailable"), lookup: true, rejected: true},
		{name: "IPv4 literal", authority: "192.0.2.1:443"},
		{name: "IPv4 mapped literal", authority: "[::ffff:192.0.2.1]:443"},
		{name: "IPv6 literal", authority: "[2001:db8::1]:443", rejected: true},
		{name: "missing port", authority: "target.example", rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			chain := &PoolChain{targetResolver: targetResolverFunc(func(ctx context.Context, network, host string) ([]net.IP, error) {
				calls++
				if network != "ip4" || host != "target.example" {
					t.Fatalf("lookup %q %q, want ip4 target.example", network, host)
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > connectTargetLookupTimeout {
					t.Fatal("target lookup is not bounded")
				}
				return tc.ips, tc.err
			})}
			err := chain.validateConnectTarget(context.Background(), tc.authority)
			if (err != nil) != tc.rejected || (calls == 1) != tc.lookup {
				t.Fatalf("error = %v, lookup calls = %d", err, calls)
			}
			if tc.rejected && (!isTargetConnectFailure(err) || forwardingReason(err) != "proxy_connect_rejected") {
				t.Fatalf("misclassified target failure: %v", err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
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
			chain := &PoolChain{targetResolver: targetResolverFunc(func(lookupCtx context.Context, _, _ string) ([]net.IP, error) {
				deadline, _ := ctx.Deadline()
				lookupDeadline, _ := lookupCtx.Deadline()
				if !deadline.Equal(lookupDeadline) {
					t.Fatal("lookup did not inherit the earlier client deadline")
				}
				<-lookupCtx.Done()
				return nil, lookupCtx.Err()
			})}
			_, _, err := chain.ConnectWithRetry("target.example:443", ctx, nil, logger.New("error"))
			if !errors.Is(err, ctx.Err()) || forwardingReason(err) != "proxy_connect_rejected" {
				t.Fatalf("context error = %v", err)
			}
		})
	}
}

func TestConnectTargetFailureDoesNotSelectProxy(t *testing.T) {
	for _, method := range []string{"roundrobin", "session"} {
		t.Run(method, func(t *testing.T) {
			sm := NewSessionManager()
			defer sm.Stop()
			selector := newSessionSelector(sm, 1, 2)
			selector.method = method
			chain := &PoolChain{
				selectors: []*PoolSelector{selector},
				tracker:   &UsageTracker{}, // Any attempt to record or update health is a bug.
				targetResolver: targetResolverFunc(func(context.Context, string, string) ([]net.IP, error) {
					return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
				}),
				failCounts: map[int]int{1: 2, 2: 2},
			}
			req := httptest.NewRequest(http.MethodConnect, "target.example:443", nil)
			req = req.WithContext(context.WithValue(ctxWithToken("test"), UserChainContextKey, chain))
			w := httptest.NewRecorder()
			NewUpstreamProxyHandler(nil, nil, logger.New("error")).HandleConnectRequest(w, req)
			if w.Code != 592 || w.Header().Get(ProxyErrorHeader) != "proxy_connect_rejected" {
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
