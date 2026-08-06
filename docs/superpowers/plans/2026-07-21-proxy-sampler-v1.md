# Proxy Sampler v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a durable, single-replica Go service that samples ad-hoc proxy endpoints, enriches observed egress IPs, persists control and time-series data, and serves an embedded React dashboard for session management and reporting.

**Architecture:** A chi HTTP server, a goroutine-per-session supervisor, and an async ClickHouse writer run in one process. Postgres is the source of truth for session lifecycle, rolling snapshots, IP inventory, and reputation cache; ClickHouse stores one append-only row per sample with parallel per-probe arrays. A generated OpenAPI boundary serves a React/Vite SPA embedded from the `web` Go package.

**Tech Stack:** Go 1.26, chi, pgx v5, sqlc, goose, clickhouse-go v2, oapi-codegen, OpenTelemetry, React 19, TypeScript, Vite, shadcn/ui, TanStack Query, React Router, Recharts, Vitest, and Testing Library.

## Global Constraints

- Module and binary are `github.com/timo972/proxy-sampler` and `proxy-sampler`; the only entrypoint is `cmd/app`.
- Runtime is one process and one replica; `claimed_by` and `lease_until` remain nullable schema reservations and have no v1 runtime behavior.
- Session lifecycle is exactly `running → stopped` for a manual stop or `running → finished` when `max_samples` or `max_duration_seconds` is reached.
- Proxy connection strings are encrypted with AES-GCM using a base64-encoded 32-byte `ENCRYPTION_KEY`; only redacted `host:port` is returned to clients.
- Postgres owns mutable session/config state, the rolling snapshot, session IP inventory, and the global reputation cache.
- ClickHouse owns append-only `sample_events`, one row per sample, with a 180-day TTL and no materialized views.
- A sample failure never aborts a session: failed probes contribute to success rate, and enrichment failures leave nullable fields or `unknown` classification.
- One process-wide `ENRICH_CONCURRENCY` semaphore bounds enrichment work across every session.
- GreyNoise and DNSBL lookups are IPv4-only; DNSBL `127.255.255.x` responses are errors, while Spamhaus PBL `127.0.0.10` and `127.0.0.11` are benign dynamic-residential evidence rather than fraud listings.
- The dashboard has no authentication, uses polling rather than SSE, and is intended for tailnet-only deployment.
- v1 does not add multi-replica leasing, alerting/notifications, commercial reputation providers, authentication, or server-sent events.
- Follow the repository naming preference: no hand-written Go or sqlc `GetX` names; use noun accessors such as `SessionByID` and verbs such as `Fetch`, `Save`, and `Delete`.
- If a `develop` branch exists when publishing, target `develop`; never target `main` or `staging`. The repository currently has only `main`.

---

## File Structure

The implementation should use the following ownership boundaries before feature work begins:

- `cmd/app/main.go`: composition root, process signals, migrations, dependency wiring, HTTP lifecycle, and graceful shutdown only.
- `internal/config/config.go`: environment parsing and validated runtime defaults.
- `internal/session/session.go`: shared session, snapshot, reputation, report, and pagination domain types.
- `internal/db/query/*.sql`: sqlc-owned Postgres statements; `internal/db/store.go` maps generated rows to domain types and provides transactional tick persistence.
- `internal/crypto/crypto.go`: AES-GCM encrypt/decrypt only.
- `internal/proxydial/*.go`: URL parsing, redaction, SOCKS5/SOCKS5h, and HTTP/HTTPS CONNECT dialers.
- `internal/enrich/types.go`, one file per external provider, `dnsbl.go`, `classify.go`, and `service.go`: provider parsing, classification, global concurrency, cache-aware enrichment orchestration.
- `internal/ch/event.go`, `writer.go`, and `reader.go`: ClickHouse event serialization, bounded async writes, report/sample reads, and synchronous deletion.
- `internal/sampler/probe.go`, `aggregate.go`, `worker.go`, and `supervisor.go`: one-request probes, deterministic aggregation, durable tick behavior, caps, resume, and stop semantics.
- `api/openapi.yaml` and generated `internal/api/openapi/openapi.gen.go`: public JSON contract and generated strict chi server boundary.
- `internal/api/server.go`, `report.go`, `csv.go`, `health.go`, and `spa.go`: transport mapping and response composition; business rules stay in lower packages.
- `web/embed.go`: embeds `dist/*` and exports `FS`; it lives under `web/` because Go embed patterns cannot contain `..`.
- `web/src/lib`, `components`, and `pages`: typed HTTP client, reusable product UI, and route-level composition.
- `migrations/embed.go`, `internal/migrate`, and `internal/chmigrate`: embedded goose migrations and boot-time runners.
- `Dockerfile`, `Makefile`, `.github/workflows/ci.yml`, `.env.example`, and `README.md`: reproducible build, validation, configuration, and operations.

Generated files (`internal/db/*.sql.go`, `internal/db/models.go`, `internal/db/querier.go`, and `internal/api/openapi/openapi.gen.go`) are outputs: regenerate them with `make generate`; do not hand-edit them.

---

### Task 1: Repository Scaffold, Configuration, and Embedded Migrations

**Files:**
- Create: `go.mod`
- Create: `go.sum` via `go mod tidy`
- Create: `.gitignore`
- Create: `Makefile`
- Create: `sqlc.yaml`
- Create: `oapi-codegen.yaml`
- Create: `migrations/embed.go`
- Create: `migrations/postgres/20260721090000_sampling_control_plane.sql`
- Create: `migrations/clickhouse/20260721090000_sample_events.sql`
- Create: `internal/migrate/migrate.go`
- Create: `internal/chmigrate/chmigrate.go`
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`
- Test: `internal/migrate/migrate_integration_test.go`
- Test: `internal/chmigrate/chmigrate_integration_test.go`

**Interfaces:**
- Consumes: environment variables listed in the design spec.
- Produces: `config.Load(getenv func(string) string) (config.Config, error)`, `migrate.Up(ctx, databaseURL, logger) error`, `chmigrate.Up(ctx, *clickhouse.Options, logger) error`, and embedded migration filesystems used by `cmd/app`.

- [ ] **Step 1: Write configuration tests for required values, defaults, and invalid input**

```go
func TestLoadDefaults(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x2a}, 32))
	env := map[string]string{
		"DATABASE_URL":   "postgres://localhost/proxy_sampler",
		"CLICKHOUSE_DSN": "clickhouse://localhost:9000/proxy_sampler",
		"ENCRYPTION_KEY": key,
	}
	cfg, err := Load(func(k string) string { return env[k] })
	if err != nil { t.Fatal(err) }
	if cfg.HTTPAddr != ":8080" || cfg.EnrichConcurrency != 2 || cfg.ReputationTTL != 24*time.Hour {
		t.Fatalf("defaults = %#v", cfg)
	}
	if cfg.ProbeTargetDefault != "https://speed.cloudflare.com/cdn-cgi/trace" || !cfg.DBAutoMigrate {
		t.Fatalf("probe/migrate defaults = %#v", cfg)
	}
}

func TestLoadRejectsBadEncryptionKey(t *testing.T) {
	_, err := Load(func(k string) string {
		return map[string]string{"DATABASE_URL": "postgres://db/x", "CLICKHOUSE_DSN": "clickhouse://ch/x", "ENCRYPTION_KEY": "c2hvcnQ="}[k]
	})
	if !strings.Contains(fmt.Sprint(err), "32 bytes") { t.Fatalf("err = %v", err) }
}
```

- [ ] **Step 2: Run the configuration tests and confirm the missing package failure**

Run: `go test ./internal/config -run TestLoad -v`

Expected: FAIL because `internal/config` does not exist.

- [ ] **Step 3: Create the module, pinned tools, generator configs, and build targets**

Use Go 1.26. `go.mod` must include `clickhouse-go/v2`, `chi/v5`, `google/uuid`, `pgx/v5`, `goose/v3`, `oapi-codegen/runtime`, `kin-openapi`, `x/net`, `x/sync`, and OpenTelemetry HTTP packages, plus tool directives for sqlc, goose, and oapi-codegen.

```make
BINARY := bin/proxy-sampler
SQLC := go tool sqlc
OAPI := go tool oapi-codegen
GOOSE := go tool goose

.PHONY: build test generate sqlc-generate openapi-generate web-install web-test web-build migrate-up ch-migrate-up

build: web-build generate
	go build -o $(BINARY) ./cmd/app

test:
	go test ./...
	cd web && npm test -- --run

sqlc-generate:
	$(SQLC) generate

openapi-generate:
	$(OAPI) -config oapi-codegen.yaml api/openapi.yaml

generate: sqlc-generate openapi-generate

web-install:
	cd web && npm ci

web-test:
	cd web && npm test -- --run

web-build:
	cd web && npm run build

migrate-up:
	$(GOOSE) -dir migrations/postgres postgres "$${DATABASE_URL}" up

ch-migrate-up:
	$(GOOSE) -dir migrations/clickhouse clickhouse "$${CLICKHOUSE_DSN}" up
```

Configure sqlc for `migrations/postgres` + `internal/db/query`, pgx/v5, JSON tags, interfaces, pointer nulls, and UUID override to `github.com/google/uuid.UUID`. Configure oapi-codegen for models, strict chi server, embedded spec, and prefixed enum values.

Use this ignore policy so the embedded SPA remains reproducible from an ordinary checkout:

```gitignore
.env
bin/
web/node_modules/
web/coverage/
```

Do not ignore `web/dist/`; Task 13 commits the production assets consumed by `go:embed`.

- [ ] **Step 4: Implement strict environment loading**

```go
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

