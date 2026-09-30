package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/jackc/pgx/v5"
)

// SecretRepository stores process-level secrets (currently the JWT signing
// key) so they survive restarts instead of changing on every boot.
type SecretRepository struct {
	db *database.DB
}

// NewSecretRepository creates a new SecretRepository.
func NewSecretRepository(db *database.DB) *SecretRepository {
	return &SecretRepository{db: db}
}

const jwtSecretKey = "jwt_secret"

// EnsureJWTSecret returns the stored JWT signing key, generating and storing a
// 256-bit one on first use. Instances booting concurrently race safely: the
// losing INSERT does nothing and every instance reads back the winner's key.
func (r *SecretRepository) EnsureJWTSecret(ctx context.Context) (secret string, created bool, err error) {
	if secret, err = r.get(ctx, jwtSecretKey); err != nil || secret != "" {
		return secret, false, err
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", false, fmt.Errorf("generate jwt secret: %w", err)
	}
	candidate := hex.EncodeToString(buf)

	if _, err := r.db.Pool.Exec(ctx,
		`INSERT INTO system_secrets (key, value) VALUES ($1, $2) ON CONFLICT (key) DO NOTHING`,
		jwtSecretKey, candidate); err != nil {
		return "", false, fmt.Errorf("store jwt secret: %w", err)
	}

	if secret, err = r.get(ctx, jwtSecretKey); err != nil {
		return "", false, err
	}
	return secret, secret == candidate, nil
}

// get returns the secret stored under key, or "" when there is none.
func (r *SecretRepository) get(ctx context.Context, key string) (string, error) {
	var v string
	err := r.db.Pool.QueryRow(ctx, `SELECT value FROM system_secrets WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read secret %q: %w", key, err)
	}
	return v, nil
}
