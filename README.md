# Proxy Sampler

Proxy Sampler repeatedly sends HTTP probes through a configured proxy, records egress IP and latency observations, enriches discovered IPs with reputation data, and presents the results in a browser UI and CSV export. It is intended for a small, trusted deployment used to examine sticky and rotating proxy behavior.

## Architecture

The Go service owns the HTTP API, sampler workers, and embedded React application. PostgreSQL is the durable control plane for sessions, encrypted proxy configuration, summary counters, IP inventory, and reputation cache. ClickHouse stores the higher-volume sample events used by reports and CSV export. A session created through the UI or API starts immediately; rows left in `running` state are resumed when the service starts again.

## Prerequisites

- Go 1.26
- Node.js 24 and npm
- PostgreSQL reachable through `DATABASE_URL`
- ClickHouse reachable through `CLICKHOUSE_DSN`
- Docker, if building the production image
- `openssl`, or another cryptographically secure generator, for the encryption key

The database users must be able to create and alter objects in their configured databases. With `DB_AUTO_MIGRATE=true`, startup creates the configured ClickHouse database when needed and applies both embedded migration sets. PostgreSQL itself must already exist. For manually managed schemas, set `DB_AUTO_MIGRATE=false` and apply the migrations before starting the service.

## Local setup

Create dedicated local PostgreSQL and ClickHouse databases, then prepare the environment:

```bash
cp .env.example .env
openssl rand -base64 32
# Replace ENCRYPTION_KEY in .env with that command's output.
set -a
. ./.env
set +a
```

`.env` is ignored by Git and the Docker build context. Never commit it or reuse the example database credentials outside a disposable local environment.

Install dependencies, generate checked-in code, test, and build:

```bash
make web-install
make generate
make test
make build
./bin/proxy-sampler
```

Open `http://localhost:8080`. Useful targets are:

| Target | Purpose |
| --- | --- |
| `make fmt` | Format Go source files. |
| `make vet` | Run Go static analysis. |
| `make test` | Run Go and frontend unit tests. Dependency-backed tests skip when their test DSNs are absent. |
| `make test-race` | Run all Go tests with the race detector. |
| `make generate` | Regenerate sqlc and OpenAPI Go output. |
| `make generate-check` | Regenerate and fail if the working tree changes. Use it from a clean tree. |
| `make build` | Build the frontend, regenerate code, and write `bin/proxy-sampler`. |
| `make integration-test` | Run the Postgres and ClickHouse integration packages with the race detector. |
| `make migrate-up` | Apply filesystem PostgreSQL migrations with `DATABASE_URL`. |
| `make ch-migrate-up` | Apply filesystem ClickHouse migrations with `CLICKHOUSE_DSN`. |
| `make docker-build` | Build `proxy-sampler:local`. |

## Run the full stack with Docker Compose

`docker-compose.yml` runs the service together with its PostgreSQL and ClickHouse datastores, so you do not have to provision databases separately. It builds the image from the local `Dockerfile`, applies migrations on startup, and binds every published port to `127.0.0.1` because the app has no authentication.

Provide the required encryption key, then start the stack:

```bash
cp .env.example .env
openssl rand -base64 32
# Replace ENCRYPTION_KEY in .env with that command's output.
docker compose up --build
```

Compose reads `ENCRYPTION_KEY` from `.env` (or your shell) and refuses to start if it is unset. The datastore DSNs are supplied by Compose and point at the internal `postgres` and `clickhouse` services, so the `localhost` DSNs in `.env` are ignored in this mode. The optional tunables (`OTEL_EXPORTER_OTLP_ENDPOINT`, `ENRICH_CONCURRENCY`, `REPUTATION_TTL`, `PROBE_TARGET_DEFAULT`) are passed through when present.

Once PostgreSQL and ClickHouse report healthy, the app starts and is reachable at `http://localhost:8080`. Stop the stack with `docker compose down`, or `docker compose down -v` to also discard the `pgdata` and `chdata` volumes.

## Configuration

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `DATABASE_URL` | yes | none | PostgreSQL connection URL for control-plane data. |
| `CLICKHOUSE_DSN` | yes | none | Native ClickHouse DSN for sample events and reports. |
| `ENCRYPTION_KEY` | yes | none | Exactly 32 random bytes encoded as standard base64. Generate with `openssl rand -base64 32`. |
| `HTTP_ADDR` | no | `:8080` | HTTP listen address. |
| `ENRICH_CONCURRENCY` | no | `2` | Positive process-wide limit for reputation provider calls. |
| `REPUTATION_TTL` | no | `24h` | Positive Go duration after which cached reputation data is refreshed. |
| `PROBE_TARGET_DEFAULT` | no | `https://speed.cloudflare.com/cdn-cgi/trace` | Default HTTP(S) probe URL for new sessions. |
| `DB_AUTO_MIGRATE` | no | `true` | Apply embedded PostgreSQL and ClickHouse migrations during startup. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | no | empty/disabled | Common OTLP/HTTP endpoint. When set, traces, metrics, and logs are exported; standard `OTEL_*` resource, header, TLS, and signal-specific variables are also honored by the OpenTelemetry SDK. |

