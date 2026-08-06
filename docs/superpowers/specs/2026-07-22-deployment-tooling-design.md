# proxy-sampler — Local Stack (Docker Compose) Design

2026-07-22 · adds a full-stack local `docker-compose.yml` to the existing `github.com/timo972/proxy-sampler` repo. (A Kubernetes/Helm deploy was considered and deferred — see Non-goals.)

## Problem

The repo already ships a production container (multi-stage `Dockerfile`, non-root `65532`, binary on `:8080`) and CI that builds it. What is missing is a one-command **local stack** that stands up the app together with its two backing datastores (Postgres + ClickHouse), so a contributor does not have to hand-provision databases before the service will start.

The stack must respect properties the application already enforces:

1. **Single instance.** On boot the service resumes `running` sessions from persisted counters; the runtime assumes one replica. Compose runs exactly one `app`.
2. **No auth layer.** Anyone who can reach `:8080` has full control, so exposure must default to private.
3. **Three required secrets** (`DATABASE_URL`, `CLICKHOUSE_DSN`, `ENCRYPTION_KEY`) with startup validation, plus `DB_AUTO_MIGRATE` that applies embedded migrations on boot (Postgres DB must pre-exist; ClickHouse DB is auto-created).

## Goals

- `docker compose up` brings up app + Postgres + ClickHouse locally, health-gated, with schema auto-applied and the app reachable on `127.0.0.1:8080`.
- No secrets are committed and no insecure default encryption key is baked into the file; the key is supplied at runtime.
- A short README section documents the flow.

## Non-goals

- **Kubernetes / Helm deployment.** Considered and deferred; not part of this change.
- Production or multi-host orchestration; this stack is for local development and evaluation.
- Multi-replica HA (the runtime assumes one replica).
- Registry publishing or CD wiring.

## `docker-compose.yml` (full local stack)

Three services on a private compose network. All published ports bind to `127.0.0.1` only, because the app has no auth.

### `postgres`
- Image `postgres:17-alpine`.
- Env `POSTGRES_DB=proxy_sampler`, `POSTGRES_USER=proxy_sampler`, `POSTGRES_PASSWORD=proxy_sampler`. Setting `POSTGRES_DB` satisfies the app's requirement that the Postgres database already exist.
- Named volume `pgdata` at `/var/lib/postgresql/data`.
- Healthcheck: `pg_isready -U proxy_sampler -d proxy_sampler`.
- Published `127.0.0.1:5432:5432` (convenience for host tooling).

### `clickhouse`
- Image `clickhouse/clickhouse-server:24.8`.
- Env `CLICKHOUSE_DB=proxy_sampler` plus a dedicated `CLICKHOUSE_USER=proxy_sampler` / `CLICKHOUSE_PASSWORD=proxy_sampler`. The image's built-in `default` user is locked to `127.0.0.1`/`::1` (it writes a `default-user.xml` restricting networks), so it is unusable by the app, which connects from a separate container. The image exposes a user created via `CLICKHOUSE_USER`/`CLICKHOUSE_PASSWORD` on all networks, so the app authenticates as that user.
- Named volume `chdata` at `/var/lib/clickhouse`.
- `ulimits.nofile` raised (262144) as the image expects.
- Healthcheck: `clickhouse-client --user proxy_sampler --password proxy_sampler --query "SELECT 1"` — uses the same credentials the app uses (not the localhost-only `default` user), so a healthy status reflects the app's actual auth path. The client binary is always present in the image, avoiding a dependency on `wget`/`curl`.
- Published `127.0.0.1:8123:8123` and `127.0.0.1:9000:9000`.

### `app`
- `build: .` (reuses the existing `Dockerfile`).
- `depends_on` both datastores with `condition: service_healthy`.
- Environment (compose-owned; DSNs point at service names, not `localhost`):
  - `DATABASE_URL=postgres://proxy_sampler:proxy_sampler@postgres:5432/proxy_sampler?sslmode=disable`
  - `CLICKHOUSE_DSN=clickhouse://proxy_sampler:proxy_sampler@clickhouse:9000/proxy_sampler`
  - `DB_AUTO_MIGRATE=true`
  - `ENCRYPTION_KEY=${ENCRYPTION_KEY:?set ENCRYPTION_KEY — generate with: openssl rand -base64 32}` — resolved from the existing `.env` via Compose variable substitution, failing fast with a clear message when unset. This reuses the README's key-generation flow rather than baking a key into the file.
  - Optional pass-through, defaulted to empty so they are only overridden when set: `OTEL_EXPORTER_OTLP_ENDPOINT`, `ENRICH_CONCURRENCY`, `REPUTATION_TTL`, `PROBE_TARGET_DEFAULT`.
- Published `127.0.0.1:8080:8080`.
- No Compose `healthcheck` on the app service — consistent with the Dockerfile's deliberate omission so the orchestrator owns liveness. Startup ordering is still gated on the datastores' healthchecks.

Named volumes `pgdata` and `chdata` are declared at the top level. A "Run the full stack with Docker Compose" section is added to `README.md`.

## Alternatives considered

- **App healthcheck in Compose.** Rejected to stay consistent with the Dockerfile's deliberate no-healthcheck stance; the datastore healthchecks already gate startup ordering.
- **Baking a dev encryption key into the file.** Rejected — the key is required and secret; resolving it from the environment with a fail-fast default keeps secrets out of the repo and reuses the README flow.

## Testing / verification

- `docker compose config` parses and interpolates (with `ENCRYPTION_KEY` set).
- `docker compose up` reaches a healthy app that serves `/healthz`, and `/readyz` passes once both datastores are healthy.
- With `ENCRYPTION_KEY` unset, `docker compose config`/`up` fails fast with the documented message.
