package database

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed schema.sql
var schema string

// Migrate applies pending migrations. Concurrent instances serialize on a
// Postgres advisory lock.
func (db *DB) Migrate(ctx context.Context) error {
	sqlDB := stdlib.OpenDBFromPool(db.Pool)
	defer sqlDB.Close()

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, nil,
		goose.WithSessionLocker(locker),
		goose.WithGoMigrations(goose.NewGoMigration(1, &goose.GoFunc{RunTx: createSchema}, nil)),
		goose.WithSlog(db.logger.Logger),
		goose.WithVerbose(true),
	)
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
}

// createSchema creates the baseline schema, or adopts a database that the
// previous migration runner already brought to it.
func createSchema(ctx context.Context, tx *sql.Tx) error {
	var legacy bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&legacy); err != nil {
		return err
	}
	if !legacy {
		_, err := tx.ExecContext(ctx, schema)
		return err
	}

	// The previous runner's last migration was version 38.
	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version BETWEEN 1 AND 38`).Scan(&applied); err != nil {
		return err
	}
	if applied != 38 {
		return fmt.Errorf("database has %d of the 38 pre-goose migrations: run rota d369ccc once to apply the rest", applied)
	}
	_, err := tx.ExecContext(ctx, `DROP TABLE schema_migrations`)
	return err
}
