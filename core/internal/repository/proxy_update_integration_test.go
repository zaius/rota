package repository

import (
	"context"
	"encoding/json"
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
