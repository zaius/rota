package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

type fakeWorkingProxies struct {
	proxyControlRepository
	proxies []models.Proxy

	gotCountry string
	gotTags    []string
	gotLimit   int
	gotAll     bool
}

func (f *fakeWorkingProxies) WorkingProxies(_ context.Context, country string, tags []string, limit int, all bool) ([]models.Proxy, error) {
	f.gotCountry, f.gotTags, f.gotLimit, f.gotAll = country, tags, limit, all
	return f.proxies, nil
}

func strPtr(s string) *string { return &s }

func TestExportWorkingProxies(t *testing.T) {
	proxies := []models.Proxy{
		{Address: "10.0.0.1:8080", Protocol: "http", Username: strPtr("bob"), Password: strPtr("p@ss:w")},
		{Address: "10.0.0.2:1080", Protocol: "socks5"},
	}
	tests := []struct {
		name        string
		query       string
		wantCode    int
		wantBody    string
		wantCountry string
		wantTags    []string
	}{
		{"URLs by default", "", 200, "http://bob:p%40ss%3Aw@10.0.0.1:8080\nsocks5://10.0.0.2:1080\n", "", nil},
		{"colon format", "?format=colon", 200, "10.0.0.1:8080:bob:p@ss:w\n10.0.0.2:1080\n", "", nil},
		{"filters", "?country=de&tag=res&tag=fast&limit=5&status=all", 200, "", "DE", []string{"res", "fast"}},
		{"bad format", "?format=json", 400, "", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeWorkingProxies{proxies: proxies}
			h := NewProxyControlHandler(nil, logger.New("error"))
			h.proxyRepo = src

			w := httptest.NewRecorder()
			h.ExportWorkingProxies(w, httptest.NewRequest(http.MethodGet, "/proxies/working"+tt.query, nil))

			if w.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tt.wantCode, w.Body)
			}
			if tt.wantCode != 200 {
				return
			}
			if tt.wantBody != "" && w.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", w.Body, tt.wantBody)
			}
			if src.gotCountry != tt.wantCountry || !reflect.DeepEqual(src.gotTags, tt.wantTags) {
				t.Errorf("filters = %q %v, want %q %v", src.gotCountry, src.gotTags, tt.wantCountry, tt.wantTags)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Error("credentials served without Cache-Control: no-store")
			}
		})
	}
}
