# Parameter Variation Runs — Design

**Date:** 2026-08-06
**Status:** Approved (brainstorming complete, pending implementation plan)

## Problem

A session probes one fixed proxy URL. When stickiness works well, the session
observes a single exit IP, so its reputation report describes one IP — not the
provider's pool. There is also no way to systematically vary targeting
parameters (country, ISP, port, session ID) and compare what the provider
actually honors.

## Goal

A **variation run** expands a proxy-URL template across parameter axes into
many child sessions (variants). Fanning out across session IDs / ports pins
many distinct exit IPs, so the aggregated report is statistically
representative of the pool. Varying targeting params (country, ISP, …) shows
per-combination behavior.

## Non-goals

- Per-provider username grammars or a provider registry. The template *is* the
  grammar; a registry can grow out of saved templates later.
- A scheduler / active-variant cap. Variants run like ordinary sessions.
- Traffic-weighted pool statistics. Pool stats dedupe by IP.

## Template, axes, expansion

A template is a normal proxy URL containing `{placeholder}` tokens, e.g.
`socks5h://u-cc-{country}-sid-{session}:pw@gate.example.io:1080` or
`socks5h://user:pass@gate.example.com:{port}`. Placeholders are substituted as
strings anywhere in the URL before parsing. The template is stored encrypted
(AES-GCM, same as session proxy URLs) because it contains credentials.

Axes are a JSON object mapping axis name (= placeholder name) to a spec with
an explicit `kind`:

| Kind | Spec | Semantics |
|---|---|---|
| list | `{"kind": "list", "values": ["de", "us", "fr", "jp"]}` | explicit values, cartesian |
| range | `{"kind": "range", "from": 10000, "to": 10019}` | inclusive integer range, cartesian (no step; use a list for gapped port blocks) |
| random | `{"kind": "random", "count": 5, "length": 8}` | alphanumeric IDs, generated fresh per cell |

**Expansion:** the cartesian product of list and range axes defines the
**cells**. For each cell, every random axis generates `count` fresh values
(fresh per cell — cleaner statistically than reusing IDs across cells). A
variant's full resolved combo is its `variant_params`
(`{"country": "de", "session": "a8x2c9d1"}`); its **cell key** is the combo
minus random axes (`{"country": "de"}`), serialized canonically (sorted keys)
for grouping.

Example: 4 countries × `session {count: 5}` → 4 cells × 5 = 20 variants.

**Validation at create:** every placeholder has a matching axis and vice
versa; `from <= to`; `count >= 1`; every expanded URL passes the same proxy-URL
validation as session creation (error names the offending cell/params); total
variant count ≤ `MAX_VARIANTS_PER_RUN` (env, default **128**, surfaced as
`Config.MaxVariantsPerRun`).

Child sessions get generated names: `<run-name>/country=de #3`.

**New package `internal/variation`:** pure domain — template token parsing,
axis expansion, random ID generation, Chao1 estimator. No DB or HTTP deps.

## Data model (Postgres, one migration)

New table `variation_runs`:

- `id uuid PK`, `name text`, `template_ciphertext bytea`,
  `template_nonce bytea`, `template_display text` (credential-free
  `host:port` with placeholders intact), `axes jsonb` (spec as submitted),
  `created_at timestamptz`.
- **No stored status.** Run status is derived at read time from child
  statuses: `running` if any child is running, else `finished` if all children
  finished via caps, else `stopped`. Cheap GROUP BY; no sync bugs.

`sampling_sessions` gains three nullable columns:

- `run_id uuid REFERENCES variation_runs(id)`
- `variant_params jsonb` — the resolved combo
- `cell_key text` — canonical JSON of params minus random axes, precomputed
  so per-cell grouping is `GROUP BY cell_key`

## Creation flow

Expand + validate → one PG transaction inserts the run row and all child
sessions (each with its own encrypted expanded URL, status `running`) → then
`Supervisor.Start` per child. A crash between commit and start is safe:
`Supervisor.Resume` re-owns all `running` sessions on boot (existing
guarantee).

## API

Extends `api/openapi.yaml`; handlers follow existing server patterns.

| Route | Behavior |
|---|---|
| `POST /api/runs` | name, template, axes, plus standard session knobs applied to every child (mode — default sticky, cadence, probes/sample, probe target, dial timeout, max samples/duration) |
| `GET /api/runs` | list: variant counts, derived status, distinct-IP rollup |
| `GET /api/runs/{id}` | run detail: axes, cells, child summaries |
| `POST /api/runs/{id}/stop` | fan out to running children |
| `POST /api/runs/{id}/reenable` | fan out to stopped/finished children |
| `DELETE /api/runs/{id}` | delete children first (each crossing the CH flush barrier), then the run row |
| `GET /api/runs/{id}/report` | pool rollup + per-cell report (below) |
| `GET /api/runs/{id}/export.csv` | streams the deduped pool IP list with reputation columns (reuses `csv.go` pattern) |

