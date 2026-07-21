// Package migrate applies the embedded Postgres migrations at startup.
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the pgx database/sql driver
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/timo972/proxy-sampler/migrations"
)

// Up applies all pending embedded Postgres migrations.
func Up(ctx context.Context, databaseURL string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	fSys, err := fs.Sub(migrations.FS, "postgres")
	if err != nil {
		return fmt.Errorf("migrations fs: %w", err)
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open migration db: %w", err)
	}
	defer db.Close()

	if schema := searchPathSchema(databaseURL); schema != "" {
		if _, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
			return fmt.Errorf("ensure schema %q: %w", schema, err)
		}
		logger.Info("ensured schema", "schema", schema)
	}

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("create migration locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fSys, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("create goose provider: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	if len(results) == 0 {
		logger.Info("database schema up to date")
		return nil
	}
	for _, result := range results {
		logger.Info("migration applied", "version", result.Source.Version, "duration_ms", result.Duration.Milliseconds())
	}
	return nil
}

func searchPathSchema(databaseURL string) string {
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return ""
	}
	for _, part := range strings.Split(cfg.RuntimeParams["search_path"], ",") {
		schema := strings.Trim(strings.TrimSpace(part), `"`)
		if schema != "" && schema != "$user" {
			return schema
		}
	}
	return ""
}
