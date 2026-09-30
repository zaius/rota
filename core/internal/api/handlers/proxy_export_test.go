package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

type fakeWorkingProxies struct {
	pools   map[int]bool
	proxies []models.Proxy

	gotPool  int
	gotLimit int
	gotAll   bool
}

func (f *fakeWorkingProxies) GetByID(_ context.Context, id int) (*models.ProxyPool, error) {
	if !f.pools[id] {
		return nil, nil
	}
	return &models.ProxyPool{ID: id}, nil
}

func (f *fakeWorkingProxies) WorkingProxies(_ context.Context, poolID, limit int, all bool) ([]models.Proxy, error) {
	f.gotPool, f.gotLimit, f.gotAll = poolID, limit, all
	return f.proxies, nil
}

func strPtr(s string) *string { return &s }

func TestExportWorkingProxies(t *testing.T) {
	proxies := []models.Proxy{
		{Address: "10.0.0.1:8080", Protocol: "http", Username: strPtr("bob"), Password: strPtr("p@ss:w")},
		{Address: "10.0.0.2:1080", Protocol: "socks5"},
	}
	asExporter := func(r *http.Request, allowed bool) *http.Request {
		main := 7
		u := &models.ProxyUser{Username: "alice", MainPoolID: &main, FallbackPoolIDs: []int{9}, AllowProxyExport: allowed}
		return r.WithContext(context.WithValue(r.Context(), models.ProxyUserContextKey, u))
	}

	tests := []struct {
		name     string
		query    string
		user     func(*http.Request) *http.Request
		wantCode int
		wantBody string
		wantPool int
	}{
		{"user defaults to main pool as URLs", "", func(r *http.Request) *http.Request { return asExporter(r, true) },
			200, "http://bob:p%40ss%3Aw@10.0.0.1:8080\nsocks5://10.0.0.2:1080\n", 7},
		{"user reads a fallback pool in colon format", "?pool=9&format=colon", func(r *http.Request) *http.Request { return asExporter(r, true) },
			200, "10.0.0.1:8080:bob:p@ss:w\n10.0.0.2:1080\n", 9},
		{"user without export permission", "", func(r *http.Request) *http.Request { return asExporter(r, false) }, 403, "", 0},
		{"user reading another pool", "?pool=8", func(r *http.Request) *http.Request { return asExporter(r, true) }, 403, "", 0},
		{"admin must name a pool", "", func(r *http.Request) *http.Request { return r }, 400, "", 0},
		{"admin reads any existing pool", "?pool=8&limit=5&status=all", func(r *http.Request) *http.Request { return r }, 200, "", 8},
		{"admin unknown pool", "?pool=99", func(r *http.Request) *http.Request { return r }, 404, "", 0},
		{"bad format", "?pool=8&format=json", func(r *http.Request) *http.Request { return r }, 400, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeWorkingProxies{pools: map[int]bool{7: true, 8: true, 9: true}, proxies: proxies}
			h := NewProxyControlHandler(nil, nil, logger.New("error"))
			h.exportPools = src

			w := httptest.NewRecorder()
			h.ExportWorkingProxies(w, tt.user(httptest.NewRequest(http.MethodGet, "/proxies/working"+tt.query, nil)))

			if w.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tt.wantCode, w.Body)
			}
			if tt.wantCode != 200 {
				return
			}
			if tt.wantBody != "" && w.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", w.Body, tt.wantBody)
			}
			if src.gotPool != tt.wantPool {
				t.Errorf("listed pool %d, want %d", src.gotPool, tt.wantPool)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Error("credentials served without Cache-Control: no-store")
			}
		})
	}
}