func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		DatabaseURL: getenv("DATABASE_URL"), ClickHouseDSN: getenv("CLICKHOUSE_DSN"),
		HTTPAddr: valueOr(getenv("HTTP_ADDR"), ":8080"), EnrichConcurrency: 2,
		ReputationTTL: 24 * time.Hour,
		ProbeTargetDefault: valueOr(getenv("PROBE_TARGET_DEFAULT"), "https://speed.cloudflare.com/cdn-cgi/trace"),
		DBAutoMigrate: true,
	}
	if cfg.DatabaseURL == "" || cfg.ClickHouseDSN == "" { return Config{}, errors.New("DATABASE_URL and CLICKHOUSE_DSN are required") }
	key, err := base64.StdEncoding.DecodeString(getenv("ENCRYPTION_KEY"))
	if err != nil || len(key) != 32 { return Config{}, errors.New("ENCRYPTION_KEY must be base64-encoded 32 bytes") }
	cfg.EncryptionKey = key
	if raw := getenv("ENRICH_CONCURRENCY"); raw != "" { cfg.EnrichConcurrency, err = positiveInt(raw, "ENRICH_CONCURRENCY") }
	if err == nil && getenv("REPUTATION_TTL") != "" { cfg.ReputationTTL, err = positiveDuration(getenv("REPUTATION_TTL"), "REPUTATION_TTL") }
	if err == nil && getenv("DB_AUTO_MIGRATE") != "" { cfg.DBAutoMigrate, err = strconv.ParseBool(getenv("DB_AUTO_MIGRATE")) }
	if err != nil { return Config{}, err }
	return cfg, nil
}
```

- [ ] **Step 5: Add the exact Postgres and ClickHouse schemas**

The Postgres migration must create the three design tables and their checks in one transaction. Add checks for `mode IN ('sticky','pool')`, `status IN ('running','stopped','finished')`, positive cadence/timeouts/caps, and `probes_per_sample BETWEEN 1 AND 255`. Add indexes on `(status, created_at DESC)` and `session_ips(session_id, last_seen DESC)`.

```sql
-- +goose Up
CREATE TABLE sampling_sessions (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    proxy_ciphertext bytea NOT NULL,
    proxy_nonce bytea NOT NULL,
    proxy_display text NOT NULL,
    mode text NOT NULL CHECK (mode IN ('sticky', 'pool')),
    cadence_seconds integer NOT NULL CHECK (cadence_seconds > 0),
    probes_per_sample integer NOT NULL CHECK (probes_per_sample BETWEEN 1 AND 255),
    probe_target text NOT NULL,
    dial_timeout_ms integer NOT NULL CHECK (dial_timeout_ms > 0),
    max_samples integer CHECK (max_samples > 0),
    max_duration_seconds integer CHECK (max_duration_seconds > 0),
    status text NOT NULL CHECK (status IN ('running', 'stopped', 'finished')),
    samples_taken integer NOT NULL DEFAULT 0,
    probes_ok bigint NOT NULL DEFAULT 0,
    probes_total bigint NOT NULL DEFAULT 0,
    distinct_ips integer NOT NULL DEFAULT 0,
    last_sample_at timestamptz,
    last_primary_ip inet,
    last_rtt_ms integer,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    stopped_at timestamptz,
    claimed_by text,
    lease_until timestamptz
);
CREATE TABLE session_ips (
    session_id uuid NOT NULL REFERENCES sampling_sessions(id) ON DELETE CASCADE,
    ip inet NOT NULL, first_seen timestamptz NOT NULL DEFAULT now(),
    last_seen timestamptz NOT NULL DEFAULT now(), hit_count bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, ip)
);
CREATE TABLE ip_reputation_cache (
    ip inet PRIMARY KEY, country text, region text, city text, isp text, asn text,
    is_mobile boolean, ipapi_proxy boolean, ipapi_hosting boolean,
    pc_type text, pc_proxy boolean, risk_score integer CHECK (risk_score BETWEEN 0 AND 100),
    greynoise_class text, sfs_appears boolean, sfs_frequency integer,
    dnsbl_listed boolean, dnsbl_hits text, category text,
    raw jsonb NOT NULL DEFAULT '{}'::jsonb,
    first_seen timestamptz NOT NULL DEFAULT now(), refreshed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sampling_sessions_status_created_idx ON sampling_sessions (status, created_at DESC);
CREATE INDEX session_ips_session_last_seen_idx ON session_ips (session_id, last_seen DESC);

-- +goose Down
DROP TABLE IF EXISTS session_ips;
DROP TABLE IF EXISTS ip_reputation_cache;
DROP TABLE IF EXISTS sampling_sessions;
```

The ClickHouse migration is:

```sql
-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS sample_events (
    session_id UUID,
    sampled_at DateTime64(3, 'UTC'),
    sample_seq UInt32,
    probes_attempted UInt8,
    probes_ok UInt8,
    primary_ip IPv6,
    distinct_ips UInt8,
    ip_changed UInt8,
    new_ips UInt8,
    rtt_min_ms UInt32,
    rtt_med_ms UInt32,
    rtt_max_ms UInt32,
    egress_country LowCardinality(FixedString(2)),
    primary_category LowCardinality(String),
    primary_risk UInt8,
    probe_ips Array(IPv6),
    probe_rtts_ms Array(UInt32),
    probe_ok Array(UInt8),
    error String
)
ENGINE = MergeTree
PARTITION BY toDate(sampled_at)
ORDER BY (session_id, sampled_at)
TTL toDateTime(sampled_at) + INTERVAL 180 DAY;
-- +goose StatementEnd
-- +goose Down
DROP TABLE IF EXISTS sample_events;
```

- [ ] **Step 6: Implement embedded goose runners**

`migrations/embed.go` exposes `//go:embed postgres/*.sql clickhouse/*.sql`. `migrate.Up` uses a pgx `database/sql` connection and `goose.WithSessionLocker(lock.NewPostgresSessionLocker())`. `chmigrate.Up` calls `EnsureDatabase`, opens `clickhouse.OpenDB(opts)`, then runs a ClickHouse goose provider over `fs.Sub(migrations.FS, "clickhouse")`. Preserve contextual errors and log each applied version.

- [ ] **Step 7: Add migration integration tests**

```go
func TestUpCreatesSamplingSessions(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" { t.Skip("set TEST_DATABASE_URL") }
	if err := Up(context.Background(), dsn, slog.Default()); err != nil { t.Fatal(err) }
	pool, err := pgxpool.New(context.Background(), dsn); if err != nil { t.Fatal(err) }
	defer pool.Close()
	var exists bool
	if err := pool.QueryRow(context.Background(), `SELECT to_regclass('sampling_sessions') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
}
```

Mirror this for ClickHouse by querying `system.tables` when `TEST_CLICKHOUSE_DSN` is set.

- [ ] **Step 8: Run scaffold validation**

Run: `go mod tidy && go test ./internal/config ./internal/migrate ./internal/chmigrate`

Expected: unit tests PASS; integration tests SKIP without their DSNs and PASS when DSNs are supplied.

- [ ] **Step 9: Commit the scaffold**

```bash
git add .gitignore go.mod go.sum Makefile sqlc.yaml oapi-codegen.yaml migrations internal/config internal/migrate internal/chmigrate
git commit -m "chore: scaffold proxy sampler service"
```

### Task 2: Session Domain and Postgres Store

**Files:**
- Create: `internal/session/session.go`
- Create: `internal/session/store.go`
- Create: `internal/db/query/sessions.sql`
- Create: `internal/db/query/reputation.sql`
- Create: `internal/db/store.go`
- Test: `internal/db/store_integration_test.go`
- Create: generated `internal/db/db.go`, `internal/db/models.go`, `internal/db/querier.go`, `internal/db/sessions.sql.go`, and `internal/db/reputation.sql.go` via `make sqlc-generate`

**Interfaces:**
- Consumes: Postgres schema from Task 1.
- Produces: `session.Store`, domain-safe `session.Session`, `session.Snapshot`, `session.Reputation`, and `session.IPRecord` used by sampler and API tasks.

- [ ] **Step 1: Define domain types and the store contract**

```go
type Mode string
const (ModeSticky Mode = "sticky"; ModePool Mode = "pool")
type Status string
const (StatusRunning Status = "running"; StatusStopped Status = "stopped"; StatusFinished Status = "finished")

type Session struct {
	ID uuid.UUID; Name string; ProxyCiphertext, ProxyNonce []byte; ProxyDisplay string
	Mode Mode; Cadence time.Duration; ProbesPerSample int; ProbeTarget string; DialTimeout time.Duration
	MaxSamples *int; MaxDuration *time.Duration; Status Status
	Snapshot Snapshot; CreatedAt time.Time; StartedAt *time.Time; StoppedAt *time.Time
}
type Snapshot struct {
	SamplesTaken int; ProbesOK, ProbesTotal int64; DistinctIPs int
	LastSampleAt *time.Time; LastPrimaryIP netip.Addr; LastCategory string; LastRTT *time.Duration; LastError string
}
type IPRecord struct { IP netip.Addr; FirstSeen, LastSeen time.Time; HitCount int64; Reputation *Reputation }
type Reputation struct {
	IP netip.Addr; Country, Region, City, ISP, ASN string
	IsMobile, IPAPIProxy, IPAPIHosting *bool; ProxyCheckType string; ProxyCheckProxy *bool; RiskScore *int
	GreyNoiseClass string; SFSAppears *bool; SFSFrequency *int; DNSBLListed *bool; DNSBLHits []string
	Category string; Raw json.RawMessage; FirstSeen, RefreshedAt time.Time
}

