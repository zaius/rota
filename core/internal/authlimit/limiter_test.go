package authlimit

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestFailureWindowAndBlockExpiry(t *testing.T) {
	now := time.Now()
	l := New(2, time.Minute, 90*time.Second)
	l.now = func() time.Time { return now }
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	l.Failed(r)
	now = now.Add(time.Minute) // The first failure falls outside the window.
	l.Failed(r)
	if l.RetryAfter(r) != 0 {
		t.Fatal("expired failure counted toward the block")
	}
	l.Failed(r)
	if got := l.RetryAfter(r); got != 90 {
		t.Fatalf("block = %ds, want 90", got)
	}
	now = now.Add(1500 * time.Millisecond)
	l.Failed(r) // A concurrent failure must not prolong the block.
	if got := l.RetryAfter(r); got != 89 {
		t.Fatalf("remaining block = %ds, want 89", got)
	}
	now = now.Add(88500 * time.Millisecond)
	if l.RetryAfter(r) != 0 {
		t.Fatal("block did not expire")
	}
	l.Failed(r)
	if l.RetryAfter(r) != 0 {
		t.Fatal("expired block retained old failures")
	}
}

func TestLoginCountsOnlyUnauthorized(t *testing.T) {
	l := New(2, time.Minute, time.Minute)
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	status, calls := http.StatusOK, 0
	h := l.Login(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(status)
	}))
	for _, code := range []int{200, 400, 403, 500} {
		status = code
		for range 1100 {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != code {
				t.Fatalf("status %d counted as an auth failure: %d", code, w.Code)
			}
		}
	}
	status = http.StatusUnauthorized
	for range 2 {
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	before := calls
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" || calls != before {
		t.Fatalf("blocked login reached authentication: %d %v calls=%d", w.Code, w.Header(), calls)
	}
}

func TestClientIPAndConcurrentFailures(t *testing.T) {
	l := New(10, time.Minute, time.Minute)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "[2001:db8::1]:1234"
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { l.Failed(r) })
	}
	wg.Wait()
	r.RemoteAddr = "[2001:db8::1]:9876"
	r.Header.Set("X-Forwarded-For", "203.0.113.1")
	r.Header.Set("X-Real-IP", "203.0.113.2")
	if l.RetryAfter(r) == 0 {
		t.Fatal("a port or forwarded-header change bypassed the block")
	}
	r.RemoteAddr = "[2001:db8::2]:1234"
	if l.RetryAfter(r) != 0 {
		t.Fatal("blocked a different IP")
	}
}

func TestExpiredEntriesCleanup(t *testing.T) {
	now := time.Now()
	l := New(2, time.Minute, time.Minute)
	l.now = func() time.Time { return now }
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.1:1"
	l.Failed(r)
	r.RemoteAddr = "192.0.2.2:1"
	l.Failed(r)
	l.Failed(r)
	now = now.Add(time.Minute)
	r.RemoteAddr = "192.0.2.3:1"
	l.Failed(r)
	if len(l.entries) != 1 {
		t.Fatalf("retained expired entries: %v", l.entries)
	}
}
