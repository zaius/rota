package repository

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestIntegration_AdminAuthentication(t *testing.T) {
	db := testDB(t)
	repo := NewAdminRepository(db)
	ctx := context.Background()
	username := "status-test-admin"
	password := "test-password"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO admin_credentials (username, password_hash) VALUES ($1, $2)`, username, string(hash)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.Pool.Exec(ctx, `DELETE FROM admin_credentials WHERE username = $1`, username); err != nil {
			t.Errorf("clean admin credentials: %v", err)
		}
	})

	if err := repo.Authenticate(ctx, username, password); err != nil {
		t.Fatalf("valid credentials rejected: %v", err)
	}
	for _, credentials := range [][2]string{{username, "wrong-password"}, {username + "-missing", password}} {
		if err := repo.Authenticate(ctx, credentials[0], credentials[1]); !errors.Is(err, ErrAdminAuthentication) {
			t.Errorf("invalid credentials returned %v, want ErrAdminAuthentication", err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := repo.Authenticate(canceled, username, password); !errors.Is(err, context.Canceled) || errors.Is(err, ErrAdminAuthentication) {
		t.Fatalf("query failure must preserve its cause, got %v", err)
	}
}