type Store interface {
	Create(ctx context.Context, s Session) (Session, error)
	Sessions(ctx context.Context) ([]Session, error)
	SessionByID(ctx context.Context, id uuid.UUID) (Session, error)
	RunningSessions(ctx context.Context) ([]Session, error)
	Stop(ctx context.Context, id uuid.UUID, at time.Time) error
	Finish(ctx context.Context, id uuid.UUID, at time.Time) error
	Delete(ctx context.Context, id uuid.UUID) error
	SaveTick(ctx context.Context, id uuid.UUID, snapshot Snapshot, hits []IPHit) error
	SessionIPs(ctx context.Context, id uuid.UUID) ([]IPRecord, error)
	ReputationByIP(ctx context.Context, ip netip.Addr) (Reputation, bool, error)
	SaveReputation(ctx context.Context, reputation Reputation) error
}
```

`IPHit` contains `IP`, `SeenAt`, and the number of successful probes for that IP in the sample. Keep SQL-specific nullable types out of this package.

- [ ] **Step 2: Write store integration tests before SQL**

Cover: insert/read round-trip without exposing plaintext proxy data; `RunningSessions` excludes stopped rows; `Stop` only transitions running sessions; `SaveTick` atomically updates the snapshot and IP hit counts; reputation upsert preserves `first_seen` while advancing `refreshed_at`; and deleting a session cascades `session_ips` but not the global cache.

```go
func TestSaveTickUpdatesSnapshotAndIPInventory(t *testing.T) {
	store := testStore(t)
	s := insertTestSession(t, store)
	ip := netip.MustParseAddr("203.0.113.7")
	now := time.Now().UTC().Truncate(time.Millisecond)
	err := store.SaveTick(context.Background(), s.ID, session.Snapshot{
		SamplesTaken: 1, ProbesOK: 2, ProbesTotal: 3, DistinctIPs: 1,
		LastSampleAt: &now, LastPrimaryIP: ip, LastRTT: durationPtr(125*time.Millisecond),
	}, []session.IPHit{{IP: ip, SeenAt: now, Hits: 2}})
	if err != nil { t.Fatal(err) }
	got, err := store.SessionByID(context.Background(), s.ID); if err != nil { t.Fatal(err) }
	if got.Snapshot.SamplesTaken != 1 || got.Snapshot.ProbesOK != 2 { t.Fatalf("snapshot=%#v", got.Snapshot) }
	ips, err := store.SessionIPs(context.Background(), s.ID); if err != nil { t.Fatal(err) }
	if len(ips) != 1 || ips[0].HitCount != 2 { t.Fatalf("ips=%#v", ips) }
}
```

- [ ] **Step 3: Add noun-style sqlc queries**

Use names `InsertSession`, `Sessions`, `SessionByID`, `RunningSessions`, `StopSession`, `FinishSession`, `DeleteSession`, `UpdateSessionSnapshot`, `UpsertSessionIP`, `SessionIPs`, `ReputationByIP`, and `UpsertReputation`. Never use a `Get` prefix. Cast `inet` values to text on reads and cast text parameters back to `inet` on writes so the domain mapping consistently uses `net/netip.Addr`. `Sessions` and `SessionByID` left-join `ip_reputation_cache` on `last_primary_ip` to populate the read-only `Snapshot.LastCategory` used by the home table; `UpdateSessionSnapshot` does not write that derived field.

```sql
-- name: StopSession :execrows
UPDATE sampling_sessions
SET status = 'stopped', stopped_at = sqlc.arg(stopped_at)
WHERE id = sqlc.arg(id) AND status = 'running';

-- name: UpdateSessionSnapshot :exec
UPDATE sampling_sessions SET
  samples_taken = sqlc.arg(samples_taken), probes_ok = sqlc.arg(probes_ok),
  probes_total = sqlc.arg(probes_total), distinct_ips = sqlc.arg(distinct_ips),
  last_sample_at = sqlc.arg(last_sample_at),
  last_primary_ip = NULLIF(sqlc.arg(last_primary_ip), '')::inet,
  last_rtt_ms = sqlc.arg(last_rtt_ms), last_error = NULLIF(sqlc.arg(last_error), '')
WHERE id = sqlc.arg(id);

-- name: UpsertSessionIP :exec
INSERT INTO session_ips (session_id, ip, first_seen, last_seen, hit_count)
VALUES (sqlc.arg(session_id), sqlc.arg(ip)::inet, sqlc.arg(seen_at), sqlc.arg(seen_at), sqlc.arg(hits))
ON CONFLICT (session_id, ip) DO UPDATE
SET last_seen = EXCLUDED.last_seen, hit_count = session_ips.hit_count + EXCLUDED.hit_count;

-- name: ReputationByIP :one
SELECT ip::text AS ip, country, region, city, isp, asn, is_mobile, ipapi_proxy,
  ipapi_hosting, pc_type, pc_proxy, risk_score, greynoise_class, sfs_appears,
  sfs_frequency, dnsbl_listed, dnsbl_hits, category, raw, first_seen, refreshed_at
FROM ip_reputation_cache WHERE ip = sqlc.arg(ip)::inet;
```

- [ ] **Step 4: Generate and inspect the sqlc boundary**

Run: `make sqlc-generate && go test ./internal/db -run TestSaveTick -v`

Expected: FAIL because `internal/db/store.go` has not mapped the generated queries yet. Inspect generated argument and nullable types before writing the adapter; update only hand-written SQL/config if generation exposes a type mismatch.

- [ ] **Step 5: Implement the Postgres adapter and transaction boundary**

`db.Store` owns `*pgxpool.Pool` and `*Queries`. `SaveTick` begins one pgx transaction, calls `UpdateSessionSnapshot`, loops through the sample's unique `IPHit` values, calls `UpsertSessionIP`, and commits. `Stop` and `Finish` inspect `ExecRows` and return `session.ErrNotRunning` on zero affected rows. Convert `pgx.ErrNoRows` to `session.ErrNotFound` at the adapter boundary.

```go
func (s *Store) SaveTick(ctx context.Context, id uuid.UUID, snap session.Snapshot, hits []session.IPHit) error {
	tx, err := s.pool.Begin(ctx); if err != nil { return fmt.Errorf("begin tick: %w", err) }
	defer tx.Rollback(ctx)
	q := s.q.WithTx(tx)
	if err := q.UpdateSessionSnapshot(ctx, snapshotParams(id, snap)); err != nil { return fmt.Errorf("save snapshot: %w", err) }
	for _, hit := range hits {
		if err := q.UpsertSessionIP(ctx, hitParams(id, hit)); err != nil { return fmt.Errorf("save session ip %s: %w", hit.IP, err) }
	}
	if err := tx.Commit(ctx); err != nil { return fmt.Errorf("commit tick: %w", err) }
	return nil
}
```

- [ ] **Step 6: Run Postgres tests and the full Go suite**

Run: `TEST_DATABASE_URL="$TEST_DATABASE_URL" go test ./internal/db -v && go test ./...`

Expected: store tests PASS with Postgres; all unit tests PASS and DSN-gated tests SKIP when unset.

- [ ] **Step 7: Commit session persistence**

```bash
git add internal/session internal/db sqlc.yaml migrations/postgres
git commit -m "feat: add durable session store"
```

### Task 3: Proxy Credential Encryption and Dialers

**Files:**
- Create: `internal/crypto/crypto.go`
- Test: `internal/crypto/crypto_test.go`
- Create: `internal/proxydial/dialer.go`
- Create: `internal/proxydial/connect.go`
- Test: `internal/proxydial/dialer_test.go`
- Test: `internal/proxydial/connect_test.go`

**Interfaces:**
- Consumes: a validated 32-byte encryption key and proxy URLs.
- Produces: `crypto.New(key) (*Cipher, error)`, `(*Cipher).Encrypt(plaintext) (ciphertext, nonce []byte, err error)`, `(*Cipher).Decrypt(ciphertext, nonce) (string, error)`, `proxydial.FromURL(raw, timeout) (proxy.ContextDialer, error)`, and `proxydial.Display(raw) (string, error)`.

- [ ] **Step 1: Write AES-GCM round-trip and tamper tests**

```go
func TestCipherRoundTrip(t *testing.T) {
	c, err := New(bytes.Repeat([]byte{7}, 32)); if err != nil { t.Fatal(err) }
	sealed, nonce, err := c.Encrypt("socks5h://user:secret@proxy.example:1080"); if err != nil { t.Fatal(err) }
	if bytes.Contains(sealed, []byte("secret")) { t.Fatal("ciphertext contains plaintext") }
	plain, err := c.Decrypt(sealed, nonce); if err != nil || plain != "socks5h://user:secret@proxy.example:1080" { t.Fatalf("plain=%q err=%v", plain, err) }
	sealed[0] ^= 1
	if _, err := c.Decrypt(sealed, nonce); err == nil { t.Fatal("tampered ciphertext decrypted") }
}
```

- [ ] **Step 2: Implement AES-GCM with fresh random nonces**

Use `aes.NewCipher`, `cipher.NewGCM`, and `io.ReadFull(rand.Reader, nonce)`. Reject non-32-byte keys, incorrect nonce lengths, and empty ciphertext with contextual errors. Do not log plaintext, keys, ciphertext, or nonce.

- [ ] **Step 3: Write URL, redaction, unsupported-scheme, and CONNECT tests**

Test `socks5`, `socks5h`, `http`, and `https`; missing port; percent-encoded credentials; IPv6 proxy hosts; and redaction. The expected display values are `proxy.example:1080` and `[2001:db8::1]:8080`, with no username or password.

For CONNECT, start an `httptest`-style TCP server that records the request line and `Proxy-Authorization`, returns `HTTP/1.1 200 Connection Established`, and verifies that the dialer returns the still-open tunnel connection. Also test a `407` response.

- [ ] **Step 4: Implement scheme-specific dialers**

```go
type ContextDialer interface { DialContext(context.Context, string, string) (net.Conn, error) }

func FromURL(raw string, timeout time.Duration) (proxy.ContextDialer, error) {
	u, err := url.Parse(raw); if err != nil { return nil, fmt.Errorf("parse proxy URL: %w", err) }
	if u.Hostname() == "" || u.Port() == "" { return nil, errors.New("proxy URL requires host and port") }
	base := &net.Dialer{Timeout: timeout}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil { password, _ := u.User.Password(); auth = &proxy.Auth{User: u.User.Username(), Password: password} }
		d, err := proxy.SOCKS5("tcp", u.Host, auth, base); if err != nil { return nil, err }
		cd, ok := d.(proxy.ContextDialer); if !ok { return nil, errors.New("SOCKS5 dialer lacks context support") }
		return cd, nil
	case "http", "https":
		return newConnectDialer(u, base), nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
}
```

The CONNECT dialer must use TLS with `ServerName: u.Hostname()` for `https`, write a request with `Host` and optional Basic proxy authorization, parse it with `http.ReadResponse`, reject non-2xx status, and close on every error path.

- [ ] **Step 5: Run crypto and dialer tests**

Run: `go test ./internal/crypto ./internal/proxydial -v`

Expected: PASS, including race-safe CONNECT tests.

- [ ] **Step 6: Commit secure proxy handling**

```bash
git add internal/crypto internal/proxydial
git commit -m "feat: encrypt credentials and build proxy dialers"
```

### Task 4: Enrichment Model and Classification

**Files:**
- Create: `internal/enrich/types.go`
- Create: `internal/enrich/classify.go`
- Test: `internal/enrich/classify_test.go`

**Interfaces:**
- Consumes: partial results from all provider clients.
- Produces: `enrich.Partial`, `enrich.Result`, `enrich.Provider`, and `enrich.Classify(partial Partial) string` with the exact categories `mobile`, `residential`, `datacenter`, and `unknown`.

- [ ] **Step 1: Write table-driven classification tests**

```go
func TestClassify(t *testing.T) {
	tests := []struct { name string; p Partial; want string }{
		{"asn hosting wins", Partial{ASN: "AS62633 HostRush", HadSignal: true}, "datacenter"},
		{"database mart", Partial{ASN: "AS401479 Database Mart, LLC", HadSignal: true}, "datacenter"},
		{"proxycheck dch", Partial{ProxyCheckType: "DCH", HadSignal: true}, "datacenter"},
		{"mobile flag", Partial{IsMobile: ptr(true), HadSignal: true}, "mobile"},
		{"wireless", Partial{ProxyCheckType: "Wireless", HadSignal: true}, "mobile"},
		{"ordinary isp", Partial{ASN: "AS7922 Comcast Cable", HadSignal: true}, "residential"},
		{"pbl dynamic", Partial{DNSBLDynamic: true, HadSignal: true}, "residential"},
		{"no successful lookup", Partial{}, "unknown"},
	}
	for _, tt := range tests { t.Run(tt.name, func(t *testing.T) { if got := Classify(tt.p); got != tt.want { t.Fatalf("got %q want %q", got, tt.want) } }) }
}
```

- [ ] **Step 2: Define mergeable provider data**

`Partial` contains pointer booleans/integers so false and zero remain distinguishable from missing data, a `Raw map[string]json.RawMessage` keyed by provider name, `HadSignal`, and `Errors map[string]string`. `Merge` fills only fields represented by the incoming provider, unions DNSBL hits without duplicates, and merges raw payloads by provider key.

```go
type Provider interface {
	Name() string
	Lookup(ctx context.Context, ip netip.Addr) (Partial, error)
}

