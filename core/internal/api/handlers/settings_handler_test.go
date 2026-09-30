package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

type fakeSettingsStore struct {
	current models.Settings
	saved   *models.Settings
}

func (f *fakeSettingsStore) GetAll(context.Context) (*models.Settings, error) {
	s := f.current
	return &s, nil
}

func (f *fakeSettingsStore) UpdateAll(_ context.Context, s *models.Settings) error {
	saved := *s
	f.saved = &saved
	f.current = saved
	return nil
}

func (f *fakeSettingsStore) Reset(context.Context) error { return nil }

func TestSettingsUpdate_KeepsOmittedSectionsAndFields(t *testing.T) {
	store := &fakeSettingsStore{current: models.Settings{
		Rotation: models.RotationSettings{FollowRedirect: true, Timeout: 90},
		HealthCheck: models.HealthCheckSettings{
			Timeout: 60, Workers: 20, URL: "https://api.ipify.org", Status: 200,
			Headers: []string{"User-Agent: rota"},
		},
		ProxyCleanup: models.ProxyCleanupSettings{Enabled: true, MaxFailedDays: 7, CleanupIntervalHours: 24},
	}}
	want := store.current
	want.HealthCheck.Timeout = 5

	h := NewSettingsHandler(store, logger.New("error"), nil)
	w := httptest.NewRecorder()
	h.Update(w, httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(`{"healthcheck":{"timeout":5}}`)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	if store.saved == nil {
		t.Fatal("settings were not saved")
	}
	if !reflect.DeepEqual(*store.saved, want) {
		t.Errorf("saved = %+v\nwant  %+v", *store.saved, want)
	}
}
