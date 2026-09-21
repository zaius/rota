package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/authlimit"
	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProxyUserAPIAuth_DatabaseFailureIsInternalError(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://localhost/rota_test")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	repo := repository.NewUserRepository(&database.DB{Pool: pool})
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("request must not be authorized") })
	handler := JWTOrProxyUserMiddleware("test", repo, logger.New("error"), authlimit.New(1, time.Minute, time.Minute))(next)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/release", nil)
	req.SetBasicAuth("alice", "secret")
	for range 3 {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 500 {
			t.Fatalf("database outage returned %d", w.Code)
		}
	}
}