type Result struct { Partial Partial; Category string; RefreshedAt time.Time }
```

- [ ] **Step 3: Implement ordered classification rules**

Apply datacenter evidence first, then mobile, then residential. Normalize ProxyCheck types case-insensitively. The datacenter type set is `DCH`, `Business`, `Hosting`, `Corporate`, and `Education`. Match ASN organization text against explicit case-insensitive tokens including `amazon`, `aws`, `google cloud`, `microsoft`, `azure`, `digitalocean`, `linode`, `vultr`, `ovh`, `hetzner`, `hostrush`, `database mart`, `hosting`, `cloud`, `colo`, `datacenter`, `data center`, and `server`.

- [ ] **Step 4: Run classification tests**

Run: `go test ./internal/enrich -run 'TestClassify|TestMerge' -v`

Expected: PASS.

- [ ] **Step 5: Commit enrichment types**

```bash
git add internal/enrich
git commit -m "feat: define reputation classification"
```

### Task 5: Keyless Enrichment Providers, DNSBL, and Cache-Aware Service

**Files:**
- Create: `internal/enrich/ipapi.go`
- Create: `internal/enrich/proxycheck.go`
- Create: `internal/enrich/greynoise.go`
- Create: `internal/enrich/stopforumspam.go`
- Create: `internal/enrich/dnsbl.go`
- Create: `internal/enrich/service.go`
- Test: `internal/enrich/providers_test.go`
- Test: `internal/enrich/dnsbl_test.go`
- Test: `internal/enrich/service_test.go`

**Interfaces:**
- Consumes: `enrich.Provider`, `session.Store`, `REPUTATION_TTL`, and the process-wide concurrency limit.
- Produces: `enrich.Service.Lookup(ctx, ip) (session.Reputation, error)`; fresh cache hits do no outbound work, stale/missing hits are refreshed, and partial provider failure returns the merged record plus a non-fatal joined error.

- [ ] **Step 1: Write provider parser tests with local HTTP servers**

Use exact representative payloads:

```json
{"status":"success","country":"United States","regionName":"Virginia","city":"Ashburn","isp":"HostRush","as":"AS62633 HostRush","mobile":false,"proxy":false,"hosting":false}
```

```json
{"status":"ok","203.0.113.7":{"type":"Wireless","proxy":"yes","risk":"73"}}
```

```json
{"ip":"203.0.113.7","noise":true,"riot":false,"classification":"malicious"}
```

```json
{"success":1,"ip":{"appears":1,"frequency":42,"lastseen":"2026-07-20 10:00:00"}}
```

Assert endpoint paths/query parameters, GreyNoise's `Accept: application/json`, pointer values, raw payload preservation, non-2xx errors bounded to a short response excerpt, and IPv6 GreyNoise skip without an HTTP call.

- [ ] **Step 2: Implement provider clients with one shared hardened HTTP client**

Use `http.Client{Timeout: 10 * time.Second}` and these production endpoints, with base URLs injectable in tests:

```go
const (
	ipAPIBase        = "http://ip-api.com/json/"
	proxyCheckBase   = "https://proxycheck.io/v2/"
	greyNoiseBase    = "https://api.greynoise.io/v3/community/"
	stopForumSpamURL = "https://api.stopforumspam.org/api"
)
```

ip-api requests append `?fields=status,country,regionName,city,isp,as,mobile,proxy,hosting`; ProxyCheck appends `?vpn=1&risk=1`; StopForumSpam sends `ip` and `json` query values. Require successful provider-level status fields, validate risk `0..100`, and never include full response bodies or proxy credentials in errors. Each provider's `Name()` is stable: `ip-api`, `proxycheck`, `greynoise`, and `stopforumspam`.

- [ ] **Step 3: Write DNSBL resolver tests**

Define a narrow resolver seam:

```go
type HostResolver interface { LookupHost(context.Context, string) ([]string, error) }
```

Test reversed query `7.113.0.203.zen.spamhaus.org`, NXDOMAIN as not listed, `127.0.0.2` as listed, PBL-only `127.0.0.10`/`.11` as `DNSBLDynamic=true` and not listed, `127.255.255.1` as a provider error, mixed zones, and IPv6 skip. Use the five zones from the design in their listed order.

- [ ] **Step 4: Implement DNSBL lookups without shell commands**

Query zones sequentially through `net.DefaultResolver` so one IP-enrichment slot never fans out past the global bound. Ignore `*net.DNSError` with `IsNotFound`; collect other zone errors; sort hit names for deterministic persistence; and return a partial result even if another zone failed.

- [ ] **Step 5: Write cache and concurrency service tests**

Use a fake store and blocking providers to prove: a record refreshed 23 hours ago bypasses providers at a 24-hour TTL; a 25-hour record refreshes; two simultaneous misses never exceed configured concurrency; provider errors still save merged successful fields; classification is saved; and a failed cache read aborts rather than spending quota.

- [ ] **Step 6: Implement the cache-aware enrichment service**

```go
func (s *Service) Lookup(ctx context.Context, ip netip.Addr) (session.Reputation, error) {
	if cached, ok, err := s.store.ReputationByIP(ctx, ip); err != nil { return session.Reputation{}, err
	} else if ok && s.now().Sub(cached.RefreshedAt) < s.ttl { return cached, nil }
	if err := s.sem.Acquire(ctx, 1); err != nil { return session.Reputation{}, err }
	defer s.sem.Release(1)
	var merged Partial
	var errs []error
	for _, provider := range s.providers {
		partial, err := provider.Lookup(ctx, ip)
		merged.Merge(partial)
		if err != nil { merged.Errors[provider.Name()] = err.Error(); errs = append(errs, fmt.Errorf("%s: %w", provider.Name(), err)) }
	}
	reputation := toReputation(ip, merged, Classify(merged), s.now())
	if err := s.store.SaveReputation(ctx, reputation); err != nil { return session.Reputation{}, errors.Join(append(errs, err)...)}
	return reputation, errors.Join(errs...)
}
```

Construct provider order as ip-api, proxycheck, GreyNoise, StopForumSpam, DNSBL. All provider errors are logged by callers at warning level; none makes a sample fail.

- [ ] **Step 7: Run provider and race tests**

Run: `go test -race ./internal/enrich -v`

Expected: PASS with observed fake concurrency never exceeding the configured limit.

- [ ] **Step 8: Commit the enrichment stack**

```bash
git add internal/enrich
git commit -m "feat: add keyless IP enrichment stack"
```

### Task 6: ClickHouse Event Writer and Report Reader

**Files:**
- Create: `internal/ch/event.go`
- Create: `internal/ch/writer.go`
- Create: `internal/ch/reader.go`
- Test: `internal/ch/writer_test.go`
- Test: `internal/ch/reader_integration_test.go`

**Interfaces:**
- Consumes: `sample_events` from Task 1 and aggregated samples from Task 7.
- Produces: `ch.Writer.Enqueue(Event) bool`, `ch.Writer.Flush(ctx) error`, `ch.Writer.Close(ctx) error`, `ch.Reader.Samples(ctx, sessionID, from, to, page)`, `ch.Reader.StreamSamples(ctx, sessionID, from, to, visit)`, `ch.Reader.Series`, `ch.Reader.Stickiness`, `ch.Reader.PoolGrowth`, `ch.Reader.DeleteSession`, and `ch.Reader.Ping`.

- [ ] **Step 1: Define the event and report read models**

```go
type Event struct {
	SessionID uuid.UUID; SampledAt time.Time; SampleSeq uint32
	ProbesAttempted, ProbesOK uint8; PrimaryIP net.IP; DistinctIPs, IPChanged, NewIPs uint8
	RTTMinMS, RTTMedMS, RTTMaxMS uint32; EgressCountry, PrimaryCategory string; PrimaryRisk uint8
	ProbeIPs []net.IP; ProbeRTTsMS []uint32; ProbeOK []uint8; Error string
}
type SamplePage struct { Items []Event; Page, PageSize int; Total uint64 }
type SeriesPoint struct {
	At time.Time; SuccessRate, LatencyP50MS, LatencyP95MS float64
	DistinctPerSample float64; IPChanges uint64; Mobile, Residential, Datacenter, Unknown uint64
}
type Hold struct { IP string; StartedAt, EndedAt time.Time; Samples uint32; DurationSeconds int64 }
type Rotation struct { At time.Time; FromIP, ToIP string; SincePreviousSeconds int64 }
type Stickiness struct { Holds []Hold; Rotations []Rotation; AverageHoldSeconds, MedianHoldSeconds float64 }
type GrowthPoint struct { At time.Time; DistinctIPs uint64 }
```

Use `net.IPv6zero` for a missing primary IP and failed probe IP array entries. Assert all three probe arrays equal `ProbesAttempted` before enqueue.

- [ ] **Step 2: Write bounded-writer tests**

Use a fake batch seam behind the writer. Test explicit insert column order, flush at batch size, interval flush, queue-full returns `false` and increments `Dropped`, append/send errors increment `FlushErrors`, `Flush` is a barrier for every event accepted before it, and `Close` drains queued events within its context.

- [ ] **Step 3: Implement the async writer**

Use one bounded `chan command`, a single flush goroutine, 100-row default batch, 2-second default flush interval, 4096-command default capacity, 15-second per-flush timeout, non-blocking event enqueue, rate-limited drop warnings, and idempotent close.

```go
type command struct { event *Event; barrier chan error }
```

`Enqueue` sends an event command; `Flush` sends a barrier command after the caller has stopped/waited for producers. The worker flushes its current batch before acknowledging the barrier, so every previously accepted event has either been inserted or counted as a flush error and a later insert cannot resurrect a deleted session. `appendArgs` must follow the migration's physical column order exactly.

- [ ] **Step 4: Add reader integration fixtures and expected report values**

Insert five deterministic events spanning two primary-IP runs and two categories. Assert newest-first page order at 50 rows per page, chronological streaming of the complete filtered export, time filtering, success percentage, quantiles, category counts, two hold durations, one rotation with cadence, monotonic cumulative pool growth, and deletion.

- [ ] **Step 5: Implement parameterized ClickHouse report queries**

Use direct `sample_events` reads. Series buckets use `toStartOfInterval(sampled_at, toIntervalSecond(?))`, `if(sum(probes_attempted)=0, 0, sum(probes_ok)/sum(probes_attempted))`, `if(countIf(probes_ok > 0)=0, 0, quantileExactIf(0.5)(rtt_med_ms, probes_ok > 0))`, the corresponding `0.95` expression, `avg(distinct_ips)`, and `countIf(primary_category = ...)`. Stickiness reads chronological `primary_ip`/`ip_changed` rows with `probes_ok > 0` and derives holds, from/to rotation events, seconds since the previous rotation, and average/median hold duration in Go so failed samples do not create a false `::` hold. Pool growth reads chronological `new_ips` and emits a cumulative sum.

Deletion must wait for completion:

```sql
ALTER TABLE sample_events DELETE WHERE session_id = ? SETTINGS mutations_sync = 1
```

- [ ] **Step 6: Run writer and ClickHouse tests**

Run: `go test -race ./internal/ch -v`

Expected: writer tests PASS; integration tests SKIP without `TEST_CLICKHOUSE_DSN` and PASS with it.

- [ ] **Step 7: Commit ClickHouse persistence**

```bash
git add internal/ch
git commit -m "feat: persist and query sample events"
```

### Task 7: Probe Execution and Deterministic Sample Aggregation

**Files:**
- Create: `internal/sampler/probe.go`
- Create: `internal/sampler/aggregate.go`
- Test: `internal/sampler/probe_test.go`
- Test: `internal/sampler/aggregate_test.go`

**Interfaces:**
- Consumes: a `proxy.ContextDialer`, probe target, timeout, previous primary IP, and session-seen IP set.
- Produces: `sampler.Prober`, `sampler.ProbeResult`, and `sampler.Aggregate(results, previous, seen) Sample` with deterministic ClickHouse-ready fields.

- [ ] **Step 1: Write Cloudflare trace probe tests**

Use a fake context dialer feeding an HTTP server and a target response:

```text
fl=123f45
h=speed.cloudflare.com
ip=203.0.113.7
loc=US
colo=IAD
```

Assert parsed IP/country/colo, positive RTT, request deadline, non-2xx failure, malformed/missing IP failure, and context cancellation. Verify a new transport with `DisableKeepAlives: true` is used for each probe so every probe establishes its own proxy connection.

- [ ] **Step 2: Implement the HTTP probe**

```go
type ProbeResult struct { IP netip.Addr; Country, Colo string; RTT time.Duration; Err error }
type Prober interface { Probe(context.Context, proxy.ContextDialer, string, time.Duration) ProbeResult }
type Sample struct {
	ProbesAttempted, ProbesOK int
	PrimaryIP netip.Addr
	DistinctIPs int
	IPChanged bool
	NewIPs int
	RTTMin, RTTMed, RTTMax time.Duration
	EgressCountry string
	ProbeIPs []netip.Addr
	ProbeRTTs []time.Duration
	ProbeOK []bool
	IPHits map[netip.Addr]int
	Error string
}

