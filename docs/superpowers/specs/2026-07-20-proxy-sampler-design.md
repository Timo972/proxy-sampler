# proxy-sampler — Design

2026-07-20 · new standalone repo `github.com/timo972/proxy-sampler` (module + binary `proxy-sampler`, entrypoint `cmd/app`)

## Problem

We evaluate third-party US residential/mobile proxy providers before relying on
them. Evaluation means pointing at a proxy endpoint
and sampling it over hours or days to measure: **stability** (is it always
reachable), **stickiness** (how long an egress IP is held / how fast the pool
rotates), **latency**, **composition** (mobile vs residential vs datacenter),
and **reputation** (is the egress IP flagged).

Until now this ran as ad-hoc Python scripts under `/tmp` on a laptop, launched
with `nohup`. A machine reboot killed the process and wiped `/tmp`, losing every
script, CSV, and cache. The sampling itself was sound; the execution and storage
were not durable.

`proxy-sampler` turns that workflow into a deployed, self-contained Go service:
it runs sampling sessions durably (survives restarts), stores results in
Postgres + ClickHouse, and serves a bundled dashboard to start, observe, and
report on samplings.

## Goals

- Start a sampling session against an **ad-hoc proxy connection string** and run
  it durably until stopped (or an optional cap is hit).
- Survive process restarts: running sessions resume automatically on boot.
- Measure stability, stickiness, latency, composition, and reputation, reusing
  the proven keyless enrichment stack.
- Persist to Postgres (control plane + IP inventory) and ClickHouse (per-sample
  time series).
- Serve a bundled React dashboard (tailnet-only) to manage sessions and view
  reports with charts and a probes-grouped-into-samples table.

## Non-goals (v1)

- Multi-replica HA (schema is reserved for it; runtime assumes one replica).
- Alerting / notifications (Slack can be added later).
- Commercial reputation APIs (the provider interface is ready; keyless is the
  default).
- Dashboard authentication (runs on a tailnet, not publicly exposed).
- Server-sent events for live updates (polling in v1).

## Architecture

One process, three concerns, one deployable container:

```
┌────────────────────────── proxy-sampler (single container) ──────────────────┐
│  HTTP server (chi)                                                            │
│    ├── /api/*   JSON control API (create/stop/list/report/export sessions)   │
│    └── /        embedded React SPA (go:embed web/dist)                        │
│                                                                              │
│  Supervisor — owns one goroutine per running session                         │
│    └── Session worker (ticker @ cadence)                                      │
│          each tick = one "sample":                                           │
│            1. N probes through the proxy → egress IP(s) + RTT                 │
│            2. enrich + classify each new/stale IP (cached, quota-bounded)     │
│            3. append one sample row (with per-probe arrays) → ClickHouse      │
│            4. upsert session_ips + the session rolling snapshot → Postgres    │
│                                                                              │
│  Postgres (control plane + IP inventory)   ClickHouse (per-sample series)     │
└──────────────────────────────────────────────────────────────────────────┘
```

### Durability

Session config lives in Postgres. On boot the supervisor loads every session
with `status = 'running'` and respawns its worker, so a restart or redeploy
resumes all active samplings — the concrete fix for the `/tmp` loss. On SIGTERM
the service stops ticking, flushes the ClickHouse writer, and leaves running
sessions as `running` so they resume on the next boot.

### Execution model

Single-replica, goroutine-per-session. A global semaphore
(`ENRICH_CONCURRENCY`) bounds all outbound enrichment across every session so
the keyless API rate limits are never tripped, independent of how many sessions
run. This is the simplest model that fits a monitoring tool.

*Future HA (out of scope):* a Postgres `claimed_by` / `lease_until` lease per
session lets exactly one replica own each session. The columns exist as nullable
reservations so HA is a code change, not a migration.

### Session lifecycle

`running → stopped` (manual) or `running → finished` (an optional `max_samples`
or `max_duration_seconds` cap is reached). Reports are viewable live and after
the session ends. Stopped/finished sessions retain all data.

## Data model

Split follows a control-plane-vs-insights separation: **Postgres**
holds mutable session/config plus the IP inventory and reputation cache;
**ClickHouse** holds the append-only per-sample time series.

