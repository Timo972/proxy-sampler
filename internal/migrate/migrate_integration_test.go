package migrate

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestUpCreatesSamplingSessions(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL")
	}
	ctx := context.Background()
	if err := Up(ctx, dsn, slog.Default()); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('sampling_sessions') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
}