func (p HTTPProber) Probe(ctx context.Context, d proxy.ContextDialer, target string, timeout time.Duration) ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, timeout); defer cancel()
	transport := &http.Transport{DialContext: d.DialContext, DisableKeepAlives: true, TLSHandshakeTimeout: timeout}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil { return ProbeResult{Err: fmt.Errorf("build probe request: %w", err)} }
	start := p.now()
	resp, err := client.Do(req)
	rtt := p.now().Sub(start)
	if err != nil { return ProbeResult{RTT: rtt, Err: err} }
	defer resp.Body.Close()
	return parseTrace(io.LimitReader(resp.Body, 64<<10), rtt)
}
```

Reject non-2xx status before parsing and limit the trace body to 64 KiB.

- [ ] **Step 3: Write aggregation tests for all specified signals**

Cover mode IP; first-observed tie breaking; distinct IPs; min/even median/max RTT; primary country; previous-IP change; new session IPs; all-failed and partially failed samples; input order preservation in parallel arrays; UInt8 saturation guards; and failed probe representation as `::`, RTT `0`, OK `0`.

```go
func TestAggregateFoldsFailures(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.7")
	s := Aggregate([]ProbeResult{{IP: ip, Country: "US", RTT: 100*time.Millisecond}, {Err: errors.New("timeout")}, {IP: ip, Country: "US", RTT: 200*time.Millisecond}}, netip.Addr{}, map[netip.Addr]struct{}{})
	if s.ProbesAttempted != 3 || s.ProbesOK != 2 || s.RTTMed != 150*time.Millisecond { t.Fatalf("sample=%#v", s) }
	if len(s.ProbeOK) != 3 || s.ProbeOK[1] || s.ProbeIPs[1].IsValid() { t.Fatalf("probe arrays=%#v", s) }
}
```

- [ ] **Step 4: Implement deterministic aggregation**

Count only successful probes with valid IPs. Choose the highest-frequency primary IP; ties go to the IP that appears in the earliest probe. For an even RTT count, median is the integer average of the two central durations. `IPChanged` is false when either previous or current primary is invalid. `NewIPs` counts distinct sample IPs absent from the passed session-seen set. Preserve probe input order in arrays. Set the sample-level error to empty when at least one probe succeeds; when all probes fail, join de-duplicated probe error messages in probe order and cap the stored string at 2 KiB.

- [ ] **Step 5: Run sampler unit tests**

Run: `go test ./internal/sampler -run 'TestHTTPProber|TestAggregate' -v`

Expected: PASS.

- [ ] **Step 6: Commit probe and aggregation behavior**

```bash
git add internal/sampler
git commit -m "feat: probe and aggregate proxy samples"
```

### Task 8: Durable Session Worker and Supervisor

**Files:**
- Create: `internal/sampler/worker.go`
- Create: `internal/sampler/supervisor.go`
- Test: `internal/sampler/worker_test.go`
- Test: `internal/sampler/supervisor_test.go`

**Interfaces:**
- Consumes: `session.Store`, `crypto.Cipher`, `proxydial.FromURL`, `sampler.Prober`, `enrich.Service`, and `ch.Writer`.
- Produces: `Supervisor.Resume(ctx) error`, `Supervisor.Start(ctx, sessionID) error`, `Supervisor.Stop(ctx, sessionID) error`, `Supervisor.Delete(ctx, sessionID) error`, and `Supervisor.Wait()`.

- [ ] **Step 1: Define worker seams and write one-tick tests**

```go
type ReputationLookup interface { Lookup(context.Context, netip.Addr) (session.Reputation, error) }
type EventSink interface { Enqueue(ch.Event) bool; Flush(context.Context) error }
type DialerFactory func(string, time.Duration) (proxy.ContextDialer, error)
```

Use fakes to prove one tick: runs exactly N probes with at most eight in flight; enriches each new/stale IP through the cache-aware service; records per-IP hit counts; denormalizes primary category/risk; enqueues one event; and persists one atomic Postgres tick. An enrichment error must still enqueue and save the sample.

- [ ] **Step 2: Implement worker preparation and the tick**

Add `Worker.Prepare(ctx, session) (*PreparedWorker, error)`, which decrypts the proxy URL, builds its dialer, loads `SessionIPs` into `seen`, and seeds `previousPrimary` and `sampleSeq` from the Postgres snapshot before a goroutine is published. This makes `Supervisor.Start` return credential/dialer/store errors synchronously; `Resume` fails startup loudly instead of leaving an unowned running row. `PreparedWorker.Run(ctx)` then runs one sample immediately followed by cadence ticks. Limit per-session probe concurrency with a weighted semaphore of `min(8, probes_per_sample)` and place results back at their original indexes.

After aggregation:

1. Call enrichment for each distinct successful IP; retain partial/unknown results on error.
2. Build the `ch.Event`, next rolling snapshot, next seen-IP set, and `IPHit` values without mutating worker state.
3. Call `SaveTick`; on a Postgres error log a warning, do not enqueue the event, and retry at the next cadence. This intentionally prefers a possible ClickHouse gap over duplicate sample rows because ClickHouse writes are already loss-tolerant and there is no cross-database transaction.
4. After `SaveTick` succeeds, publish the next in-memory sequence/previous/seen state and enqueue the event; log and count a full-queue drop without ending the worker.

- [ ] **Step 3: Write cap, cancellation, and resume tests with a fake clock**

Test: manual cancellation does not change the database status; `max_samples` marks finished after the capped sample; `max_duration` uses persisted `started_at`, including after restart; process-root cancellation exits and leaves status running; and resuming from `samples_taken=12` emits sequence 13 with the prior primary IP.

- [ ] **Step 4: Implement cap semantics**

Before starting any tick, check persisted/in-memory `samples_taken >= max_samples` and elapsed time since persisted `started_at >= max_duration_seconds`; this handles a crash after the capped tick was saved but before status changed. Re-check `max_samples` after each successfully persisted tick so the normal path marks finished immediately. A reached cap calls `Store.Finish` and exits. Root context cancellation exits without calling `Stop` or `Finish`, preserving restart durability.

- [ ] **Step 5: Write supervisor lifecycle tests**

Prove `Resume` starts every and only `running` row; repeated `Start` is idempotent; `Stop` cancels the worker, waits for it, then atomically marks the row stopped; stop on a non-running session returns `session.ErrNotRunning`; `Delete` stops a running worker, crosses the writer flush barrier, synchronously deletes ClickHouse data, then deletes Postgres; and root cancellation waits for all workers.

- [ ] **Step 6: Implement race-safe supervisor ownership**

```go
type Supervisor struct {
	store session.Store; sink EventSink; reader interface{ DeleteSession(context.Context, uuid.UUID) error }
	workerFactory func(session.Session) *Worker
	mu sync.Mutex; workers map[uuid.UUID]*runningWorker; root context.Context; wg sync.WaitGroup
}
type runningWorker struct { cancel context.CancelFunc; done chan struct{} }
```

Guard the map with `mu`; never wait on `done` while holding the mutex. `Start` fetches `SessionByID`, rejects non-running rows, runs `Worker.Prepare`, and double-checks the map before publishing the prepared goroutine. `Delete` waits for the stopped worker, calls `sink.Flush`, then removes ClickHouse data before Postgres so a CH failure leaves the recoverable Postgres control row intact and no queued insert can recreate deleted samples.

- [ ] **Step 7: Run the sampler under the race detector**

Run: `go test -race ./internal/sampler -v`

Expected: PASS with no leaked worker goroutines.

- [ ] **Step 8: Commit durable sampling**

```bash
git add internal/sampler
git commit -m "feat: supervise durable sampling sessions"
```

### Task 9: OpenAPI Contract and Session Control API

**Files:**
- Create: `api/openapi.yaml`
- Create: `internal/api/openapi/openapi.gen.go` via generation
- Create: `internal/api/server.go`
- Create: `internal/api/errors.go`
- Test: `internal/api/server_test.go`

**Interfaces:**
- Consumes: `session.Store`, `crypto.Cipher`, `proxydial.Display`, and supervisor start/stop/delete methods.
- Produces: strict generated handlers for create/list/detail/stop/delete and stable JSON errors.

- [ ] **Step 1: Write the OpenAPI document and schemas**

Define OpenAPI 3.0.3 operations for the exact public paths `/api/sessions`, `/api/sessions/{id}`, `/api/sessions/{id}/stop`, `/api/sessions/{id}/report`, `/api/sessions/{id}/samples`, `/api/sessions/{id}/export.csv`, `/healthz`, and `/readyz`. For creation, use:

```yaml
CreateSessionRequest:
  type: object
  required: [name, proxy, mode, cadence_seconds]
  properties:
    name: {type: string, minLength: 1, maxLength: 100}
    proxy: {type: string, minLength: 1, writeOnly: true}
    mode: {type: string, enum: [sticky, pool]}
    cadence_seconds: {type: integer, minimum: 1}
    probes_per_sample: {type: integer, minimum: 1, maximum: 255}
    probe_target: {type: string, format: uri}
    dial_timeout_ms: {type: integer, minimum: 100, default: 10000}
    max_samples: {type: integer, minimum: 1, nullable: true}
    max_duration_seconds: {type: integer, minimum: 1, nullable: true}
