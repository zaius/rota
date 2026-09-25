package repository

import (
	"context"
	"testing"
)

func TestIntegration_EnsureJWTSecret(t *testing.T) {
	db := testDB(t)
	repo := NewSecretRepository(db)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `DELETE FROM system_secrets WHERE key = $1`, jwtSecretKey); err != nil {
		t.Fatalf("clear secret: %v", err)
	}

	first, created, err := repo.EnsureJWTSecret(ctx)
	if err != nil {
		t.Fatalf("first EnsureJWTSecret: %v", err)
	}
	if !created || len(first) != 64 {
		t.Fatalf("first call = (%d chars, created=%v), want a new 64-char key", len(first), created)
	}

	second, created, err := repo.EnsureJWTSecret(ctx)
	if err != nil {
		t.Fatalf("second EnsureJWTSecret: %v", err)
	}
	if created || second != first {
		t.Fatalf("second call = (created=%v, same=%v), want the stored key back", created, second == first)
	}
}