### Postgres (goose migrations in `migrations/postgres`, sqlc queries in `internal/db/query`)

```sql
-- Session config + a light "current status" snapshot for the list view.
CREATE TABLE sampling_sessions (
    id                   uuid PRIMARY KEY,
    name                 text NOT NULL,
    proxy_ciphertext     bytea NOT NULL,          -- AES-GCM(connection string)
    proxy_nonce          bytea NOT NULL,
    proxy_display        text NOT NULL,           -- redacted host:port for UI
    mode                 text NOT NULL,           -- 'sticky' | 'pool'
    cadence_seconds      integer NOT NULL,
    probes_per_sample    integer NOT NULL,
    probe_target         text NOT NULL,           -- egress-trace URL
    dial_timeout_ms      integer NOT NULL,
    max_samples          integer,                 -- optional cap
    max_duration_seconds integer,                 -- optional cap
    status               text NOT NULL,           -- 'running' | 'stopped' | 'finished'
    -- rolling snapshot, upserted each tick (cheap list view; survives CH downtime)
    samples_taken        integer NOT NULL DEFAULT 0,
    probes_ok            bigint  NOT NULL DEFAULT 0,
    probes_total         bigint  NOT NULL DEFAULT 0,
    distinct_ips         integer NOT NULL DEFAULT 0,
    last_sample_at       timestamptz,
    last_primary_ip      inet,
    last_rtt_ms          integer,
    last_error           text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    started_at           timestamptz,
    stopped_at           timestamptz,
    -- reserved-nullable for future multi-replica HA (no migration later)
    claimed_by           text,
    lease_until          timestamptz
);

-- Distinct egress IPs seen per session → pool size, growth curve, composition.
CREATE TABLE session_ips (
    session_id  uuid NOT NULL REFERENCES sampling_sessions (id) ON DELETE CASCADE,
    ip          inet NOT NULL,
    first_seen  timestamptz NOT NULL DEFAULT now(),
    last_seen   timestamptz NOT NULL DEFAULT now(),
    hit_count   bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, ip)
);

-- Global, keyed by IP (shared across sessions → protects enrichment quotas).
CREATE TABLE ip_reputation_cache (
    ip              inet PRIMARY KEY,
    -- ip-api.com
    country         text,
    region          text,
    city            text,
    isp             text,
    asn             text,
    is_mobile       boolean,
    ipapi_proxy     boolean,
    ipapi_hosting   boolean,
    -- proxycheck.io
    pc_type         text,     -- Residential | Wireless | Business | DCH | ...
    pc_proxy        boolean,
    risk_score      integer,  -- 0-100
    -- greynoise + stopforumspam
    greynoise_class text,     -- benign | malicious | unknown
    sfs_appears     boolean,
    sfs_frequency   integer,
    -- DNSBLs
    dnsbl_listed    boolean,
    dnsbl_hits      text,     -- e.g. "zen.spamhaus.org;b.barracudacentral.org"
    -- derived
    category        text,     -- mobile | residential | datacenter | unknown
    raw             jsonb,    -- full provider payloads for debugging
    first_seen      timestamptz NOT NULL DEFAULT now(),
    refreshed_at    timestamptz NOT NULL DEFAULT now()
);
```

`refreshed_at` drives staleness: an IP older than `REPUTATION_TTL` is
re-enriched the next time it is seen.

### ClickHouse (goose migrations in `migrations/clickhouse`, async batch Writer)

One row per **sample** (aggregated over N probes). Per-probe detail is carried
in parallel arrays (length = `probes_attempted`), so the report can expand each
sample into its probe rows without a second table.

```sql
CREATE TABLE sample_events (
    session_id       UUID,
    sampled_at       DateTime64(3, 'UTC'),
    sample_seq       UInt32,
    probes_attempted UInt8,
    probes_ok        UInt8,                            -- → success rate
    primary_ip       IPv6,                             -- mode IP of the sample
    distinct_ips     UInt8,                            -- distinct within this sample
    ip_changed       UInt8,                            -- primary != previous sample's primary
    new_ips          UInt8,                            -- IPs never seen before in this session
    rtt_min_ms       UInt32,
    rtt_med_ms       UInt32,
    rtt_max_ms       UInt32,
    egress_country   LowCardinality(FixedString(2)),
    primary_category LowCardinality(String),           -- denormalized from cache at write time
    primary_risk     UInt8,
    probe_ips        Array(IPv6),                       -- per-probe egress IP (:: for a failed probe)
    probe_rtts_ms    Array(UInt32),                     -- per-probe RTT
    probe_ok         Array(UInt8),                      -- per-probe success
    error            String                             -- empty if the sample had ≥1 ok probe
)
ENGINE = MergeTree
PARTITION BY toDate(sampled_at)
ORDER BY (session_id, sampled_at)
TTL toDateTime(sampled_at) + INTERVAL 180 DAY;
```