```

`Session` never contains ciphertext, nonce, or plaintext proxy. Include snapshot fields and derived `success_rate`. Define reusable `Error{code,message}` responses for 400, 404, 409, 500, and 503. Define sample/report schemas in Task 10 now so one generated contract remains authoritative.

Use operation IDs `createSession`, `listSessions`, `sessionByID`, `stopSession`, `deleteSession`, `sessionReport`, `sessionSamples`, `exportSessionCSV`, `healthz`, and `readyz`. The response models have these exact JSON fields:

```yaml
Session:
  required: [id, name, proxy_display, mode, status, cadence_seconds, probes_per_sample,
    probe_target, dial_timeout_ms, samples_taken, probes_ok, probes_total, success_rate,
    distinct_ips, created_at]
  properties:
    id: {type: string, format: uuid}
    name: {type: string}
    proxy_display: {type: string}
    mode: {type: string, enum: [sticky, pool]}
    status: {type: string, enum: [running, stopped, finished]}
    cadence_seconds: {type: integer}
    probes_per_sample: {type: integer}
    probe_target: {type: string, format: uri}
    dial_timeout_ms: {type: integer}
    max_samples: {type: integer, nullable: true}
    max_duration_seconds: {type: integer, nullable: true}
    samples_taken: {type: integer}
    probes_ok: {type: integer, format: int64}
    probes_total: {type: integer, format: int64}
    success_rate: {type: number, format: double, minimum: 0, maximum: 1}
    distinct_ips: {type: integer}
    last_sample_at: {type: string, format: date-time, nullable: true}
    last_primary_ip: {type: string, nullable: true}
    last_primary_category: {type: string, nullable: true}
    last_rtt_ms: {type: integer, nullable: true}
    last_error: {type: string, nullable: true}
    created_at: {type: string, format: date-time}
    started_at: {type: string, format: date-time, nullable: true}
    stopped_at: {type: string, format: date-time, nullable: true}

SampleEvent:
  required: [sample_seq, sampled_at, probes_attempted, probes_ok, distinct_ips,
    ip_changed, new_ips, rtt_min_ms, rtt_med_ms, rtt_max_ms, primary_category,
    primary_risk, probe_ips, probe_rtts_ms, probe_ok, error]
  properties:
    sample_seq: {type: integer, format: int64}
    sampled_at: {type: string, format: date-time}
    probes_attempted: {type: integer}
    probes_ok: {type: integer}
    primary_ip: {type: string, nullable: true}
    distinct_ips: {type: integer}
    ip_changed: {type: boolean}
    new_ips: {type: integer}
    rtt_min_ms: {type: integer}
    rtt_med_ms: {type: integer}
    rtt_max_ms: {type: integer}
    egress_country: {type: string}
    primary_category: {type: string}
    primary_risk: {type: integer}
    probe_ips: {type: array, items: {type: string, nullable: true}}
    probe_rtts_ms: {type: array, items: {type: integer}}
    probe_ok: {type: array, items: {type: boolean}}
    error: {type: string}
```

`SessionReport` requires `series`, `stickiness`, `pool_growth`, `pool_composition`, `reputation_summary`, `risk_histogram`, and `ips`. A series point has `at`, `success_rate`, `latency_p50_ms`, `latency_p95_ms`, `distinct_per_sample`, `ip_changes`, `mobile`, `residential`, `datacenter`, and `unknown`. Every `success_rate` is a `0..1` ratio; the UI formats it as a percentage. Stickiness has `holds`, `rotations`, `average_hold_seconds`, and `median_hold_seconds`; rotations expose `at`, `from_ip`, `to_ip`, and `since_previous_seconds`. Pool composition has the four category counts. Reputation summary has `total_ips`, `flagged_ips`, `flagged_percent` on a `0..100` scale, and `dnsbl_hit_ips`. Each risk bucket has `label`, `min`, `max`, and `count`. Each IP row exposes `ip`, `category`, `country`, `isp`, `asn`, `risk_score`, `greynoise_class`, `dnsbl_listed`, `dnsbl_hits`, `first_seen`, `last_seen`, and `hit_count`.

- [ ] **Step 2: Generate server types and verify they compile**

Run: `make openapi-generate && go test ./internal/api/openapi`

Expected: PASS.

- [ ] **Step 3: Write handler tests against the real chi router**

Test sticky defaults to 3 probes and pool defaults to 8 when omitted; explicit probes win; default probe target/dial timeout; validation rejects malformed proxy URLs before encryption; created sessions are persisted as running with `started_at`; supervisor start failure returns 500 and marks the just-created session stopped to avoid an orphaned running row; list/detail redact secrets; stop maps not-running to 409; delete maps missing to 404; and every error has JSON content type and stable code.

- [ ] **Step 4: Implement strict handlers and mapping helpers**

```go
type Control interface {
	Start(context.Context, uuid.UUID) error
	Stop(context.Context, uuid.UUID) error
	Delete(context.Context, uuid.UUID) error
}
type Server struct { store session.Store; control Control; cipher *cryptox.Cipher; defaults Defaults }
```

Creation trims the name, validates `http`/`https` probe target, validates the proxy by calling `Display`, encrypts it, creates a UUID, stores it with status running and UTC `started_at`, then starts the supervisor. Use `201` with the mapped session. Stop and delete return `204`. Compute success rate as `probes_ok / probes_total`, with zero total represented as `0`.

- [ ] **Step 5: Register generated strict handlers at their public paths**

Wrap `openapi.NewStrictHandler` with request-error and response-error hooks that use the stable JSON error shape, then register `openapi.HandlerFromMux` on the root chi router. Do not add a second prefix: the generated paths already contain `/api` where required, while health/readiness remain at the root. Do not install authentication middleware.

- [ ] **Step 6: Run API unit tests**

Run: `go test ./internal/api -run 'TestCreate|TestList|TestStop|TestDelete' -v`

Expected: PASS and no response contains a proxy password.

- [ ] **Step 7: Commit the control API**

```bash
git add api internal/api oapi-codegen.yaml
git commit -m "feat: expose sampling session control API"
```

### Task 10: Reports, Grouped Samples, CSV, and Health API

**Files:**
- Create: `internal/api/report.go`
- Create: `internal/api/csv.go`
- Create: `internal/api/health.go`
- Test: `internal/api/report_test.go`
- Test: `internal/api/csv_test.go`
- Test: `internal/api/health_test.go`
- Modify: `internal/api/server.go`

**Interfaces:**
- Consumes: `ch.Reader`, `session.Store`, and generated report/sample response types.
- Produces: `/report`, `/samples`, `/export.csv`, `/healthz`, and `/readyz`.

- [ ] **Step 1: Write report composition tests**

Use fake CH series/stickiness/growth and Postgres IP inventory. Assert one response contains `series`, `stickiness`, `pool_growth`, `pool_composition`, `reputation_summary`, risk histogram buckets `0-9` through `90-100`, and `ips`. Flagged means any of: ProxyCheck proxy true, risk at least 70, GreyNoise malicious, StopForumSpam appears, or DNSBL listed. DNSBL-hit count is the number of IPs with a true listing, not the number of zones.

- [ ] **Step 2: Implement report time windows and composition**

Default report range is session `started_at` through now. Use 5-minute buckets for ranges up to 48 hours and 1-hour buckets for longer ranges. Query CH series/stickiness/growth and Postgres IPs; calculate reputation summary and risk histogram in Go; return partial empty arrays rather than null arrays. A missing session is 404; a dependency error is 503 with `code: dependency_unavailable`.

- [ ] **Step 3: Write sample page and CSV tests**

Samples use fixed page size 50, page minimum 1, optional RFC3339 `from`/`to`, newest-first rows, and preserve parallel arrays. CSV emits one row per attempted probe with this exact header:

```text
sample_seq,sampled_at,probe_index,probe_ok,probe_ip,probe_rtt_ms,primary_ip,sample_probes_ok,sample_probes_attempted,rtt_min_ms,rtt_med_ms,rtt_max_ms,ip_changed,new_ips,primary_category,primary_risk,error
```

Use `encoding/csv`; failed probe IP is empty in CSV, not `::`. Assert quoted errors containing commas/newlines and `Content-Disposition: attachment; filename="<sanitized-name>-<id>.csv"`.

- [ ] **Step 4: Implement samples and streaming CSV handlers**

Parse query values once, reject `from > to`, and call `Reader.StreamSamples` so CSV is chronological and does not construct a second full copy. Sanitize filename characters to `[A-Za-z0-9._-]`, replacing runs of others with `-`.

- [ ] **Step 5: Write and implement health semantics**

`GET /healthz` returns `200 {"status":"ok"}` without dependency checks. `GET /readyz` concurrently pings Postgres and ClickHouse with a 2-second timeout; it returns 200 only when both pass, otherwise 503 with booleans for `postgres` and `clickhouse` and no DSNs/errors that could reveal credentials.

- [ ] **Step 6: Run all API tests**

Run: `go test ./internal/api -v`

Expected: PASS.

- [ ] **Step 7: Commit reporting endpoints**

```bash
git add internal/api api/openapi.yaml
git commit -m "feat: add sampling reports and exports"
```

### Task 11: Dashboard Foundation, Home, and Session Creation

**Files:**
- Create: `web/package.json`
- Create: `web/package-lock.json`
- Create: `web/.gitignore`
- Create: `web/tsconfig.json`
- Create: `web/vite.config.ts`
- Create: `web/src/main.tsx`
- Create: `web/src/app.tsx`
- Create: `web/src/index.css`
- Create: `web/src/lib/api.ts`
- Create: `web/src/lib/format.ts`
- Create: `web/src/components/ui/alert.tsx`
- Create: `web/src/components/ui/badge.tsx`
- Create: `web/src/components/ui/button.tsx`
- Create: `web/src/components/ui/collapsible.tsx`
- Create: `web/src/components/ui/dialog.tsx`
- Create: `web/src/components/ui/input.tsx`
- Create: `web/src/components/ui/label.tsx`
- Create: `web/src/components/ui/select.tsx`
- Create: `web/src/components/ui/skeleton.tsx`
- Create: `web/src/components/ui/table.tsx`
- Create: `web/src/components/ui/tabs.tsx`
- Create: `web/src/components/ui/tooltip.tsx`
- Create: `web/src/components/app-shell.tsx`
- Create: `web/src/components/session-status-badge.tsx`
- Create: `web/src/components/session-table.tsx`
- Create: `web/src/components/new-session-dialog.tsx`
- Create: `web/src/pages/home-page.tsx`
- Create: `web/src/test/setup.ts`
- Test: `web/src/pages/home-page.test.tsx`
- Test: `web/src/components/new-session-dialog.test.tsx`

**Interfaces:**
- Consumes: generated API JSON shapes and `/api/sessions` endpoints.
- Produces: responsive `/` dashboard, active/inactive session tables, and create/start workflow with 5-second polling.

- [ ] **Step 1: Scaffold the TypeScript app and install focused dependencies**

Run:

```bash
npm create vite@latest web -- --template react-ts
cd web && npm install react-router-dom @tanstack/react-query recharts lucide-react clsx tailwind-merge class-variance-authority zod react-hook-form @hookform/resolvers
cd web && npm install -D vitest jsdom @testing-library/react @testing-library/user-event @testing-library/jest-dom
cd web && npx shadcn@latest init -d
cd web && npx shadcn@latest add alert badge button collapsible dialog input label select skeleton table tabs tooltip
```

Commit the resulting lockfile. Configure Vite dev proxy `/api`, `/healthz`, and `/readyz` to `http://localhost:8080`; set Vitest to jsdom with `src/test/setup.ts` importing `@testing-library/jest-dom/vitest`.

