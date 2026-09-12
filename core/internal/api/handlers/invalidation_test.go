package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/proxy"
	"github.com/go-chi/chi/v5"
)

type cooldownRepo struct {
	proxyControlRepository
	durations []time.Duration
}

type reactivationRepo struct {
	proxyControlRepository
	scopes             []string
	global, allDomains bool
	err                error
}

func (r *reactivationRepo) ClearCooldown(_ context.Context, id int) (*models.Proxy, error) {
	r.global = true
	return &models.Proxy{ID: id}, nil
}

func (r *reactivationRepo) ClearAllDomainCooldowns(context.Context, int) (int, error) {
	r.allDomains = true
	return 1, nil
}

func (r *reactivationRepo) ClearScopeCooldowns(_ context.Context, _ int, scope string) (int, error) {
	r.scopes = append(r.scopes, scope)
	return 1, r.err
}

func TestReactivateProxy_ScopeAndFullReactivation(t *testing.T) {
	for _, tt := range []struct {
		body, scope string
		global      bool
	}{
		{`{"scope":"Shopping."}`, "shopping", false}, {`{}`, "", true},
	} {
		ps := &fakeProxyServer{}
		h := newTestControlHandler(ps)
		repo := &reactivationRepo{}
		h.proxyRepo = repo
		router := chi.NewRouter()
		router.Post("/{id}", h.ReactivateProxy)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("POST", "/42", strings.NewReader(tt.body)))
		if w.Code != http.StatusOK || len(repo.scopes) != 1 || repo.scopes[0] != tt.scope || repo.global != tt.global || repo.allDomains != tt.global {
			t.Fatalf("reactivation failed: %d %+v", w.Code, repo)
		}
		if len(ps.clearedScopes) != 1 || ps.clearedScopes[0].ProxyID != 42 || ps.clearedScopes[0].Scope != tt.scope {
			t.Fatalf("incorrect live reactivation: %+v", ps.clearedScopes)
		}
	}
}

func TestReactivateProxy_ScopePersistenceFailure(t *testing.T) {
	ps := &fakeProxyServer{}
	h := newTestControlHandler(ps)
	h.proxyRepo = &reactivationRepo{err: errors.New("database unavailable")}
	router := chi.NewRouter()
	router.Post("/{id}", h.ReactivateProxy)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/42", strings.NewReader(`{"scope":"shopping"}`)))
	if w.Code != http.StatusInternalServerError || len(ps.clearedScopes) != 0 {
		t.Fatalf("live cooldown cleared despite persistence failure: %d %+v", w.Code, ps.clearedScopes)
	}
}

func (r *cooldownRepo) SetCooldown(_ context.Context, id int, d time.Duration, _ string) (*models.Proxy, error) {
	r.durations = append(r.durations, d)
	until := time.Now().Add(d)
	return &models.Proxy{ID: id, CooldownUntil: &until}, nil
}
func (r *cooldownRepo) SetScopeCooldown(_ context.Context, id int, _ string, until time.Time, _ string) (*models.Proxy, error) {
	r.durations = append(r.durations, time.Until(until))
	return &models.Proxy{ID: id}, nil
}
func (r *cooldownRepo) SetDomainCooldown(_ context.Context, id int, _ string, until time.Time, _ string) (*models.Proxy, error) {
	r.durations = append(r.durations, time.Until(until))
	return &models.Proxy{ID: id}, nil
}