**Two deliberate calls.** (1) No materialized views: a 24h session at 5-minute
cadence is ~288 rows, a week ~2k — charts aggregate `sample_events` directly on
read, so a SummingMergeTree MV pattern would be premature here. (2)
Reputation is denormalized into `primary_category` / `primary_risk` at write
time, so composition-over-time charts need no cross-DB join; pool-wide
composition and the per-IP reputation table come from Postgres
`session_ips ⋈ ip_reputation_cache`.

## Sampling engine

Packages: `internal/sampler` (supervisor + worker + probe loop), `internal/enrich`
(providers + classification), `internal/proxydial` (dialer from a connection
string), `internal/crypto` (AES-GCM).

### Probe — one connection through the proxy

1. Build a dialer from the session's connection string via
   `golang.org/x/net/proxy` (SOCKS5h — proxy-side DNS) or an HTTP CONNECT
   dialer, chosen by the URL scheme.
2. `GET` the `probe_target` (default `https://speed.cloudflare.com/cdn-cgi/trace`)
   through the proxy, timing the full request. Parse `ip=` (egress IP), `loc=`
   (country), `colo=`.
3. Return `(egress_ip, country, rtt, err)`. A dial/read failure is a **failed
   probe**, folded into the success rate — it never aborts the sample (the same
   way a latency sampler treats packet loss).

### Sample — one tick = N probes

- Run `probes_per_sample` probes (bounded concurrency per session).
- Aggregate: `primary_ip = mode(egress_ips)`, `distinct_ips = |set|`,
  `rtt_{min,med,max}` over successful probes, `ip_changed = primary != prev.primary`,
  `new_ips = |set \ session_seen|`.
- For each **new-or-stale** IP: enrich, then upsert `ip_reputation_cache`;
  upsert `session_ips` (first_seen / last_seen / hit_count).
- Write one `sample_events` row (with per-probe arrays), denormalizing the
  primary IP's `category` / `risk`.
- Upsert the session's rolling snapshot in Postgres.

Enrichment of new IPs happens inside the tick, bounded by the global semaphore
and per-request timeouts; cached IPs are instant. A slow enrichment delays a
tick but never fails a sample.

### Modes

Same code path; different intent and probe defaults:

- **sticky** — expects one held IP; default `probes_per_sample = 3`. Signal of
  interest: hold duration & rotation cadence (runs of `ip_changed`).
- **pool** — expects per-request rotation; default `probes_per_sample = 8`.
  Signal of interest: distinct-pool growth & composition (`session_ips`,
  `new_ips`).

### Enrichment provider stack

Keyless by default; each provider sits behind
`interface{ Lookup(ctx, ip) (partial, error) }` so a commercial key can be
slotted in later. A failing provider degrades that IP's record (fields stay
null) rather than failing the sample.

