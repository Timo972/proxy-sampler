// Package config loads and validates runtime configuration from the environment.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Config contains the service's validated runtime configuration.
type Config struct {
	DatabaseURL        string
	ClickHouseDSN      string
	EncryptionKey      []byte
	HTTPAddr           string
	EnrichConcurrency  int
	ReputationTTL      time.Duration
	ProbeTargetDefault string
	DBAutoMigrate      bool
}

// Load reads and validates configuration using getenv.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		DatabaseURL:        getenv("DATABASE_URL"),
		ClickHouseDSN:      getenv("CLICKHOUSE_DSN"),
		HTTPAddr:           valueOr(getenv("HTTP_ADDR"), ":8080"),
		EnrichConcurrency:  2,
		ReputationTTL:      24 * time.Hour,
		ProbeTargetDefault: valueOr(getenv("PROBE_TARGET_DEFAULT"), "https://speed.cloudflare.com/cdn-cgi/trace"),
		DBAutoMigrate:      true,
	}
	if cfg.DatabaseURL == "" || cfg.ClickHouseDSN == "" {
		return Config{}, errors.New("DATABASE_URL and CLICKHOUSE_DSN are required")
	}

	key, err := base64.StdEncoding.DecodeString(getenv("ENCRYPTION_KEY"))
	if err != nil || len(key) != 32 {
		return Config{}, errors.New("ENCRYPTION_KEY must be base64-encoded 32 bytes")
	}
	cfg.EncryptionKey = key

	if raw := getenv("ENRICH_CONCURRENCY"); raw != "" {
		cfg.EnrichConcurrency, err = positiveInt(raw, "ENRICH_CONCURRENCY")
	}
	if err == nil {
		if raw := getenv("REPUTATION_TTL"); raw != "" {
			cfg.ReputationTTL, err = positiveDuration(raw, "REPUTATION_TTL")
		}
	}
	if err == nil {
		if raw := getenv("DB_AUTO_MIGRATE"); raw != "" {
			cfg.DBAutoMigrate, err = strconv.ParseBool(raw)
		}
	}
	if err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func positiveInt(raw, name string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func positiveDuration(raw, name string) (time.Duration, error) {
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}