func TestInvalidateSession_DefaultScopeAndOverrides(t *testing.T) {
	for _, tt := range []struct {
		name, body               string
		scopes, domains, globals int
		minutes                  int
	}{
		{"default", `{"token":"job42"}`, 2, 0, 0, 30},
		{"scope filter and duration", `{"token":"job42","scope":"Shopping.","minutes":7}`, 1, 0, 0, 7},
		{"domain override", `{"token":"job42","domain":"https://Foo.COM/path","minutes":45}`, 0, 1, 0, 45},
		{"global override", `{"token":"job42","global":true,"minutes":12}`, 0, 0, 1, 12},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ps := &fakeProxyServer{sessions: []proxy.SessionInfo{
				{Token: "job42", Username: "alice", PoolID: 1, Scope: "shopping", ProxyID: 42},
				{Token: "job42", Username: "alice", PoolID: 2, Scope: "shopping", ProxyID: 42},
				{Token: "job42", Username: "alice", PoolID: 1, Scope: "foo.com", ProxyID: 42},
				{Token: "job42", Username: "bob", PoolID: 1, Scope: "foreign", ProxyID: 99},
				{Token: "job42", Username: "alice", PoolID: 99, Scope: "foreign-pool", ProxyID: 100},
			}}
			repo := &cooldownRepo{}
			h := newTestControlHandler(ps)
			h.proxyRepo = repo
			w := httptest.NewRecorder()
			h.InvalidateSession(w, asProxyUser(httptest.NewRequest("POST", "/sessions/invalidate", strings.NewReader(tt.body)), 1, 2))
			if w.Code != http.StatusOK {
				t.Fatalf("invalidation failed: %d %s", w.Code, w.Body.String())
			}
			if len(ps.cooldowns) != tt.scopes || len(ps.domainCooldowns) != tt.domains || len(ps.evicted) != tt.globals {
				t.Fatalf("wrong effects: scopes=%v domains=%v evicted=%v", ps.cooldowns, ps.domainCooldowns, ps.evicted)
			}
			if len(repo.durations) != tt.scopes+tt.domains+tt.globals {
				t.Fatalf("duplicate or out-of-scope writes: %v", repo.durations)
			}
			for _, d := range repo.durations {
				want := time.Duration(tt.minutes) * time.Minute
				if d > want || d < want-time.Second {
					t.Fatalf("wrong caller duration: %v, want %v", d, want)
				}
			}
			if tt.domains > 0 && ps.domainCooldowns[0].Domain != "foo.com" {
				t.Fatal("domain not normalized")
			}
			var response struct {
				Proxies []map[string]any `json:"proxies"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Proxies) != len(repo.durations) {
				t.Fatalf("wrong response: %s", w.Body.String())
			}
		})
	}
}

func TestInvalidation_RejectsInvalidInputBeforeMutation(t *testing.T) {
	for _, body := range []string{
		`{"minutes":0}`, `{"minutes":-1}`, `{"minutes":153722868}`,
		`{"minutes":"10"}`, `{"minutes":1.5}`, `{"minutes":9223372036854775808}`,
		`{"domain":"/"}`, `{"domain":"foo.com","global":true}`, `{"minutes":4,`,
	} {
		for _, session := range []bool{false, true} {
			t.Run(body+map[bool]string{false: "/proxy", true: "/session"}[session], func(t *testing.T) {
				h := newTestControlHandler(&fakeProxyServer{})
				requestBody := body
				if session {
					requestBody = strings.Replace(body, "{", `{"token":"job42",`, 1)
				}
				r := httptest.NewRequest("POST", "/", strings.NewReader(requestBody))
				w := httptest.NewRecorder()
				if session {
					h.InvalidateSession(w, r)
				} else {
					router := chi.NewRouter()
					router.Post("/{id}", h.InvalidateProxy)
					r.URL.Path = "/42"
					router.ServeHTTP(w, r)
				}
				if w.Code != http.StatusBadRequest {
					t.Fatalf("got %d: %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestInvalidateProxy_DefaultAndCallerDuration(t *testing.T) {
	for _, tt := range []struct {
		body    string
		minutes int
	}{{"", 30}, {`{}`, 30}, {`{"minutes":5}`, 5}, {`{"minutes":9,"domain":"foo.com"}`, 9}} {
		ps := &fakeProxyServer{}
		h := newTestControlHandler(ps)
		repo := &cooldownRepo{}
		h.proxyRepo = repo
		router := chi.NewRouter()
		router.Post("/{id}", h.InvalidateProxy)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("POST", "/42", strings.NewReader(tt.body)))
		if w.Code != http.StatusOK || len(repo.durations) != 1 {
			t.Fatalf("failed: %d %s", w.Code, w.Body.String())
		}
		want := time.Duration(tt.minutes) * time.Minute
		if d := repo.durations[0]; d > want || d < want-time.Second {
			t.Fatalf("duration = %v, want %v", d, want)
		}
	}
}
