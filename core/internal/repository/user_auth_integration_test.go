package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/alpkeskin/rota/core/internal/models"
)

func TestIntegration_UserAuthenticationCache(t *testing.T) {
	db := testDB(t)
	cleanTables(t, db)
	repo := NewUserRepository(db)
	ctx := context.Background()
	pool := mustPool(t, db, "auth-cache-pool")
	created, err := repo.Create(ctx, models.CreateProxyUserRequest{
		Username: "auth-cache-user", Password: "old-secret", Enabled: true, MainPoolID: &pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Delete(ctx, created.ID); err != nil {
			t.Error(err)
		}
	})
	auth := func(password string, wantErr error) *models.ProxyUser {
		t.Helper()
		user, err := repo.Authenticate(ctx, created.Username, password)
		if !errors.Is(err, wantErr) || (wantErr != nil && user != nil) {
			t.Fatalf("authenticate: user=%v err=%v, want %v", user, err, wantErr)
		}
		return user
	}
	auth("old-secret", nil)
	queries := db.Pool.Stat().AcquireCount()
	for range 5 {
		auth("old-secret", nil)
		auth("wrong", ErrProxyAuthentication)
	}
	if got := db.Pool.Stat().AcquireCount(); got != queries {
		t.Fatalf("warm auth acquired database connections: before=%d after=%d", queries, got)
	}

	if _, err := repo.Update(ctx, created.ID, models.UpdateProxyUserRequest{Password: "new-secret"}); err != nil {
		t.Fatal(err)
	}
	auth("old-secret", ErrProxyAuthentication)
	auth("new-secret", nil)
	enabled := false
	if _, err := repo.Update(ctx, created.ID, models.UpdateProxyUserRequest{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	auth("new-secret", ErrProxyAuthentication)
	enabled = true
	if _, err := repo.Update(ctx, created.ID, models.UpdateProxyUserRequest{
		Enabled:         &enabled,
		MainPoolID:      models.Optional[int]{Present: true, Null: true},
		FallbackPoolIDs: models.Optional[[]int]{Present: true, Value: []int{pool}},
	}); err != nil {
		t.Fatal(err)
	}
	user := auth("new-secret", nil)
	if user.MainPoolID != nil || len(user.FallbackPoolIDs) != 1 || user.FallbackPoolIDs[0] != pool {
		t.Fatalf("cached stale pool permissions: %v", user)
	}

	if err := repo.Delete(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	auth("new-secret", ErrProxyAuthentication)
	created, err = repo.Create(ctx, models.CreateProxyUserRequest{
		Username: created.Username, Password: "replacement-secret", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	auth("new-secret", ErrProxyAuthentication)
	auth("replacement-secret", nil)
}
