package config

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x2a}, 32))
	env := map[string]string{
		"DATABASE_URL":   "postgres://localhost/proxy_sampler",
		"CLICKHOUSE_DSN": "clickhouse://localhost:9000/proxy_sampler",
		"ENCRYPTION_KEY": key,
	}
	cfg, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.EnrichConcurrency != 2 || cfg.ReputationTTL != 24*time.Hour {
		t.Fatalf("defaults = %#v", cfg)
	}
	if cfg.ProbeTargetDefault != "https://speed.cloudflare.com/cdn-cgi/trace" || !cfg.DBAutoMigrate {
		t.Fatalf("probe/migrate defaults = %#v", cfg)
	}
}

func TestLoadRejectsMissingDatabaseURLs(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x2a}, 32))
	_, err := Load(func(k string) string {
		return map[string]string{"ENCRYPTION_KEY": key}[k]
	})
	if !strings.Contains(fmt.Sprint(err), "DATABASE_URL and CLICKHOUSE_DSN are required") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRejectsBadEncryptionKey(t *testing.T) {
	_, err := Load(func(k string) string {
		return map[string]string{"DATABASE_URL": "postgres://db/x", "CLICKHOUSE_DSN": "clickhouse://ch/x", "ENCRYPTION_KEY": "c2hvcnQ="}[k]
	})
	if !strings.Contains(fmt.Sprint(err), "32 bytes") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadOverrides(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x2a}, 32))
	env := map[string]string{
		"DATABASE_URL":         "postgres://localhost/proxy_sampler",
		"CLICKHOUSE_DSN":       "clickhouse://localhost:9000/proxy_sampler",
		"ENCRYPTION_KEY":       key,
		"HTTP_ADDR":            "127.0.0.1:9090",
		"ENRICH_CONCURRENCY":   "4",
		"REPUTATION_TTL":       "30m",
		"PROBE_TARGET_DEFAULT": "https://example.com/trace",
		"DB_AUTO_MIGRATE":      "false",
	}

	cfg, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9090" || cfg.EnrichConcurrency != 4 || cfg.ReputationTTL != 30*time.Minute {
		t.Fatalf("overrides = %#v", cfg)
	}
	if cfg.ProbeTargetDefault != "https://example.com/trace" || cfg.DBAutoMigrate {
		t.Fatalf("probe/migrate overrides = %#v", cfg)
	}
}

func TestLoadRejectsInvalidOverrides(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x2a}, 32))
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr string
	}{
		{name: "non-positive concurrency", key: "ENRICH_CONCURRENCY", value: "0", wantErr: "ENRICH_CONCURRENCY must be a positive integer"},
		{name: "invalid concurrency", key: "ENRICH_CONCURRENCY", value: "many", wantErr: "ENRICH_CONCURRENCY must be a positive integer"},
		{name: "non-positive TTL", key: "REPUTATION_TTL", value: "0s", wantErr: "REPUTATION_TTL must be a positive duration"},
		{name: "invalid TTL", key: "REPUTATION_TTL", value: "daily", wantErr: "REPUTATION_TTL must be a positive duration"},
		{name: "invalid auto-migrate", key: "DB_AUTO_MIGRATE", value: "sometimes", wantErr: "invalid syntax"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{
				"DATABASE_URL":   "postgres://localhost/proxy_sampler",
				"CLICKHOUSE_DSN": "clickhouse://localhost:9000/proxy_sampler",
				"ENCRYPTION_KEY": key,
				tt.key:           tt.value,
			}
			_, err := Load(func(k string) string { return env[k] })
			if !strings.Contains(fmt.Sprint(err), tt.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadDefaultsMaxVariantsPerRun(t *testing.T) {
	cfg, err := Load(envFunc(map[string]string{
		"DATABASE_URL":   "postgres://x",
		"CLICKHOUSE_DSN": "clickhouse://x",
		"ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(make([]byte, 32)),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxVariantsPerRun != 128 {
		t.Fatalf("MaxVariantsPerRun = %d, want 128", cfg.MaxVariantsPerRun)
	}
}

func TestLoadOverridesMaxVariantsPerRun(t *testing.T) {
	cfg, err := Load(envFunc(map[string]string{
		"DATABASE_URL":         "postgres://x",
		"CLICKHOUSE_DSN":       "clickhouse://x",
		"ENCRYPTION_KEY":       base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"MAX_VARIANTS_PER_RUN": "20",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxVariantsPerRun != 20 {
		t.Fatalf("MaxVariantsPerRun = %d, want 20", cfg.MaxVariantsPerRun)
	}
}

func TestLoadRejectsNonPositiveMaxVariantsPerRun(t *testing.T) {
	_, err := Load(envFunc(map[string]string{
		"DATABASE_URL":         "postgres://x",
		"CLICKHOUSE_DSN":       "clickhouse://x",
		"ENCRYPTION_KEY":       base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"MAX_VARIANTS_PER_RUN": "0",
	}))
	if err == nil {
		t.Fatal("expected error for non-positive MAX_VARIANTS_PER_RUN")
	}
}

func envFunc(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}
