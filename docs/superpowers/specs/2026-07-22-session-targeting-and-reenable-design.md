# proxy-sampler — Session Targeting Attributes & Re-enable Design

2026-07-22 · adds two capabilities to the existing `github.com/timo972/proxy-sampler` repo: (1) surfacing the proxy username and its encoded targeting attributes on the session detail page, compared against what the session actually observed; and (2) re-enabling a stopped or finished session.

## Problem

Two gaps on the session detail page:

1. **No visibility into what was requested.** A session's proxy is stored encrypted and the API exposes only `host:port` (`proxydial.Display`). Many providers encode targeting attributes — country, region, city, session/rotation type, network zone, ISP — in the proxy *username*. Operators cannot currently see which attributes a session requested, nor whether the proxy actually delivered them (e.g. "asked for US residential, got 95% US / 60% residential").
2. **Sessions are terminal once stopped.** A `stopped` (manual) or `finished` (cap reached) session cannot be run again; the UI states outright that "Sampling cannot be resumed." Operators want to re-enable a prior session and sample it again without recreating it, keeping the historical samples already gathered.

## Goals

- Show the proxy username (password redacted) and its parsed targeting attributes on the detail page, grouped into readable categories.
- Compare requested attributes against observed results using data the report already produces.
- Let an operator re-enable a `stopped` or `finished` session so it runs again, preserving prior ClickHouse samples and IP inventory while resetting the visible per-run progress counters.

## Non-goals

- Extending the report/ClickHouse schema to add new observed dimensions (city/region/state have no observed counterpart today; they render requested-only rather than triggering a schema change).
- Exposing the proxy password anywhere. Only the username is surfaced.
- A provider-specific username grammar. Parsing is a tolerant, best-effort heuristic.
- Re-enable on the sessions list page or bulk re-enable (detail page only for now).
- Archiving prior runs as distinct entities, or per-run report filtering. All runs of a re-enabled session share one cumulative report.

## Resolved decisions

- **Expose the username.** The detail endpoint returns the proxy username (password stripped). This is a deliberate softening of the current "encrypt everything, show only host:port" posture, acceptable for a trusted-network tool. The username is returned only by the detail endpoint, never by the list endpoint. The password remains encrypted and is never returned.
- **`distinct_ips` on re-enable stays tied to the retained IP inventory** rather than hard-resetting to zero. The inventory is kept (so reports and IP-novelty detection remain meaningful), and `distinct_ips` therefore continues from the retained pool. This is the one counter that cannot both "reset" and "keep prior data" coherently.

## Feature 1 — Targeting attributes & requested-vs-observed comparison

### Backend

- **Schema:** add an optional `proxy_username` string to the `Session` response schema (`api/openapi.yaml`), regenerated into `internal/api/openapi`.
- **Population:** `SessionByID` decrypts the stored proxy ciphertext with the server's existing `cipher`, parses the URL, and sets `proxy_username = url.User.Username()` (empty → omit). `ListSessions` and `CreateSession` responses leave it unset. Decrypt failure is treated as a `500` exactly as elsewhere; a proxy with no userinfo yields an absent field, not an error.
- **No password path:** only `Username()` is read; `Password()` is never returned or logged.

### Frontend

A new **Targeting** section rendered on the Overview tab of the session detail page (`web/src/pages/session-page.tsx`), backed by a new component under `web/src/components/report/` and a pure parser/comparison module under `web/src/lib/`.

- **Parser (`web/src/lib/proxy-attributes.ts`, pure + unit-tested):** given the username string, tokenize on `-`, `_`, and `:`; fold adjacent `key value` pairs where the key is recognized. Recognized keys (case-insensitive, with common aliases) map to a friendly label and a category:
  - **Geo:** `country`/`cc`, `region`, `state`/`st`, `city`
  - **Session:** `session`/`sess`/`sid`, `sticky`, `rotate`/`rotating`, `sesstime`/`ttl`
  - **Network:** `zone`/`type` (values like `resi`/`residential`, `mobile`/`mob`, `dc`/`datacenter`), `isp`, `asn`
  - Unrecognized tokens are preserved verbatim as "raw" chips so nothing is silently dropped.
  - The parser returns a structured list of `{category, label, key, value, raw?}` plus the leftover raw tokens. It never throws on malformed input.
- **Display:** recognized attributes render as labeled chips grouped by category (Geo / Session / Network); raw tokens render in a separate "Unparsed" group. If `proxy_username` is absent, the section shows a short "No username on this proxy" note.
- **Requested vs Observed table:** for each recognized attribute that has an observable counterpart in the current `SessionReport`, show requested value, observed value, and a match indicator (`match` / `partial` / `mismatch`). Mapping:
  - country → most-common `report.ips[].country` (share shown, e.g. "US 95%")
  - zone/type → `report.pool_composition` (dominant network class + its share)
  - isp → most-common `report.ips[].isp`
  - asn → most-common `report.ips[].asn`
  - sticky/rotate/session → `report.stickiness` (average hold seconds, rotation count) as observed context; match indicator only when the requested intent (sticky vs rotating) can be compared against observed rotation behavior
  - Attributes with no observable counterpart today (region, state, city) render requested-only with an "—" observed cell.
  - The comparison is a pure function (`web/src/lib/proxy-attributes.ts` or a sibling), unit-tested against representative report shapes.
