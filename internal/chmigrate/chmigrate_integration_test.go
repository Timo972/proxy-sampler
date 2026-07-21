package chmigrate

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

func TestUpCreatesSampleEvents(t *testing.T) {
	dsn := os.Getenv("TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("set TEST_CLICKHOUSE_DSN")
	}
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := Up(ctx, opts, slog.Default()); err != nil {
		t.Fatal(err)
	}

	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var exists bool
	if err := conn.QueryRow(ctx, `SELECT count() > 0 FROM system.tables WHERE database = currentDatabase() AND name = 'sample_events'`).Scan(&exists); err != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
}