| Provider | Endpoint | Fields | Notes |
| --- | --- | --- | --- |
| **ip-api.com** | `http://ip-api.com/json/{ip}?fields=status,country,regionName,city,isp,as,mobile,proxy,hosting` | country, region, city, isp, asn, `mobile`, `proxy`, `hosting` | 45 req/min; HTTP-only on the free tier. The ASN org string — not the unreliable `hosting` flag — drives datacenter classification. |
| **proxycheck.io** | `https://proxycheck.io/v2/{ip}?vpn=1&risk=1` | `type` (Residential/Wireless/Business/DCH/…), `proxy`, `risk` 0-100 | Keyless ~100/day → the per-IP cache is what makes this viable. |
| **GreyNoise community** | `https://api.greynoise.io/v3/community/{ip}` (Accept: application/json) | `classification` (benign/malicious/unknown), `noise`, `riot` | **IPv4-only** — skip for IPv6. |
| **StopForumSpam** | `https://api.stopforumspam.org/api?ip={ip}&json` | `appears`, `frequency`, `lastseen` | Abuse history. |
| **DNSBLs** | `net.Resolver` A-lookup of `{reversed-ip}.{zone}` | listed + which zones | Zones: `zen.spamhaus.org`, `b.barracudacentral.org`, `dnsbl.sorbs.net`, `bl.spamcop.net`, `dnsbl-1.uceprotect.net`. **IPv4-only.** Done in Go via `net.Resolver`, not by shelling to `dig`. A `127.255.255.x` answer = ratelimited → treat as error, not listed. Spamhaus `127.0.0.10/.11` (PBL) is a benign dynamic-residential tag, not fraud. |

### Classification

`category` is derived once per IP and cached:

- `datacenter` if the ASN org matches a hosting/cloud pattern, or
  `proxycheck.type ∈ {DCH, Business, …}`.
- `mobile` if `ip-api.mobile` or `proxycheck.type = Wireless`.
- else `residential`.
- `unknown` if lookups failed.

Datacenter detection leans on the **ASN org**, because the manual runs showed
the `hosting` boolean lies (HostRush AS62633, Database Mart AS401479 returned
`hosting=false` but are datacenter).

### Caching & quota control

- `ip_reputation_cache` is global and keyed by IP: an IP seen across sessions is
  enriched once; `refreshed_at` older than `REPUTATION_TTL` (default 24h)
  triggers re-enrichment.
- One global semaphore (`ENRICH_CONCURRENCY`, small default) bounds all outbound
  enrichment across every session, so keyless rate limits hold regardless of
  session count.

## JSON API