- **Data dependency:** the section needs both the session detail (username) and the report (observed). It renders within the existing report boundary; while the report is loading it shows requested-only, filling in observed values when the report resolves. For a stopped/finished session the report is still available and used.

## Feature 2 — Re-enable a stopped or finished session

### Sequence/counter decoupling

Today `Snapshot.SamplesTaken` is simultaneously the ClickHouse `sample_seq`, the value the `max_samples` cap is checked against, and the header progress counter (`worker.go`). To keep prior samples and continue the sequence while resetting the visible counters, introduce a persisted offset:

- **Model:** add `SequenceOffset int` to `session.Session` (a session-level attribute, not a per-tick snapshot value), persisted as a new Postgres column `sequence_offset INTEGER NOT NULL DEFAULT 0`.
- **Worker:** the ClickHouse sequence written for each event becomes `SequenceOffset + runCount`, where `runCount` starts from `Snapshot.SamplesTaken` (per-run count). The `max_samples` cap is checked against `runCount` (per run), and `max_duration` against `StartedAt` (reset on re-enable). `Snapshot.SamplesTaken` continues to track the per-run count. Existing behavior is unchanged for sessions that were never re-enabled (`SequenceOffset == 0`), including boot resume.

### Lifecycle

- **Store:** add `Reenable(ctx, id, at)` to `session.Store`, implemented in both `internal/db/store.go` (sqlc query + migration) and the in-memory store used by tests. It:
  - loads the session; errors `ErrNotFound` if missing;
  - is a no-op-error (`ErrAlreadyRunning`, a new sentinel) if the session is already `running`;
  - sets `sequence_offset = sequence_offset + samples_taken`;
  - zeroes the per-run scalars: `samples_taken`, `probes_ok`, `probes_total`, and clears `last_sample_at`, `last_primary_ip`, `last_category`, `last_rtt`, `last_error`;
  - sets `started_at = at`, `stopped_at = NULL`, `status = 'running'`;
  - leaves ClickHouse sample_events and the Postgres IP inventory untouched (`distinct_ips` is recomputed by the worker from the retained inventory).
- **Control/supervisor:** add `Reenable(ctx, id)` to the `Control` interface and `Supervisor`. It calls `store.Reenable` then `Start(id)` (which is idempotent and verifies the row is `running`). Concurrency uses the same per-session operation guard as `Start`/`Stop`.
- **API:** add `POST /api/sessions/{id}/reenable` to `api/openapi.yaml` (regenerated). Handler maps: success → `200` with the updated session (or `204`, matching the stop convention — see below); `ErrNotFound` → `404`; `ErrAlreadyRunning` → the existing `sessionNotRunning`-style `409`/`400` conflict response. To let the UI update immediately, the handler returns `200` with the refreshed `Session` body.

### Accepted divergences (documented, per the chosen behavior)

- **Counters vs history.** The header/summary reflects the current run (reset counters); the report and samples tabs are ClickHouse-backed and show all runs cumulatively. Sequence numbers continue across runs, so there are no collisions and prior samples remain visible.
- **`distinct_ips`.** Tied to the retained IP inventory (kept), so it continues from the prior pool rather than resetting.

### Frontend

- **Types/query (`web/src/lib/api.ts`):** add `proxy_username?: string | null` to `Session`; add `useReenableSession` mutation calling `POST /api/sessions/{id}/reenable` and invalidating `['sessions']`, `['sessions', id]`, `['session-report', id]`, `['session-samples', id]`.
- **UI (`session-page.tsx`):** show a **Re-enable** button in the header when `status !== 'running'` (i.e. `stopped` or `finished`), alongside Export CSV. Clicking opens a confirm dialog: "Re-enable “{name}”? Sampling resumes; progress counters reset while prior samples are kept." On success the session query refetches and the header flips to the running state (Stop button, live polling). Error surfaces via the existing inline alert pattern. The Stop confirmation copy is updated to reflect that a stopped session can now be re-enabled.

## Testing / verification

- **Go unit:** parser is frontend-only; backend tests cover `store.Reenable` (offset folding, counter reset, status/timestamps, already-running and not-found errors) in the in-memory store; worker tests cover sequence continuity across a simulated re-enable (offset applied, `max_samples` re-evaluated against per-run count, `max_duration` from new `started_at`); server tests cover the reenable endpoint status mapping and `proxy_username` population on detail (and its absence on list).
- **Go integration:** `internal/db` and `internal/chmigrate`/`internal/migrate` suites exercise the new migration and `Reenable` query against real Postgres when `TEST_DATABASE_URL` is set (gated as today).
- **Frontend unit (vitest):** `proxy-attributes` parser (recognized keys, aliases, raw passthrough, empty/malformed input); comparison mapping against representative report fixtures; `session-page` renders the Targeting section and the Re-enable button per status, and the confirm/mutation flow.
- **Generation gate:** `make generate` (sqlc + oapi-codegen) leaves a clean tree; `make test` (Go + web) and `make test-race` pass.
- **Manual (compose stack):** create a session with an attribute-rich username, stop it, re-enable it, confirm the sample sequence continues in the samples tab while the header counters restart, and confirm the Targeting comparison renders.