Fan-out ops are per-child and idempotent: children already in the target state
are skipped; on a child failure the response names the child and cause, and a
retry finishes the remainder.

Existing session endpoints keep working on variants unchanged (per-variant
drill-down is free). `GET /api/sessions` excludes sessions with `run_id` set
by default and gains a `?run_id=` filter.

## Run report

### Pool rollup (primary)

Deduped by IP across all children — `session_ips` for the run's sessions
joined with `ip_reputation_cache`:

- distinct exit-IP count; category shares (residential / mobile / hosting /
  …); risk-score histogram; % flagged (ProxyCheck / DNSBL / SFS); top
  ISPs/ASNs; observed country distribution
- **targeting honor rate:** `variant_params` is the requested targeting — no
  username parsing needed. Each variant's dominant observed country/ISP (from
  its samples) is compared to its requested params → match / mismatch /
  unknown per variant → honor rate = % of variants honored, overall and per
  cell. Variants without samples yet report `unknown`.
- success-rate and RTT time series aggregated over children from ClickHouse
  (`session_id IN (...)`, reusing existing query shapes)
- **estimated pool size (Chao1):** from IP frequency across variants,
  `Ŝ = S_obs + f1²/(2·f2)` (f1 = IPs seen by exactly one variant, f2 = by
  exactly two). When `f2 = 0` the estimate is undefined → fall back to
  `S_obs`, explicitly flagged as a lower bound.

**Dedup rule:** pool stats count each IP once regardless of how many
variants/samples hit it — representative-of-pool, not traffic-weighted.

### Per-cell breakdown (secondary)

Grouped by `cell_key`: variant count, distinct IPs, honor rate, category
shares, median risk, success rate, median RTT. Sortable table so a misbehaving
combo stands out.

Implementation: one PG aggregate query (sessions → session_ips → reputation,
grouped by run and by cell) + a CH series query; new `RunReport` schema in the
OpenAPI spec; Chao1 as a pure function in `internal/variation`.

## UI

- **Home page:** tabbed header — Runs list (name, template display, variant
  count, derived status, distinct IPs, created) alongside the session table,
  which now shows only standalone sessions.
- **New-run dialog:** name, template URL, **axes builder** (rows of
  `key | type (list / range / random) | values`), live variant-count preview
  (`4 × 5 = 20 variants`) that turns red past the cap before submit; standard
  session knobs in a collapsed advanced section. The preview-count logic is a
  pure TS function.
- **Run detail page (`/runs/:id`):** status badge; Stop / Re-enable / Delete;
  stat strip (distinct IPs, estimated pool size, honor rate, % flagged,
  success rate); pool charts reusing existing components (`RiskHistogram`,
  category composition, success/RTT series); per-cell table; variants table
  linking to each child's session page. Child session pages get a
  "part of run ⟨name⟩" backlink badge.
- **API client:** `useRuns`, `useRun`, `useRunReport`, `useCreateRun`,
  `useStopRun`, `useReenableRun`, `useDeleteRun` in `web/src/lib/api.ts`,
  existing TanStack Query patterns.

## Error handling

- Validation failures are 400s naming the exact problem (unknown placeholder,
  unused axis, `from > to`, `count < 1`, cap exceeded with the computed
  count, unparseable expanded URL with its cell/params).
- Creation is all-or-nothing (single PG transaction).
- Fan-out partial failures: named child + cause; idempotent retry.
- Sparse runs: honor rate `unknown` without samples; Chao1 falls back to
  `S_obs` when `f2 = 0` (no division by zero).
- Enrichment load: already governed by the global `ENRICH_CONCURRENCY`
  semaphore; the reputation cache dedups by IP across variants. No changes
  needed.

## Testing

- `internal/variation`: pure unit tests — token parsing, expansion counts,
  fresh-per-cell randomness, range inclusivity, cap, Chao1 fixtures including
  the `f2 = 0` fallback.
- `internal/db`: run-create transaction, report/cell-grouping queries,
  following existing sqlc store test conventions.
- API + supervisor fan-out: existing fake-store / fake-worker patterns from
  the re-enable work.
- Web: variant-count preview tested as a pure function; otherwise mirror the
  existing web test setup.

## Config additions

| Env | Field | Default |
|---|---|---|
| `MAX_VARIANTS_PER_RUN` | `Config.MaxVariantsPerRun` | 128 |
