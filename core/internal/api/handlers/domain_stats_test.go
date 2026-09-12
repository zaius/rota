package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alpkeskin/rota/core/internal/events"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

type domainStatsStore struct {
	events.Store
	rng, domain string
	limit       int
	err         error
}

func (s *domainStatsStore) DomainStats(_ context.Context, rng, domain string, limit int) ([]models.DomainStats, error) {
	s.rng, s.domain, s.limit = rng, domain, limit
	return []models.DomainStats{}, s.err
}

func TestDomainStats_QueryValidation(t *testing.T) {
	for _, query := range []string{"range=bad", "limit=0", "limit=-1", "limit=1001", "limit=abc", "domain=%2F"} {
		h := NewDashboardHandler(nil, nil, logger.New("error"))
		w := httptest.NewRecorder()
		h.GetDomainStats(w, httptest.NewRequest("GET", "/dashboard/domains?"+query, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", query, w.Code)
		}
	}
}

func TestDomainStats_DefaultsAndFilter(t *testing.T) {
	for _, tt := range []struct {
		query, rng, domain string
		limit              int
	}{
		{"", "24h", "", 100},
		{"range=7d&domain=https%3A%2F%2FFOO.com%2Fpath&limit=5", "7d", "foo.com", 5},
	} {
		store := &domainStatsStore{}
		h := NewDashboardHandler(repository.NewDashboardRepository(nil, store), nil, logger.New("error"))
		w := httptest.NewRecorder()
		h.GetDomainStats(w, httptest.NewRequest("GET", "/dashboard/domains?"+tt.query, nil))
		if w.Code != http.StatusOK || store.rng != tt.rng || store.domain != tt.domain || store.limit != tt.limit {
			t.Fatalf("response=%d filter=%+v", w.Code, store)
		}
		if w.Body.String() != `{"range":"`+tt.rng+`","data":[]}`+"\n" {
			t.Fatalf("empty response: %s", w.Body.String())
		}
	}
}

func TestDomainStats_StoreError(t *testing.T) {
	store := &domainStatsStore{err: errors.New("unavailable")}
	h := NewDashboardHandler(repository.NewDashboardRepository(nil, store), nil, logger.New("error"))
	w := httptest.NewRecorder()
	h.GetDomainStats(w, httptest.NewRequest("GET", "/dashboard/domains", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got %d", w.Code)
	}
}