Replace Vite's generated `web/.gitignore` with:

```gitignore
node_modules/
coverage/
```

This prevents the scaffold's default `dist` rule from hiding the embedded production assets committed in Task 13.

- [ ] **Step 2: Establish the dashboard visual system**

Physical scene: an operator reviews long-running proxy experiments on a bright office display and needs calm density, immediate state recognition, and readable tables. Use a restrained light theme, system sans, 10px component radius, no decorative gradients, no glass, no nested cards, and OKLCH tokens:

```css
:root {
  --background: oklch(1 0 0);
  --surface: oklch(0.97 0.006 120);
  --foreground: oklch(0.20 0.025 120);
  --muted-foreground: oklch(0.46 0.025 120);
  --border: oklch(0.88 0.01 120);
  --primary: oklch(0.36 0.09 120);
  --primary-foreground: oklch(1 0 0);
  --accent: oklch(0.50 0.16 45);
  --ring: oklch(0.52 0.11 120);
  --success: oklch(0.56 0.14 145);
  --warning: oklch(0.70 0.15 75);
  --danger: oklch(0.56 0.18 28);
  --chart-mobile: oklch(0.58 0.14 145);
  --chart-residential: oklch(0.58 0.14 245);
  --chart-datacenter: oklch(0.60 0.17 35);
  --chart-unknown: oklch(0.60 0.02 120);
  --radius: 0.625rem;
}
```

Use the accent only for warnings/high-risk marks; primary is reserved for actions, focus, and current selection. Add visible keyboard focus, 44px mobile hit targets, tabular numerals for metrics, `prefers-reduced-motion`, and structural breakpoints rather than fluid heading sizes.
The planned light-theme WCAG contrast ratios are foreground/background 18.04:1, muted/background 7.07:1, white/primary 10.63:1, and white/accent 6.41:1; preserve or improve them when mapping tokens into shadcn variables.

- [ ] **Step 3: Implement the typed HTTP client and query hooks**

```ts
export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {headers: {'Content-Type': 'application/json', ...init?.headers}, ...init})
  if (!response.ok) {
    const body = await response.json().catch(() => ({message: response.statusText}))
    throw new APIError(response.status, body.code ?? 'request_failed', body.message ?? 'Request failed')
  }
  return response.status === 204 ? undefined as T : response.json()
}
```

Define TypeScript types matching OpenAPI field names exactly. `useSessions` polls every five seconds only while the document is visible and contains at least one running session. Mutation success invalidates `['sessions']`; errors surface in an inline alert or dialog message, not only a toast.

- [ ] **Step 4: Write home page behavior tests**

Mock `fetch` and assert: skeleton rows while loading; a useful empty state with “Start your first sampling session”; active and stopped/finished headings; required columns; row click navigation; stop button does not trigger row navigation; dependency error with retry; and narrow layout exposes each row as a labeled stacked record without horizontal page overflow.

- [ ] **Step 5: Implement the app shell and session tables**

Use one compact top bar with product name, readiness indicator, and “New session” primary action. The content width is `min(1440px, calc(100% - 32px))`. Desktop tables show name, redacted proxy, mode, status, success rate, median RTT, distinct IPs, last IP/category, and last sample time. At widths below 760px, render the same data as semantic `<dl>` rows; do not squeeze a ten-column table.

The empty state explains sticky versus pool in two sentences and keeps the create action in the normal page flow.

- [ ] **Step 6: Write create-dialog validation tests**

Assert proxy/name/mode/cadence required; sticky mode sets probes to 3 and pool mode to 8 only until the user manually edits probes; caps accept blank or positive integers; password field is never echoed after submit; submit loading disables controls; success closes and navigates to `/sessions/:id`; server validation stays in the dialog.

- [ ] **Step 7: Implement the create form**

Use React Hook Form + Zod. Show advanced fields (probe target, timeout, optional caps) in an inline collapsible section inside the dialog, not a second modal. Labels include units. Use copy “Proxy connection string” and examples for `socks5h://` and `http://`; set `autoComplete="off"`, `spellCheck={false}`, and `type="password"` for the connection string.

- [ ] **Step 8: Run frontend smoke tests and build**

Run: `cd web && npm test -- --run && npm run build`

Expected: Vitest PASS and Vite emits `web/dist/index.html` plus hashed assets.

- [ ] **Step 9: Commit the dashboard foundation**

```bash
git add web
git commit -m "feat: add session dashboard and creation flow"
```

### Task 12: Session Detail Charts, Reputation, and Grouped Probe Table

**Files:**
- Create: `web/src/pages/session-page.tsx`
- Create: `web/src/components/report/summary-strip.tsx`
- Create: `web/src/components/report/timeseries-charts.tsx`
- Create: `web/src/components/report/composition-charts.tsx`
- Create: `web/src/components/report/stickiness-timeline.tsx`
- Create: `web/src/components/report/risk-histogram.tsx`
- Create: `web/src/components/report/ip-table.tsx`
- Create: `web/src/components/report/grouped-samples-table.tsx`
- Test: `web/src/pages/session-page.test.tsx`
- Test: `web/src/components/report/grouped-samples-table.test.tsx`

**Interfaces:**
- Consumes: `/api/sessions/{id}`, `/report`, `/samples?page=`, `/stop`, and `/export.csv`.
- Produces: responsive session route with live report polling, all specified charts, per-IP table, expandable grouped sample rows, stop, and export.

- [ ] **Step 1: Write route-level loading, error, refresh, and action tests**

Assert detail/report/sample skeletons; 404 navigation back home; partial report empty states; five-second polling only while running and visible; stopped sessions stop polling; stop confirmation and invalidation; export link; “last updated” timestamp; and all chart sections have accessible headings and text summaries.

- [ ] **Step 2: Implement session information hierarchy**

Top row: breadcrumb, session name, status badge, redacted proxy, mode, stop button only while running, and CSV export. Next: one border-separated summary strip for success rate, median RTT, distinct IPs, samples, last IP, and last sample—do not wrap each metric in a separate card. Then tabs `Overview`, `Reputation`, and `Samples`, with the current tab reflected in `?tab=` so refresh/back navigation is stable.

- [ ] **Step 3: Implement charts with shared semantics**

Overview uses:

