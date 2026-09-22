package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/api/handlers"
	"github.com/alpkeskin/rota/core/internal/authlimit"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/proxy"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
)

type testAuthenticator func(context.Context, string, string) (*models.ProxyUser, error)

func (f testAuthenticator) Authenticate(ctx context.Context, username, password string) (*models.ProxyUser, error) {
	return f(ctx, username, password)
}

type releaseServer struct {
	handlers.ProxyServer
	count int
}

func (s *releaseServer) ReleaseSessions(proxy.SessionFilter) int {
	s.count++
	return 1
}

func adminToken(t *testing.T) string {
	t.Helper()
	token, err := jwt.New(jwt.SigningMethodHS256).SignedString([]byte("test"))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestReleaseHasNoRequestLimit(t *testing.T) {
	for _, auth := range []string{"basic", "bearer"} {
		t.Run(auth, func(t *testing.T) {
			log := logger.New("error")
			ps := &releaseServer{}
			h := handlers.NewProxyControlHandler(nil, nil, log)
			h.SetProxyServer(ps)
			s := &Server{
				router: chi.NewRouter(), jwtSecret: "test", logger: log,
				authLimiter:         authlimit.New(2, time.Minute, time.Minute),
				proxyControlHandler: h,
				userRepo: testAuthenticator(func(context.Context, string, string) (*models.ProxyUser, error) {
					id := 1
					return &models.ProxyUser{Username: "alice", MainPoolID: &id}, nil
				}),
			}
			s.setupRoutes()
			token := adminToken(t)
			for i := range 1500 {
				body, want := `{"token":"job42"}`, 200
				if auth == "basic" && i < 20 {
					body, want = `{"token":"job42","pool_id":99}`, 403
				}
				r := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/release", strings.NewReader(body))
				if auth == "basic" {
					r.SetBasicAuth("alice", "secret")
				} else {
					r.Header.Set("Authorization", "Bearer "+token)
				}
				w := httptest.NewRecorder()
				s.router.ServeHTTP(w, r)
				if w.Code != want {
					t.Fatalf("request %d returned %d, want %d: %s", i, w.Code, want, w.Body.String())
				}
			}
			want := 1500
			if auth == "basic" {
				want -= 20
			}
			if ps.count != want {
				t.Fatalf("released %d sessions, want %d", ps.count, want)
			}
		})
	}
}

func TestControlAuthSharesFailureBlocksAndAllowsValidJWT(t *testing.T) {
	l := authlimit.New(2, time.Minute, time.Minute)
	calls := 0
	repo := testAuthenticator(func(context.Context, string, string) (*models.ProxyUser, error) {
		calls++
		return nil, repository.ErrProxyAuthentication
	})
	h := JWTOrProxyUserMiddleware("test", repo, logger.New("error"), l)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(204)
	}))
	// A request without credentials gets the challenge without consuming
	// budget: Basic clients often send credentials only after a 401.
	for range 1500 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/sessions/release", nil))
		if w.Code != 401 || w.Header().Get("Retry-After") != "" {
			t.Fatalf("bare request counted as a failure: %d %v", w.Code, w.Header())
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/release", nil)
	r.SetBasicAuth("alice", "wrong")
	// A login failure and a control-API failure share one IP budget.
	l.Login(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) })).ServeHTTP(httptest.NewRecorder(), r)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("first control failure = %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 429 || calls != 1 || w.Header().Get("Retry-After") != "60" {
		t.Fatalf("block did not skip auth: status=%d calls=%d headers=%v", w.Code, calls, w.Header())
	}
	r.Header.Set("Authorization", "Bearer "+adminToken(t))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("blocked a valid admin token: %d", w.Code)
	}
}

func TestControlAuthTrustsForwardedIPOnlyWhenConfigured(t *testing.T) {
	for _, trust := range []bool{false, true} {
		s := &Server{router: chi.NewRouter(), logger: logger.New("error"), trustProxyHeaders: trust}
		s.setupMiddleware()
		repo := testAuthenticator(func(context.Context, string, string) (*models.ProxyUser, error) {
			return nil, repository.ErrProxyAuthentication
		})
		s.router.With(JWTOrProxyUserMiddleware("test", repo, s.logger, authlimit.New(1, time.Minute, time.Minute))).Post("/auth", func(http.ResponseWriter, *http.Request) {
			t.Fatal("invalid credentials reached handler")
		})
		for i, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.2"} {
			r := httptest.NewRequest(http.MethodPost, "/auth", nil)
			r.SetBasicAuth("alice", "wrong")
			r.Header.Set("X-Forwarded-For", ip)
			w := httptest.NewRecorder()
			s.router.ServeHTTP(w, r)
			want := 429
			if i == 0 || (trust && i == 1) {
				want = 401
			}
			if w.Code != want {
				t.Fatalf("trust=%v request=%d: got %d, want %d", trust, i, w.Code, want)
			}
		}
	}
}
