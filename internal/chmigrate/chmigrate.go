// Package chmigrate applies the embedded ClickHouse migrations at startup.
package chmigrate

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/pressly/goose/v3"

	"github.com/timo972/proxy-sampler/migrations"
)

// Up applies all pending embedded ClickHouse migrations.
func Up(ctx context.Context, opts *clickhouse.Options, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	fSys, err := fs.Sub(migrations.FS, "clickhouse")
	if err != nil {
		return fmt.Errorf("clickhouse migrations fs: %w", err)
	}
	if err := EnsureDatabase(ctx, opts); err != nil {
		return fmt.Errorf("ensure database: %w", err)
	}

	db := clickhouse.OpenDB(opts)
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping clickhouse: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectClickHouse, db, fSys)
	if err != nil {
		return fmt.Errorf("create goose provider: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply clickhouse migrations: %w", err)
	}
	if len(results) == 0 {
		logger.Info("clickhouse schema up to date")
		return nil
	}
	for _, result := range results {
		logger.Info("clickhouse migration applied", "version", result.Source.Version, "duration_ms", result.Duration.Milliseconds())
	}
	return nil
}

// EnsureDatabase creates the configured ClickHouse database when necessary.
func EnsureDatabase(ctx context.Context, opts *clickhouse.Options) error {
	name := opts.Auth.Database
	if name == "" || name == "default" {
		return nil
	}
	bootstrap := *opts
	bootstrap.Auth.Database = "default"
	conn, err := clickhouse.Open(&bootstrap)
	if err != nil {
		return err
	}
	defer conn.Close()

	statement := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", strings.ReplaceAll(name, "`", "``"))
	return conn.Exec(ctx, statement)
}
