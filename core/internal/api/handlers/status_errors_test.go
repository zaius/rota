package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestHandlers_DatabaseFailureIsInternalError(t *testing.T) {
	// A closed pool exercises real repository errors without a running database.
	pool, err := pgxpool.New(context.Background(), "postgres://localhost/rota_test")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	db := &database.DB{Pool: pool}
	log := logger.New("error")
	pools := NewPoolHandler(repository.NewPoolRepository(db), nil, log)
	users := NewUserHandler(repository.NewUserRepository(db), nil, log)
	users.SetOnUserChanged(func(string) { t.Error("failed update must not notify user changes") })
	sources := NewSourceHandler(repository.NewSourceRepository(db), nil, nil, log)
	auth := NewAuthHandler(nil, repository.NewAdminRepository(db), log, "test", "", "")

	tests := []struct {
		name    string
		method  string
		route   string
		path    string
		body    string
		handler http.HandlerFunc
		message string
	}{
		{"get pool", "GET", "/pools/{id}", "/pools/1", "", pools.Get, "failed to get pool"},
		{"update pool", "PUT", "/pools/{id}", "/pools/1", `{}`, pools.Update, "failed to update pool"},
		{"health check", "POST", "/pools/{id}/health-check", "/pools/1/health-check", `{}`, pools.HealthCheck, "failed to get pool"},
		{"export pool", "GET", "/pools/{id}/export", "/pools/1/export", "", pools.Export, "failed to get pool"},
		{"update alert rule", "PUT", "/pools/{id}/alert-rules/{rule_id}", "/pools/1/alert-rules/2",
			`{"min_active_proxies":1,"webhook_url":"https://example.com/hook","cooldown_minutes":1}`,
			pools.UpdateAlertRule, "failed to update alert rule"},
		{"get user", "GET", "/users/{id}", "/users/1", "", users.Get, "failed to get user"},
		{"update user", "PUT", "/users/{id}", "/users/1", `{}`, users.Update, "failed to update user"},
		{"update source", "PUT", "/sources/{id}", "/sources/1", `{}`, sources.Update, "failed to update source"},
		{"admin login", "POST", "/auth/login", "/auth/login", `{"username":"admin","password":"secret"}`, auth.Login, "Internal server error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := chi.NewRouter()
			router.MethodFunc(tt.method, tt.route, tt.handler)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body)))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("database failure returned %d: %s", w.Code, w.Body.String())
			}
			var response models.ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Error != tt.message {
				t.Fatalf("expected generic error %q, got %q", tt.message, response.Error)
			}
		})
	}
}
