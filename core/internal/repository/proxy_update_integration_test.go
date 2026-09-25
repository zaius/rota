package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/alpkeskin/rota/core/internal/models"
)

func TestIntegration_ProxyUpdate_PartialCredentialsAndTags(t *testing.T) {
	db := testDB(t)
	cleanTables(t, db)
	repo := NewProxyRepository(db)
	ctx := context.Background()

	user, pass := "alice", "s3cret"
	created, err := repo.Create(ctx, models.CreateProxyRequest{
		Address: "10.0.0.1:8080", Protocol: "http",
		Username: &user, Password: &pass, Tags: []string{"dc", "eu"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	update := func(body string) *models.Proxy {
		t.Helper()
		var req models.UpdateProxyRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if _, err := repo.Update(ctx, created.ID, req); err != nil {
			t.Fatalf("update %s: %v", body, err)
		}
		p, err := repo.GetByID(ctx, created.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return p
	}
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}

	// The dashboard's edit form sends no password and no tags.
	p := update(`{"address":"10.0.0.2:8080","protocol":"socks5","username":"alice"}`)
	if p.Address != "10.0.0.2:8080" || p.Protocol != "socks5" {
		t.Errorf("endpoint = %s://%s, want socks5://10.0.0.2:8080", p.Protocol, p.Address)
	}
	if str(p.Username) != "alice" || str(p.Password) != "s3cret" || !reflect.DeepEqual(p.Tags, []string{"dc", "eu"}) {
		t.Errorf("omitted fields changed: user=%s pass=%s tags=%v", str(p.Username), str(p.Password), p.Tags)
	}

	p = update(`{"password":"n3w","tags":["us"]}`)
	if str(p.Username) != "alice" || str(p.Password) != "n3w" || !reflect.DeepEqual(p.Tags, []string{"us"}) {
		t.Errorf("replace: user=%s pass=%s tags=%v", str(p.Username), str(p.Password), p.Tags)
	}

	p = update(`{"username":"","password":null,"tags":[]}`)
	if p.Username != nil || p.Password != nil || len(p.Tags) != 0 {
		t.Errorf("clear: user=%s pass=%s tags=%v", str(p.Username), str(p.Password), p.Tags)
	}
}

func TestIntegration_ProxyBulkUpdateTags(t *testing.T) {
	db := testDB(t)
	cleanTables(t, db)
	repo := NewProxyRepository(db)
	ctx := context.Background()

	var ids []int
	for i, tags := range [][]string{{"eu", "dc"}, {"us"}, {}} {
		p, err := repo.Create(ctx, models.CreateProxyRequest{
			Address: fmt.Sprintf("10.0.1.%d:8080", i+1), Protocol: "http", Tags: tags,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ids = append(ids, p.ID)
	}
	tagsOf := func(id int) []string {
		t.Helper()
		p, err := repo.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return p.Tags
	}

	// By ID: add merges and deduplicates, remove wins over add.
	n, err := repo.BulkUpdateTags(ctx, ids[:2], nil, []string{"eu", "res", "gone"}, []string{"dc", "gone"})
	if err != nil || n != 2 {
		t.Fatalf("by ids: n=%d err=%v", n, err)
	}
	if got := tagsOf(ids[0]); !reflect.DeepEqual(got, []string{"eu", "res"}) {
		t.Errorf("proxy 1 tags = %v, want [eu res]", got)
	}
	if got := tagsOf(ids[1]); !reflect.DeepEqual(got, []string{"eu", "res", "us"}) {
		t.Errorf("proxy 2 tags = %v, want [eu res us]", got)
	}
	if got := tagsOf(ids[2]); len(got) != 0 {
		t.Errorf("unselected proxy tags = %v, want none", got)
	}

	// By filter: every proxy matching the search.
	n, err = repo.BulkUpdateTags(ctx, nil, &models.ProxyFilter{Search: "10.0.1."}, nil, []string{"eu"})
	if err != nil || n != 3 {
		t.Fatalf("by filter: n=%d err=%v", n, err)
	}
	if got := tagsOf(ids[0]); !reflect.DeepEqual(got, []string{"res"}) {
		t.Errorf("proxy 1 tags after filter removal = %v, want [res]", got)
	}
}

func TestIntegration_PoolWorkingProxies(t *testing.T) {
	db := testDB(t)
	cleanTables(t, db)
	proxies := NewProxyRepository(db)
	pools := NewPoolRepository(db)
	ctx := context.Background()

	pool, err := pools.Create(ctx, models.CreatePoolRequest{Name: "export", RotationMethod: "roundrobin", Enabled: true})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	user, pass := "u", "p"
	var ids []int
	for i, state := range []struct {
		status   string
		latency  int
		cooldown bool
	}{
		{"active", 300, false}, {"active", 0, false}, {"active", 100, false},
		{"failed", 50, false}, {"active", 10, true},
	} {
		p, err := proxies.Create(ctx, models.CreateProxyRequest{
			Address: fmt.Sprintf("10.0.2.%d:8080", i+1), Protocol: "http", Username: &user, Password: &pass,
		})
		if err != nil {
			t.Fatalf("create proxy: %v", err)
		}
		if _, err := db.Pool.Exec(ctx,
			`UPDATE proxies SET status=$2, avg_response_time=$3, cooldown_until = CASE WHEN $4 THEN NOW() + INTERVAL '1 hour' END WHERE id=$1`,
			p.ID, state.status, state.latency, state.cooldown); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
	}
	if err := pools.AddProxies(ctx, pool.ID, ids); err != nil {
		t.Fatalf("add proxies: %v", err)
	}

	addresses := func(ps []models.Proxy) []string {
		out := make([]string, len(ps))
		for i, p := range ps {
			out[i] = p.Address
		}
		return out
	}

	working, err := pools.WorkingProxies(ctx, pool.ID, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	// Active and outside a cooldown, fastest first, unmeasured last.
	if got, want := addresses(working), []string{"10.0.2.3:8080", "10.0.2.1:8080", "10.0.2.2:8080"}; !reflect.DeepEqual(got, want) {
		t.Errorf("working = %v, want %v", got, want)
	}
	if working[0].Password == nil || *working[0].Password != "p" {
		t.Errorf("credentials missing: %+v", working[0])
	}

	all, err := pools.WorkingProxies(ctx, pool.ID, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := addresses(all), []string{"10.0.2.5:8080", "10.0.2.4:8080"}; !reflect.DeepEqual(got, want) {
		t.Errorf("all, limit 2 = %v, want %v", got, want)
	}
}