Invalid or missing required configuration stops startup. `ENRICH_CONCURRENCY` and `REPUTATION_TTL` must be positive, and `DB_AUTO_MIGRATE` must be a Go boolean.

## Sessions and operations

Create sessions in the UI, or call the API directly:

```bash
curl --fail-with-body http://localhost:8080/api/sessions \
  -H 'Content-Type: application/json' \
  --data '{
    "name": "five-sample check",
    "proxy": "http://username:password@proxy.example:8080",
    "mode": "sticky",
    "cadence_seconds": 5,
    "max_samples": 5
  }'
```

Sticky sessions default to 3 probes per sample and pool sessions to 8. The default dial timeout is 10 seconds. `max_samples` and `max_duration_seconds` are optional caps; hitting either marks the session `finished` and retains its reports. A manual stop marks it `stopped` and also retains reports.

The list and detail views are backed by PostgreSQL and remain useful if ClickHouse reporting is unavailable. Reports, grouped samples, and CSV depend on ClickHouse. Export a session with `GET /api/sessions/{id}/export.csv`, stop it with `POST /api/sessions/{id}/stop`, and delete it with `DELETE /api/sessions/{id}`. Deleting a running session stops its worker first. Deletion then crosses the writer queue barrier, removes ClickHouse events, and finally removes the PostgreSQL session and its IP inventory.

Rename a session with `PATCH /api/sessions/{id}` and a run with `PATCH /api/runs/{id}`, sending the properties to change (`{"name": "..."}`). Renaming never touches sampling state, so it is valid in any status. A run's variant sessions are named `<run name> (axis=value)` when the run is created; renaming the run rewrites the variants that still carry that generated name and leaves any variant renamed on its own untouched.

On SIGINT or SIGTERM, the HTTP server stops accepting work, active samplers are cancelled and awaited, and the ClickHouse writer drains its accepted queue before connections close. Shutdown does not change active session rows from `running`; starting the service with the same databases and encryption key resumes them from their persisted sample counters, so sequence numbers continue rather than restarting.

`GET /healthz` is process liveness. `GET /readyz` checks both PostgreSQL and ClickHouse and returns HTTP 503 with per-dependency status when either is unavailable. The container intentionally has no baked-in healthcheck so the target orchestrator can set its own intervals and failure policy against these endpoints.

## Retention and keys

ClickHouse sample events have a 180-day table TTL. PostgreSQL session metadata and IP inventory remain until the session is deleted. Reputation cache rows are process-wide and are refreshed after `REPUTATION_TTL`; they are not deleted with an individual session.

Proxy URLs are encrypted in PostgreSQL with AES-GCM and API responses expose only a redacted display value. The service does not keep old encryption keys or perform automatic key rotation. Before changing `ENCRYPTION_KEY`, decrypt every existing session proxy with the old key and re-encrypt it with the new key; switching first makes existing sessions unreadable and prevents their resume. Back up the key separately from the databases.

## Integration tests

The dependency suites are deliberately gated so ordinary CI needs no database credentials:

```bash
TEST_DATABASE_URL='postgres://proxy_sampler:proxy_sampler@localhost:5432/proxy_sampler_test?sslmode=disable' \
TEST_CLICKHOUSE_DSN='clickhouse://localhost:9000/proxy_sampler_test' \
make integration-test
```

Use disposable test databases. The tests create and delete rows and may migrate their configured schema. With neither variable set, the relevant packages report explicit skips.

## Deployment security

This application has no authentication or authorization layer. Anyone who can reach it can create sessions containing proxy credentials, run probes, export observations, stop work, and delete data. Deploy it only on a trusted tailnet or equivalently private network, bind or firewall it accordingly, and do not expose it directly to the public internet. Use a trusted reverse proxy if transport encryption or access control is required.

Build and run the non-root container with the three required values supplied at runtime:

```bash
docker build -t proxy-sampler:local .
docker run --rm -p 8080:8080 \
  -e DATABASE_URL \
  -e CLICKHOUSE_DSN \
  -e ENCRYPTION_KEY \
  proxy-sampler:local
```

## License

Copyright (c) 2026 Timo Beckmann. Released under the [MIT License](LICENSE).