chi router; spec in `api/openapi.yaml`; server stubs generated by oapi-codegen
into `internal/api`.

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/api/sessions` | Create **and start** a session (name, proxy string, mode, cadence, probes, optional caps, probe target). |
| `GET` | `/api/sessions` | List all sessions with the Postgres status snapshot. |
| `GET` | `/api/sessions/{id}` | Session detail + current snapshot. |
| `POST` | `/api/sessions/{id}/stop` | Stop a running session (worker cancelled, status → `stopped`). |
| `DELETE` | `/api/sessions/{id}` | Delete a session and its Postgres/ClickHouse data. |
| `GET` | `/api/sessions/{id}/report` | Composed report payload (below). |
| `GET` | `/api/sessions/{id}/samples?from&to&page` | Raw samples (each with per-probe arrays) for the grouped table / drill-down. |
| `GET` | `/api/sessions/{id}/export.csv` | CSV export. |
| `GET` | `/healthz`, `/readyz` | Liveness / readiness (Postgres + ClickHouse ping). |

The `/report` payload bundles everything the detail view charts:

- **series** (bucketed from ClickHouse): success rate, latency p50/p95,
  distinct-per-sample, IP-change events, composition-per-bucket.
- **stickiness**: hold durations + rotation cadence (derived from `ip_changed`
  runs).
- **pool**: cumulative distinct-IP growth curve.
- **reputation summary**: % flagged, DNSBL-hit count, risk-score histogram.
- **ips table**: `session_ips ⋈ ip_reputation_cache` → ip, category, isp, asn,
  risk, greynoise, dnsbl, first_seen, hit_count.

## Dashboard

React + Vite in `web/`, built to `web/dist`, embedded via `go:embed`; the Go
server serves the SPA at `/` and the API at `/api`. Components from **shadcn/ui**
(Table, Card, Badge, Dialog, Tabs, …); charts from **Recharts**. Simple but
fully featured. No auth (tailnet-only). Live updates via **polling** (a few
seconds).

Screens:

1. **Home** — two shadcn tables: **Active sessions** and **Stopped / Finished
   sessions**, each with headline stats (name, proxy_display, mode, status
   badge, success rate, median RTT, distinct IPs, last IP + category, last
   sample time). A "New session" button opens the create dialog.
2. **New session** (dialog/form) — proxy connection string, mode (selecting
   sticky/pool sets the probe defaults), cadence, probes/sample, optional caps,
   probe target. Submit starts the session.
3. **Session page** (row click → dedicated route) — details + full report +
   charts (success-rate line, latency p50/p95, composition stacked-area + pool
   donut, stickiness/rotation timeline, pool-growth curve, risk histogram), the
   per-IP reputation table, and the **probes-grouped-into-samples** table
   (paged, most-recent first, each sample row expandable into its N probe
   sub-rows from the arrays). Running sessions auto-refresh; a **Stop** button is
   present while running.

## Project layout

```
proxy-sampler/
  api/openapi.yaml
  cmd/app/main.go
  internal/
    sampler/    supervisor + per-session worker + probe loop
    enrich/     ip-api, proxycheck, greynoise, stopforumspam, dnsbl + classify
    proxydial/  build SOCKS5h/HTTP dialer from a connection string
    crypto/     AES-GCM for proxy creds
    db/         sqlc-generated Postgres  (+ query/*.sql)
    ch/         ClickHouse writer + report queries
    api/        chi handlers (oapi-codegen server) + SPA embed
    config/     env loading
  migrations/postgres/*.sql
  migrations/clickhouse/*.sql
  web/          React + Vite + shadcn/ui → web/dist (go:embed)
  Dockerfile  Makefile  go.mod
```

## Config

| Env | Meaning |
| --- | --- |
| `DATABASE_URL` | Postgres DSN. |
| `CLICKHOUSE_DSN` | clickhouse-go DSN (e.g. `clickhouse://user:pass@host:9000/db`). |
| `ENCRYPTION_KEY` | base64-encoded 32-byte key for AES-GCM proxy-cred encryption. |
| `HTTP_ADDR` | Listen address (default `:8080`). |
| `ENRICH_CONCURRENCY` | Global cap on outbound enrichment (default small; protects quotas). |
| `REPUTATION_TTL` | Re-enrich an IP older than this (default 24h). |
| `PROBE_TARGET_DEFAULT` | Default egress-trace URL (Cloudflare trace). |
| `DB_AUTO_MIGRATE` | Run goose Postgres + ClickHouse migrations at boot (default true). |
| `OTEL_*` | Optional, standard OpenTelemetry configuration. |

## Deployment

One multi-stage Dockerfile: a Node stage builds `web/dist` (Vite); a Go stage
embeds it and builds a static `cmd/app` binary; a slim runtime
(`debian-bookworm-slim` + ca-certificates). Single image, single container,
single replica; requires reachable Postgres + ClickHouse. Migrations are applied
at boot (Postgres advisory-locked) or via
`DB_AUTO_MIGRATE=false` and a separate job. `/healthz` and `/readyz` back the
orchestrator probes.

## Testing

- **enrich**: each provider against `httptest`; DNSBL against a stub
  `net.Resolver`; classification unit-tested against the known cases (mobile /
  residential / datacenter-by-ASN, PBL-is-benign).
- **sampler**: a fake dialer + fake enrichers assert aggregation (mode IP,
  distinct, median RTT, `ip_changed`, `new_ips`, failure-folding into success
  rate) and resume-on-boot.
- **db** / **ch**: integration tests gated on `TEST_DATABASE_URL` /
  `TEST_CLICKHOUSE_DSN` + goose up; ClickHouse covers the batch writer and the
  report queries.
- **crypto**: AES-GCM encrypt/decrypt round-trip.
- **api**: handler tests against the chi router with fakes.
- **web**: a build / render smoke check; full e2e is out of scope for v1.

## Out of scope (v1)

- Multi-replica HA (schema reserved via `claimed_by` / `lease_until`).
- Alerting / notifications (Slack later).
- Commercial reputation APIs (the provider interface is ready).
- Dashboard authentication (tailnet-only).
- Server-sent events (polling in v1).

## Key decisions

- **Standalone repo/module** with an independent lifecycle; it implements its
  own SOCKS5 dialer and ClickHouse batch writer.
- **Ad-hoc proxy connection strings** as the sampling target (matches the
  existing manual provider evals), stored AES-GCM-encrypted at rest.
- **Embedded React/Vite SPA** with shadcn/ui, served from the Go binary.
- **Postgres + ClickHouse**: control plane / IP inventory in Postgres, the
  per-sample time series in ClickHouse.