- success-rate line with API domain `0..1`, fixed Y domain `[0, 1]`, and tick labels `0–100%`;
- latency p50/p95 lines with milliseconds formatting;
- composition stacked area using stable mobile/residential/datacenter/unknown tokens;
- pool donut with an adjacent text legend and counts;
- stickiness timeline made from CSS-positioned run segments rather than a misleading continuous line;
- cumulative pool-growth line; and
- risk histogram with threshold 70 visibly marked.

Every Recharts component receives `responsiveContainer`, a keyboard-readable nearby table or summary sentence, UTC timestamps rendered in the browser's local zone with the zone in the tooltip, and no entrance animation when reduced motion is requested. Empty datasets render explanatory copy instead of empty axes.

- [ ] **Step 4: Implement the per-IP reputation table**

Columns: IP, category, ISP, ASN, risk, GreyNoise, DNSBL, first seen, last seen, and hit count. Default order is hit count descending. Use text plus color for category/risk states; expose full ISP/ASN in a tooltip without clipping inside the scroll container. On narrow screens keep IP/category/risk visible and put remaining fields in an expandable detail row.

- [ ] **Step 5: Write grouped-sample interaction tests**

Assert newest-first sample rows; disclosure button `aria-expanded`; expanding sample sequence 12 renders exactly N probe subrows in original array order; failed probe displays “Failed” and the sample error; pagination preserves the tab; malformed mismatched arrays render safely using `probes_attempted`; and collapse restores focus to the disclosure button.

- [ ] **Step 6: Implement the grouped table**

The parent row shows sampled time, sequence, success `ok/attempted`, primary IP, distinct/new IPs, IP-change badge, min/median/max RTT, category/risk, and error. Expanded child rows show probe index, status, egress IP, and RTT. Use a real button in the first cell; do not make the entire row an unlabeled click target. Fetch only the current fixed-size page.

- [ ] **Step 7: Run detail-view tests and production build**

Run: `cd web && npm test -- --run && npm run build`

Expected: PASS with no React key, act, or accessibility warnings.

- [ ] **Step 8: Commit full reporting UI**

```bash
git add web
git commit -m "feat: visualize sampling reports"
```

### Task 13: Embedded SPA, Application Wiring, Telemetry, and Graceful Shutdown

**Files:**
- Create: `web/embed.go`
- Create: `internal/api/spa.go`
- Create: `internal/api/router.go`
- Create: `internal/telemetry/otel.go`
- Create: `cmd/app/main.go`
- Test: `internal/api/spa_test.go`
- Test: `cmd/app/main_test.go`

**Interfaces:**
- Consumes: all lower-level constructors and built `web/dist`.
- Produces: a single runnable `proxy-sampler` process that resumes sessions, serves API/SPA, instruments HTTP, and shuts down without changing running-session status.

- [ ] **Step 1: Add the embed package and SPA fallback tests**

```go
package web

import "embed"

//go:embed dist/*
var FS embed.FS
```

Test `/` serves `index.html`, hashed assets have their correct content type and cache headers, `/sessions/<uuid>` falls back to `index.html`, missing `/api/*` remains JSON 404 rather than SPA HTML, and path traversal cannot escape the embedded filesystem.

- [ ] **Step 2: Implement the chi router order**

Register the generated `/api/*`, `/healthz`, and `/readyz` routes before SPA fallback. Static hashed assets receive `Cache-Control: public, max-age=31536000, immutable`; `index.html` receives `no-cache`. Add request IDs, recovery, real-IP, and structured request logging. Wrap the final handler with `otelhttp.NewHandler`.

- [ ] **Step 3: Add optional standard OpenTelemetry setup**

Implement telemetry initialization in `internal/telemetry`. When `OTEL_EXPORTER_OTLP_ENDPOINT` is absent, install no exporters and return a no-op shutdown. When present, configure resource attributes with service name `proxy-sampler` and build OTLP HTTP trace/metric/log providers from standard `OTEL_*` environment variables. Bound shutdown to five seconds and never fail normal request handling because an export fails after startup.

- [ ] **Step 4: Write composition-root tests for startup failures and shutdown order**

Extract `run(ctx, getenv, dependencies)` seams sufficient to test missing config, migration failure, Postgres/ClickHouse ping failure, resume failure, server error, and cancellation. Assert signal cancellation makes the HTTP server stop accepting new work and sampler workers stop ticking, then waits for workers, drains/closes the ClickHouse writer, closes CH reader and Postgres, and finally shuts down telemetry. Running rows remain running.

- [ ] **Step 5: Implement startup wiring**

`cmd/app/main.go` must:

1. Load config and install structured logging.
2. Create signal context for SIGINT/SIGTERM.
3. Parse ClickHouse DSN; run Postgres and ClickHouse migrations when enabled.
4. Open and ping pgx + ClickHouse.
5. Construct DB store, cipher, provider stack, enrichment service, CH writer/reader, worker factory, supervisor, API server, and router.
6. Call `Supervisor.Resume(ctx)` before listening so running sessions are owned before readiness succeeds.
7. Serve with `ReadHeaderTimeout: 5s`, `ReadTimeout: 30s`, `WriteTimeout: 60s`, and `IdleTimeout: 120s`.
8. On cancellation, call `http.Server.Shutdown` with ten seconds, then wait/drain dependencies in the tested order.

- [ ] **Step 6: Run generated build, SPA tests, and process tests**

Run: `make web-build generate && go test ./internal/api ./cmd/app -v && go build ./cmd/app`

Expected: PASS and one Go binary contains the SPA assets.

- [ ] **Step 7: Commit the runnable service**

```bash
git add cmd internal/api internal/telemetry web/embed.go web/dist
git commit -m "feat: wire proxy sampler application"
```

Commit the generated `web/dist` output so an ordinary checkout can run `go test ./...` without first installing Node. The Docker build still rebuilds and replaces it from the lockfile before compiling Go.

### Task 14: Container, CI, Documentation, and End-to-End Verification

**Files:**
- Create: `Dockerfile`
- Create: `.dockerignore`
- Create: `.github/workflows/ci.yml`
- Create: `.env.example`
- Create: `README.md`
- Modify: `Makefile`

**Interfaces:**
- Consumes: completed Go service and frontend.
- Produces: reproducible image, continuous unit/build checks, operator configuration reference, and verified local workflow.

- [ ] **Step 1: Write the multi-stage Dockerfile**

```dockerfile
FROM node:24-bookworm-slim AS web-builder
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26 AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-builder /src/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/proxy-sampler ./cmd/app

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata && rm -rf /var/lib/apt/lists/*
COPY --from=go-builder /out/proxy-sampler /usr/local/bin/proxy-sampler
ENV HTTP_ADDR=:8080
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/proxy-sampler"]
```

Add a Docker healthcheck only if the target orchestrator does not supply one; otherwise document `/healthz` and `/readyz` rather than duplicating policy in the image.

Use this `.dockerignore` so local artifacts and plaintext environment files never enter build context:

```dockerignore
.git
.env
bin
web/node_modules
web/coverage
web/dist
```

- [ ] **Step 2: Add CI for deterministic codegen, tests, builds, and image construction**

Use separate jobs with Go 1.26 and Node 24. Run `go mod download`, `make generate`, `git diff --exit-code` to catch stale generated code, `go test -race ./...`, `npm ci`, `npm test -- --run`, `npm run build`, `go build ./cmd/app`, and `docker build .`. Do not require Postgres/ClickHouse secrets for normal CI; the gated integration suites remain a documented optional job until service containers are added intentionally.

- [ ] **Step 3: Document every configuration value and operational behavior**

`.env.example` includes non-secret examples for every design variable and a generated-key command:

```bash
openssl rand -base64 32
```

README sections: purpose; architecture; prerequisites; local Postgres/ClickHouse setup expectations; `make generate`, `make test`, and `make build`; environment table with defaults; creating a session; durability/resume behavior; graceful shutdown; health/readiness; CSV export; retention; integration test DSNs; key rotation limitation (existing rows require decrypting with the old key and re-encrypting before switching); and tailnet-only/no-auth deployment warning.

- [ ] **Step 4: Run formatting, static checks, and all local tests**

Run:

```bash
gofmt -w cmd internal migrations web/embed.go
go vet ./...
go test -race ./...
cd web && npm test -- --run && npm run build
cd .. && make generate && git diff --exit-code
go build ./cmd/app
docker build -t proxy-sampler:local .
```

Expected: every command exits 0; integration tests explicitly SKIP when DSNs are absent; code generation leaves no diff.

- [ ] **Step 5: Run dependency-backed integration tests**

With disposable Postgres and ClickHouse databases configured, run:

```bash
TEST_DATABASE_URL="$TEST_DATABASE_URL" TEST_CLICKHOUSE_DSN="$TEST_CLICKHOUSE_DSN" go test -race ./internal/migrate ./internal/chmigrate ./internal/db ./internal/ch -v
```

Expected: migrations apply idempotently, Postgres store tests PASS, ClickHouse writer/report/delete tests PASS, and a second run reports schemas current.

- [ ] **Step 6: Perform a manual durability smoke test**

Start the service with real local dependencies, create a capped session through the UI, verify samples and grouped probes appear, stop the process while a second uncapped session is running, restart with the same databases/key, and confirm the session resumes with increasing sequence numbers. Stop it in the UI, export CSV, delete it, and confirm Postgres rows and ClickHouse events are gone.

- [ ] **Step 7: Commit release infrastructure and docs**

```bash
git add Dockerfile .dockerignore .github .env.example README.md Makefile
git commit -m "chore: ship proxy sampler container and CI"
```

---

## Final Acceptance Checklist

- A created session stores no plaintext proxy credential and begins sampling without a process restart.
- SIGTERM cancels active workers, drains queued ClickHouse rows, and leaves active session status as `running` for boot-time resume.
- Manual stop produces `stopped`; sample/duration caps produce `finished`; both retain reports.
- All-failed samples are persisted with zero success and probe error details; enrichment outages never terminate a session.
- Fresh reputation cache records bypass providers; stale records refresh under the process-wide concurrency bound.
- Postgres list/detail data remains useful during a ClickHouse outage; readiness reports the outage.
- Report charts, IP inventory, risk summary, grouped sample/probe table, CSV, polling, stop, and delete match the OpenAPI contract.
- The SPA is keyboard-operable, responsive without page-level overflow, contrast-safe, reduced-motion aware, and provides loading, empty, error, disabled, and partial-data states.
- `go test -race ./...`, frontend tests/build, deterministic generation, Go build, dependency-backed integrations, and Docker build all pass.
