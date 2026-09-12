package repository

import (
	"context"
	"testing"
	"time"
)

func TestIntegration_ScopeCooldownPersistence(t *testing.T) {
	db := testDB(t)
	cleanTables(t, db)
	ctx := context.Background()
	repo := NewProxyRepository(db)
	var id int
	if err := db.Pool.QueryRow(ctx, `INSERT INTO proxies (address, protocol) VALUES ('127.0.0.1:9988', 'http') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(7 * time.Minute).Truncate(time.Microsecond)
	for _, scope := range []string{"foo.com", "shopping"} {
		if p, err := repo.SetScopeCooldown(ctx, id, scope, until, "429"); err != nil || p == nil || p.ID != id {
			t.Fatalf("set: %v %v", p, err)
		}
	}
	// Recreating the repository must preserve both scopes, without a global cooldown.
	repo = NewProxyRepository(db)
	rows, err := repo.ListActiveScopeCooldowns(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list: %v %v", rows, err)
	}
	for _, row := range rows {
		if !row.CooldownUntil.Equal(until) || row.Reason != "429" {
			t.Fatalf("persisted cooldown: %+v", row)
		}
	}
	var global *time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT cooldown_until FROM proxies WHERE id=$1`, id).Scan(&global); err != nil || global != nil {
		t.Fatalf("global cooldown changed: %v %v", global, err)
	}
	if _, err := repo.SetScopeCooldown(ctx, id, "shopping", time.Now().Add(-time.Minute), "expired"); err != nil {
		t.Fatal(err)
	}
	rows, err = repo.ListActiveScopeCooldowns(ctx)
	if err != nil || len(rows) != 1 || rows[0].Scope != "foo.com" {
		t.Fatalf("expiry/upsert: %v %v", rows, err)
	}
	if n, err := repo.ClearScopeCooldowns(ctx, id, "foo.com"); err != nil || n != 1 {
		t.Fatalf("clear: %d %v", n, err)
	}
	if p, err := repo.SetScopeCooldown(ctx, id+100, "foo.com", until, ""); err != nil || p != nil {
		t.Fatalf("missing proxy: %v %v", p, err)
	}
}
