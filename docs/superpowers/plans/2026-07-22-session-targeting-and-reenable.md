# Session Targeting Attributes & Re-enable Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Surface the proxy username and its parsed targeting attributes (compared against observed egress) on the session detail page, and let an operator re-enable a stopped or finished session with reset per-run counters while keeping prior samples.

**Architecture:** Re-enable decouples the per-run counter from the global ClickHouse sequence via a new persisted `sequence_offset`: the worker writes `sequence_offset + runCount` to ClickHouse (no collisions, prior samples kept) while `samples_taken` and the `max_samples` cap track the current run only. A new `POST /api/sessions/{id}/reenable` folds the prior run into the offset, zeroes per-run scalars, and restarts the worker. Targeting exposes the proxy username on the detail endpoint only (decrypted at read time, password never returned); a pure frontend module parses attributes and compares them to the existing report.

**Tech Stack:** Go 1.26 (pgx/v5, sqlc, oapi-codegen strict server, goose migrations), ClickHouse, React 19 + TypeScript + Vite + vitest + @testing-library/react.

## Global Constraints

- Go 1.26; Node 24. Follow existing patterns; keep files focused.
- Regenerated code must be deterministic: `make generate` (sqlc + oapi-codegen) must leave a clean tree (`git diff --exit-code`).
- Never return, log, or persist the proxy password. Only `url.User.Username()` may be surfaced, and only on the detail endpoint (`SessionByID`) — never in `ListSessions`.
- The service assumes a single replica; do not add multi-replica behavior.
- Postgres migrations are goose-format under `migrations/postgres/`; ClickHouse under `migrations/clickhouse/`. Migrations are embedded and applied on startup when `DB_AUTO_MIGRATE=true`.
- Commit after each task. Use `make test` (Go + web) and `make generate-check` from a clean tree before finishing.

---

## File Structure

**Backend (Go):**
- `migrations/postgres/20260722120000_session_sequence_offset.sql` — new migration adding `sequence_offset`.
- `internal/session/session.go` — add `SequenceOffset` field.
- `internal/session/store.go` — add `ErrAlreadyRunning`, add `Reenable` to `Store`.
- `internal/db/query/sessions.sql` — thread `sequence_offset`; add `ReenableSession`.
- `internal/db/store.go` — thread `sequence_offset`; implement `Reenable`.
- `internal/sampler/worker.go` — compute ClickHouse sequence as `sequence_offset + runCount`.
- `internal/sampler/supervisor.go` — add `Reenable`.
- `internal/api/server.go` — add `ReenableSession` handler; populate `proxy_username` in `SessionByID`; add `Reenable` to `Control`.
- `api/openapi.yaml` — add `/api/sessions/{id}/reenable`; add `proxy_username` to `Session`.

**Frontend (web):**
- `web/src/lib/proxy-attributes.ts` — pure parser + comparison.
- `web/src/lib/proxy-attributes.test.ts` — unit tests.
- `web/src/components/report/targeting-panel.tsx` — Targeting UI.
- `web/src/components/report/targeting-panel.test.tsx` — component test.
- `web/src/lib/api.ts` — add `proxy_username`; add `useReenableSession`.
- `web/src/pages/session-page.tsx` — render Targeting panel; add Re-enable button.

**Tests touched for interface changes:** `internal/api/server_test.go` (`memoryStore`, `fakeControl`), `internal/sampler/worker_test.go` (`fakeSessionStore`), `internal/sampler/supervisor_test.go`, `internal/db/store_integration_test.go`.

---

## Task 1: Persist `sequence_offset` (schema + model wiring, no behavior)

**Files:**
- Create: `migrations/postgres/20260722120000_session_sequence_offset.sql`
- Modify: `internal/session/session.go:27-45`
- Modify: `internal/db/query/sessions.sql:1-55`
- Modify: `internal/db/store.go:218-322`
- Regenerate: `internal/db/*.sql.go` (via `make sqlc-generate`)

**Interfaces:**
- Produces: `session.Session.SequenceOffset int`; DB column `sampling_sessions.sequence_offset integer NOT NULL DEFAULT 0`; sqlc `InsertSessionParams.SequenceOffset int32`, `SessionByIDRow.SequenceOffset int32` (and `SessionsRow`, `RunningSessionsRow`).

- [ ] **Step 1: Write the migration**

Create `migrations/postgres/20260722120000_session_sequence_offset.sql`:

```sql
-- +goose Up
ALTER TABLE sampling_sessions ADD COLUMN sequence_offset integer NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sampling_sessions DROP COLUMN sequence_offset;
```

- [ ] **Step 2: Add the model field**

In `internal/session/session.go`, add to the `Session` struct after `Status Status`:

```go
	Status          Status
	SequenceOffset  int
```

- [ ] **Step 3: Thread the column through the SQL queries**

In `internal/db/query/sessions.sql`:

In `InsertSession`, add `sequence_offset` to the column list (after `stopped_at`) and `sqlc.arg(sequence_offset)` to the VALUES list (after `sqlc.narg(stopped_at)`).

In `Sessions`, `SessionByID`, and `RunningSessions`, add `s.sequence_offset` to each SELECT column list (after `s.stopped_at`).

- [ ] **Step 4: Regenerate sqlc and verify it fails to compile until the mapper is updated**

Run: `make sqlc-generate`
Expected: regenerates `internal/db/sessions.sql.go` with `SequenceOffset int32` on the params/row structs.

Run: `go build ./...`
Expected: FAIL — `insertSessionParams` and the `sessionRow` mappers don't set the new field yet.

- [ ] **Step 5: Update the db mapping**

In `internal/db/store.go`:

Add to `sessionRow` (after `StoppedAt`):
```go
	StoppedAt          pgtype.Timestamptz
	SequenceOffset     int32
```

In `mapSession`, add to the returned `session.Session` literal (after `StartedAt/StoppedAt` line, before `Snapshot:`):
```go
		Status: session.Status(row.Status), CreatedAt: row.CreatedAt.Time,
		StartedAt: timePtr(row.StartedAt), StoppedAt: timePtr(row.StoppedAt),
		SequenceOffset: int(row.SequenceOffset),
```

In `mapSessionByIDRow`, `mapSessionsRow`, and `mapRunningSessionsRow`, add to each `sessionRow{...}` literal (after `StoppedAt: row.StoppedAt,`):
```go
			CreatedAt: row.CreatedAt, StartedAt: row.StartedAt, StoppedAt: row.StoppedAt,
			SequenceOffset: row.SequenceOffset,
```

In `insertSessionParams`, add (after `StoppedAt: timestampPtr(value.StoppedAt),`):
```go
			StartedAt: timestampPtr(value.StartedAt), StoppedAt: timestampPtr(value.StoppedAt),
			SequenceOffset: int32(value.SequenceOffset),
```

- [ ] **Step 6: Verify build and existing tests pass**

Run: `make sqlc-generate && go build ./... && go test ./...`
Expected: PASS (new column defaults to 0; behavior unchanged).

- [ ] **Step 7: Verify generation is clean**

Run: `make generate && git diff --exit-code`
Expected: no diff.

- [ ] **Step 8: Commit**

```bash
git add migrations/postgres/20260722120000_session_sequence_offset.sql internal/session/session.go internal/db
git commit -m "feat: persist per-session sequence_offset"
```

---

## Task 2: `Reenable` store method + query + sentinel error

**Files:**
- Modify: `internal/db/query/sessions.sql` (add `ReenableSession`)
- Modify: `internal/session/store.go:12-29`
- Modify: `internal/db/store.go:83-110`
- Modify: `internal/api/server_test.go:527-...` (`memoryStore.Reenable`)
- Modify: `internal/sampler/worker_test.go:375-...` (`fakeSessionStore.Reenable`)
- Test: `internal/db/store_integration_test.go` (add `TestReenableFoldsOffsetAndResetsCounters`)

**Interfaces:**
- Consumes: `session.Session.SequenceOffset` (Task 1).
- Produces: `session.ErrAlreadyRunning error`; `session.Store.Reenable(ctx context.Context, id uuid.UUID, at time.Time) error`; sqlc `ReenableSession`/`ReenableSessionParams{ID uuid.UUID; StartedAt pgtype.Timestamptz}`.

- [ ] **Step 1: Write the failing integration test**

Add to `internal/db/store_integration_test.go`:

```go
func TestReenableFoldsOffsetAndResetsCounters(t *testing.T) {
	store := testStore(t)
	s := insertTestSession(t, store)
	// Simulate a prior run: 5 samples, some probes, one IP.
	snap := session.Snapshot{SamplesTaken: 5, ProbesOK: 12, ProbesTotal: 15, DistinctIPs: 1}
	if err := store.SaveTick(context.Background(), s.ID, snap, []session.IPHit{{IP: netip.MustParseAddr("203.0.113.7"), SeenAt: s.CreatedAt, Hits: 3}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Stop(context.Background(), s.ID, s.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	reenabledAt := s.CreatedAt.Add(2 * time.Minute)
	if err := store.Reenable(context.Background(), s.ID, reenabledAt); err != nil {
		t.Fatalf("reenable: %v", err)
	}

	got, err := store.SessionByID(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != session.StatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
	if got.SequenceOffset != 5 {
		t.Errorf("sequence_offset = %d, want 5", got.SequenceOffset)
	}
	if got.Snapshot.SamplesTaken != 0 || got.Snapshot.ProbesOK != 0 || got.Snapshot.ProbesTotal != 0 {
		t.Errorf("counters not reset: %+v", got.Snapshot)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(reenabledAt) || got.StoppedAt != nil {
		t.Errorf("timestamps: started=%v stopped=%v", got.StartedAt, got.StoppedAt)
	}
	// IP inventory is kept.
	ips, err := store.SessionIPs(context.Background(), s.ID)
	if err != nil || len(ips) != 1 {
		t.Fatalf("session ips = %d (err %v), want 1 kept", len(ips), err)
	}
	// Re-enabling a running session is rejected.
	if err := store.Reenable(context.Background(), s.ID, reenabledAt); !errors.Is(err, session.ErrAlreadyRunning) {
		t.Fatalf("second reenable error = %v, want ErrAlreadyRunning", err)
	}
	// Unknown session is not found.
	if err := store.Reenable(context.Background(), uuid.New(), reenabledAt); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("unknown reenable error = %v, want ErrNotFound", err)
	}
}
```

Add `"net/netip"` to the test file imports if not already present.

- [ ] **Step 2: Run the test to verify it fails**

Run: `TEST_DATABASE_URL='postgres://proxy_sampler:proxy_sampler@localhost:5432/proxy_sampler_test?sslmode=disable' go test ./internal/db -run TestReenable -v`
Expected: FAIL to compile — `store.Reenable` and `session.ErrAlreadyRunning` undefined. (With no `TEST_DATABASE_URL`, the test skips; you still get the compile error.)

- [ ] **Step 3: Add the sqlc query**

Append to `internal/db/query/sessions.sql`:

```sql
-- name: ReenableSession :execrows
UPDATE sampling_sessions
SET status = 'running',
    sequence_offset = sequence_offset + samples_taken,
    samples_taken = 0,
    probes_ok = 0,
    probes_total = 0,
    last_sample_at = NULL,
    last_primary_ip = NULL,
    last_rtt_ms = NULL,
    last_error = NULL,
    started_at = sqlc.arg(started_at),
    stopped_at = NULL
WHERE id = sqlc.arg(id) AND status IN ('stopped', 'finished');
```

Note: `distinct_ips` is intentionally NOT reset (the IP inventory is kept). `last_category` is join-derived, not a column.

- [ ] **Step 4: Add the sentinel error and interface method**

In `internal/session/store.go`, extend the error block and interface:

```go
var (
	ErrNotFound       = errors.New("session not found")
	ErrNotRunning     = errors.New("session is not running")
	ErrAlreadyRunning = errors.New("session is already running")
)
```

Add to the `Store` interface (after `Finish`):
```go
	Finish(ctx context.Context, id uuid.UUID, at time.Time) error
	Reenable(ctx context.Context, id uuid.UUID, at time.Time) error
```

- [ ] **Step 5: Regenerate sqlc and implement the store method**

Run: `make sqlc-generate`

In `internal/db/store.go`, add after `Finish`:

```go
func (s *Store) Reenable(ctx context.Context, id uuid.UUID, at time.Time) error {
	rows, err := s.q.ReenableSession(ctx, ReenableSessionParams{ID: id, StartedAt: timestamp(at)})
	if err != nil {
		return fmt.Errorf("reenable session: %w", err)
	}
	if rows == 0 {
		// No row transitioned: either it does not exist or it is already running.
		if _, err := s.SessionByID(ctx, id); err != nil {
			return err // ErrNotFound or a real error
		}
		return session.ErrAlreadyRunning
	}
	return nil
}
```

- [ ] **Step 6: Add `Reenable` to the test fakes so the interface is satisfied**

In `internal/api/server_test.go`, add a field to `memoryStore` (`reenableErr error`) and a method:

```go
func (s *memoryStore) Reenable(_ context.Context, id uuid.UUID, at time.Time) error {
	if s.reenableErr != nil {
		return s.reenableErr
	}
	for i := range s.sessions {
		if s.sessions[i].ID == id {
			if s.sessions[i].Status == session.StatusRunning {
				return session.ErrAlreadyRunning
			}
			s.sessions[i].SequenceOffset += s.sessions[i].Snapshot.SamplesTaken
			s.sessions[i].Snapshot.SamplesTaken = 0
			s.sessions[i].Snapshot.ProbesOK = 0
			s.sessions[i].Snapshot.ProbesTotal = 0
			started := at
			s.sessions[i].Status = session.StatusRunning
			s.sessions[i].StartedAt = &started
			s.sessions[i].StoppedAt = nil
			return nil
		}
	}
	return session.ErrNotFound
}
```

In `internal/sampler/worker_test.go`, add to `fakeSessionStore`:

```go
func (s *fakeSessionStore) Reenable(context.Context, uuid.UUID, time.Time) error { return nil }
```

- [ ] **Step 7: Run tests**

Run: `go build ./... && go test ./internal/db ./internal/api ./internal/sampler`
Expected: PASS (integration test skips without `TEST_DATABASE_URL`). With the compose stack up and `TEST_DATABASE_URL` set, run the integration test and expect PASS.

- [ ] **Step 8: Verify generation clean and commit**

```bash
make generate && git diff --exit-code
git add internal/db internal/session/store.go internal/api/server_test.go internal/sampler/worker_test.go
git commit -m "feat: add store.Reenable folding offset and resetting counters"
```

---

## Task 3: Worker writes the global sequence (`offset + runCount`)

**Files:**
- Modify: `internal/sampler/worker.go:214-246`
- Test: `internal/sampler/worker_test.go` (add `TestPreparedWorkerTickAppliesSequenceOffset`)

**Interfaces:**
- Consumes: `session.Session.SequenceOffset` (Task 1).
- Produces: ClickHouse `ch.Event.SampleSeq == SequenceOffset + perRunSequence`; `Snapshot.SamplesTaken` remains the per-run count; `max_samples` still checked against the per-run count.

- [ ] **Step 1: Write the failing test**

Add to `internal/sampler/worker_test.go` (mirrors `TestPreparedWorkerTickPersistsBeforeEnqueueAndLimitsOrderedProbes`):

```go
func TestPreparedWorkerTickAppliesSequenceOffset(t *testing.T) {
	store := newFakeSessionStore()
	value, cipher := encryptedSession(t, 1)
	value.SequenceOffset = 100
	value.Snapshot = session.Snapshot{SamplesTaken: 12, ProbesOK: 20, ProbesTotal: 24, DistinctIPs: 1, LastPrimaryIP: netip.MustParseAddr("192.0.2.1")}
	store.sessions[value.ID] = value
	store.ips[value.ID] = []session.IPRecord{{IP: value.Snapshot.LastPrimaryIP}}

	sink := &fakeEventSink{}
	worker := testWorker(store, cipher, fixedProber{result: ProbeResult{IP: netip.MustParseAddr("192.0.2.2")}}, &fakeLookup{}, sink)
	prepared, err := worker.Prepare(context.Background(), value)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := prepared.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want 1", len(sink.events))
	}
	// Per-run sequence is 12+1=13; ClickHouse sequence continues at 100+13=113.
	if sink.events[0].SampleSeq != 113 {
		t.Errorf("event SampleSeq = %d, want 113", sink.events[0].SampleSeq)
	}
	if store.saved[0].snapshot.SamplesTaken != 13 {
		t.Errorf("SamplesTaken = %d, want 13 (per-run)", store.saved[0].snapshot.SamplesTaken)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/sampler -run TestPreparedWorkerTickAppliesSequenceOffset -v`
Expected: FAIL — `event SampleSeq = 13, want 113` (offset not yet applied).

- [ ] **Step 3: Apply the offset in `tick`**

In `internal/sampler/worker.go`, in `tick`, change the sequence/event lines. Replace:

```go
	nextSequence := p.sampleSeq + 1
```
with:
```go
	nextSequence := p.sampleSeq + 1
	clickhouseSequence := uint32(p.session.SequenceOffset) + nextSequence
```

Replace:
```go
	event := sampleEvent(p.session.ID, sampledAt, nextSequence, sample, reputations[sample.PrimaryIP])
```
with:
```go
	event := sampleEvent(p.session.ID, sampledAt, clickhouseSequence, sample, reputations[sample.PrimaryIP])
```

Replace the drop-log line's `"sample_seq", nextSequence` with `"sample_seq", clickhouseSequence`.

Leave `nextSnapshot.SamplesTaken = int(nextSequence)` and `maxSamplesReached` (which uses `p.sampleSeq`) unchanged — those stay per-run.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/sampler -run TestPreparedWorkerTickAppliesSequenceOffset -v`
Expected: PASS.

- [ ] **Step 5: Run the full sampler suite (guards against regressions in existing seq assertions)**

Run: `go test ./internal/sampler`
Expected: PASS (existing tests use `SequenceOffset == 0`, so `SampleSeq` is unchanged for them).

- [ ] **Step 6: Commit**

```bash
git add internal/sampler/worker.go internal/sampler/worker_test.go
git commit -m "feat: worker writes global sequence as offset + per-run count"
```

---

## Task 4: `Reenable` on the supervisor / Control

**Files:**
- Modify: `internal/sampler/supervisor.go` (add `Reenable`)
- Test: `internal/sampler/supervisor_test.go` (add `TestSupervisorReenableResetsAndStarts`)

**Interfaces:**
- Consumes: `session.Store.Reenable` (Task 2), `Supervisor.Start` (existing).
- Produces: `(*Supervisor).Reenable(ctx context.Context, id uuid.UUID) error` — calls `store.Reenable` then `Start`; propagates `session.ErrNotFound`/`session.ErrAlreadyRunning`.

- [ ] **Step 1: Write the failing test**

Add to `internal/sampler/supervisor_test.go` (use the file's existing supervisor test helpers/fakes; the store fake must expose a way to preset a stopped session and record the `Reenable`/`Start` calls). Concretely:

```go
func TestSupervisorReenableResetsAndStarts(t *testing.T) {
	sup, store := newTestSupervisor(t) // existing helper in this test file
	value := stoppedTestSession()      // a session with Status = stopped, SamplesTaken > 0
	store.sessions[value.ID] = value

	if err := sup.Reenable(context.Background(), value.ID); err != nil {
		t.Fatalf("Reenable: %v", err)
	}
	if !store.reenabled[value.ID] {
		t.Error("store.Reenable was not called")
	}
	got := store.sessions[value.ID]
	if got.Status != session.StatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
}
```

If `supervisor_test.go` lacks `newTestSupervisor`/`store.reenabled`/`stoppedTestSession`, add minimal equivalents next to the existing fakes in that file (the store fake's `Reenable` should set `status=running`, fold offset, zero counters, and record `reenabled[id]=true`). Keep it consistent with the existing supervisor test store.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/sampler -run TestSupervisorReenableResetsAndStarts -v`
Expected: FAIL — `sup.Reenable` undefined.

- [ ] **Step 3: Implement `Reenable` on the supervisor**

In `internal/sampler/supervisor.go`, add after `Start`:

```go
// Reenable resets a stopped or finished session's per-run counters and starts
// a fresh worker. Prior samples and IP inventory are preserved.
func (s *Supervisor) Reenable(ctx context.Context, id uuid.UUID) error {
	if err := s.root.Err(); err != nil {
		return err
	}
	if err := s.store.Reenable(ctx, id, s.now().UTC()); err != nil {
		return err
	}
	return s.Start(ctx, id)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/sampler -run TestSupervisorReenable -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sampler/supervisor.go internal/sampler/supervisor_test.go
git commit -m "feat: supervisor Reenable resets counters and restarts worker"
```

---

## Task 5: `POST /api/sessions/{id}/reenable` endpoint

**Files:**
- Modify: `api/openapi.yaml` (add path)
- Regenerate: `internal/api/openapi/openapi.gen.go` (via `make openapi-generate`)
- Modify: `internal/api/server.go:33-37` (Control interface), add `ReenableSession` handler
- Test: `internal/api/server_test.go` (add reenable tests)

**Interfaces:**
- Consumes: `Supervisor.Reenable` (Task 4), `store.SessionByID`.
- Produces: `Control.Reenable(context.Context, uuid.UUID) error`; `Server.ReenableSession(ctx, openapi.ReenableSessionRequestObject) (openapi.ReenableSessionResponseObject, error)` returning `200` with the refreshed `Session`.

- [ ] **Step 1: Add the OpenAPI path**

In `api/openapi.yaml`, after the `/api/sessions/{id}/stop` block, add:

```yaml
  /api/sessions/{id}/reenable:
    parameters:
      - $ref: '#/components/parameters/SessionID'
    post:
      operationId: reenableSession
      responses:
        '200':
          description: Session re-enabled and running
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Session'
        '400': {$ref: '#/components/responses/BadRequest'}
        '404': {$ref: '#/components/responses/NotFound'}
        '409': {$ref: '#/components/responses/Conflict'}
        '500': {$ref: '#/components/responses/InternalError'}
        '503': {$ref: '#/components/responses/DependencyUnavailable'}
```

- [ ] **Step 2: Regenerate the server interface**

Run: `make openapi-generate`
Expected: `internal/api/openapi/openapi.gen.go` gains `ReenableSession` on `StrictServerInterface`, plus `ReenableSessionRequestObject` and `ReenableSession200JSONResponse`.

Run: `go build ./...`
Expected: FAIL — `*Server` no longer satisfies the interface (`ReenableSession` missing).

- [ ] **Step 3: Write the failing handler tests**

Add to `internal/api/server_test.go`:

```go
func TestReenableSessionRestartsStoppedSession(t *testing.T) {
	store := newMemoryStore()
	s := sampleSession()
	s.Status = session.StatusStopped
	s.Snapshot.SamplesTaken = 5
	store.sessions = append(store.sessions, s)
	// The fake control stands in for the supervisor: its Reenable performs the
	// real store transition so the handler's refreshed read reflects it.
	control := &fakeControl{reenable: func(ctx context.Context, id uuid.UUID) error {
		return store.Reenable(ctx, id, time.Now().UTC())
	}}
	handler := testHandler(t, store, control)

	response := request(t, handler, http.MethodPost, "/api/sessions/"+s.ID.String()+"/reenable", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if len(control.reenabled) != 1 || control.reenabled[0] != s.ID {
		t.Fatalf("reenabled = %v, want [%s]", control.reenabled, s.ID)
	}
	var body struct {
		Status       string `json:"status"`
		SamplesTaken int    `json:"samples_taken"`
	}
	decodeJSON(t, response, &body)
	if body.Status != "running" || body.SamplesTaken != 0 {
		t.Errorf("body = %+v, want running with samples_taken 0", body)
	}
}

func TestReenableSessionConflictWhenAlreadyRunning(t *testing.T) {
	store := newMemoryStore()
	control := &fakeControl{reenableErr: session.ErrAlreadyRunning}
	handler := testHandler(t, store, control)
	response := request(t, handler, http.MethodPost, "/api/sessions/"+uuid.New().String()+"/reenable", "")
	assertAPIError(t, response, http.StatusConflict, "session_not_running")
}

func TestReenableSessionNotFound(t *testing.T) {
	store := newMemoryStore()
	control := &fakeControl{reenableErr: session.ErrNotFound}
	handler := testHandler(t, store, control)
	response := request(t, handler, http.MethodPost, "/api/sessions/"+uuid.New().String()+"/reenable", "")
	assertAPIError(t, response, http.StatusNotFound, "not_found")
}
```

Extend `fakeControl` with `reenableErr error`, `reenabled []uuid.UUID`, a `reenable func(context.Context, uuid.UUID) error` hook (mirroring the existing `start` hook), and a method:

```go
func (f *fakeControl) Reenable(ctx context.Context, id uuid.UUID) error {
	f.reenabled = append(f.reenabled, id)
	if f.reenable != nil {
		return f.reenable(ctx, id)
	}
	return f.reenableErr
}
```

- [ ] **Step 4: Run the tests to verify they fail**

Run: `go test ./internal/api -run TestReenableSession -v`
Expected: FAIL to compile — `Control` lacks `Reenable`; `Server.ReenableSession` missing.

- [ ] **Step 5: Extend the Control interface and implement the handler**

In `internal/api/server.go`, add to the `Control` interface:

```go
type Control interface {
	Start(context.Context, uuid.UUID) error
	Stop(context.Context, uuid.UUID) error
	Reenable(context.Context, uuid.UUID) error
	Delete(context.Context, uuid.UUID) error
}
```

Add the handler after `StopSession`:

```go
// ReenableSession restarts a stopped or finished session with reset per-run
// counters, preserving prior samples, and returns the refreshed snapshot.
func (s *Server) ReenableSession(ctx context.Context, request openapi.ReenableSessionRequestObject) (openapi.ReenableSessionResponseObject, error) {
	if s.control == nil || s.store == nil {
		return nil, internalError()
	}
	err := s.control.Reenable(ctx, request.Id)
	switch {
	case err == nil:
		value, err := s.store.SessionByID(ctx, request.Id)
		if err != nil {
			return nil, internalError()
		}
		return openapi.ReenableSession200JSONResponse(mapSession(value)), nil
	case errors.Is(err, session.ErrNotFound):
		return nil, notFound()
	case errors.Is(err, session.ErrAlreadyRunning):
		return nil, sessionNotRunning()
	default:
		return nil, internalError()
	}
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/api -run TestReenableSession -v`
Expected: PASS.

- [ ] **Step 7: Verify generation clean, full Go tests, commit**

```bash
make generate && git diff --exit-code
go test ./...
git add api/openapi.yaml internal/api
git commit -m "feat: add POST /api/sessions/{id}/reenable endpoint"
```

---

## Task 6: Expose `proxy_username` on the session detail endpoint

**Files:**
- Modify: `api/openapi.yaml:231-260` (Session schema)
- Regenerate: `internal/api/openapi/openapi.gen.go`
- Modify: `internal/api/server.go:232-245` (`SessionByID`)
- Test: `internal/api/server_test.go` (add username tests)

**Interfaces:**
- Consumes: `s.cipher.Decrypt(ciphertext, nonce) (string, error)` (existing), `proxydial`-style URL parsing via `net/url`.
- Produces: `openapi.Session.ProxyUsername *string`, populated only by `SessionByID`.

- [ ] **Step 1: Add the schema field**

In `api/openapi.yaml`, under `Session.properties` (after `proxy_display`):

```yaml
        proxy_display: {type: string}
        proxy_username: {type: string, nullable: true}
```

- [ ] **Step 2: Regenerate**

Run: `make openapi-generate && go build ./...`
Expected: builds; `openapi.Session` gains `ProxyUsername *string`.

- [ ] **Step 3: Write the failing tests**

Add to `internal/api/server_test.go`:

```go
func TestSessionByIDExposesProxyUsernameNotPassword(t *testing.T) {
	store := newMemoryStore()
	handler := testHandler(t, store, &fakeControl{})
	// Create through the API so the proxy is encrypted with the handler's cipher.
	create := request(t, handler, http.MethodPost, "/api/sessions",
		`{"name":"n","proxy":"`+testProxy+`","mode":"sticky","cadence_seconds":30}`)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d", create.Code)
	}
	id := store.sessions[0].ID

	response := request(t, handler, http.MethodGet, "/api/sessions/"+id.String(), "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body.String())
	}
	var body struct {
		ProxyUsername *string `json:"proxy_username"`
	}
	decodeJSON(t, response, &body)
	if body.ProxyUsername == nil || *body.ProxyUsername != "proxy-user" {
		t.Fatalf("proxy_username = %v, want proxy-user", body.ProxyUsername)
	}
	// Never leak the password.
	if strings.Contains(response.Body.String(), "proxy-password") {
		t.Error("detail response leaked proxy password")
	}
}

func TestListSessionsOmitsProxyUsername(t *testing.T) {
	store := newMemoryStore()
	handler := testHandler(t, store, &fakeControl{})
	request(t, handler, http.MethodPost, "/api/sessions",
		`{"name":"n","proxy":"`+testProxy+`","mode":"sticky","cadence_seconds":30}`)

	response := request(t, handler, http.MethodGet, "/api/sessions", "")
	if strings.Contains(response.Body.String(), "proxy_username") {
		t.Error("list response should not include proxy_username")
	}
}
```

- [ ] **Step 4: Run the tests to verify they fail**

Run: `go test ./internal/api -run 'ProxyUsername|OmitsProxyUsername' -v`
Expected: FAIL — `proxy_username` absent from detail response.

- [ ] **Step 5: Populate the username in `SessionByID`**

In `internal/api/server.go`, ensure `net/url` is imported, and change the `SessionByID` return path to decrypt and attach the username:

```go
	mapped := mapSession(value)
	if s.cipher != nil {
		if plaintext, err := s.cipher.Decrypt(value.ProxyCiphertext, value.ProxyNonce); err == nil {
			if u, err := url.Parse(plaintext); err == nil {
				if username := u.User.Username(); username != "" {
					mapped.ProxyUsername = &username
				}
			}
		}
	}
	return openapi.SessionByID200JSONResponse(mapped), nil
```

(Decryption failure is non-fatal here: the field is simply omitted; the rest of the detail still renders.)

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/api -run 'ProxyUsername|OmitsProxyUsername' -v`
Expected: PASS.

- [ ] **Step 7: Verify generation clean, full tests, commit**

```bash
make generate && git diff --exit-code
go test ./...
git add api/openapi.yaml internal/api
git commit -m "feat: expose proxy_username on session detail endpoint"
```

---

## Task 7: Frontend attribute parser + comparison (pure module)

**Files:**
- Create: `web/src/lib/proxy-attributes.ts`
- Test: `web/src/lib/proxy-attributes.test.ts`

**Interfaces:**
- Consumes: `SessionReport` from `./api` (existing shape: `pool_composition`, `ips[]`, `stickiness`).
- Produces:
  - `parseProxyAttributes(username: string | null | undefined): ParsedAttributes`
  - `compareAttributes(parsed: ParsedAttributes, report: SessionReport | undefined): ComparisonRow[]`
  - types `AttributeCategory = 'geo' | 'session' | 'network' | 'other'`, `ParsedAttribute { category: AttributeCategory; key: string; label: string; value: string }`, `ParsedAttributes { attributes: ParsedAttribute[]; raw: string[] }`, `ComparisonRow { label: string; requested: string; observed: string; verdict: 'match' | 'partial' | 'mismatch' | 'unknown' }`.

- [ ] **Step 1: Write the failing tests**

Create `web/src/lib/proxy-attributes.test.ts`:

```ts
import { describe, expect, it } from 'vitest'

import { compareAttributes, parseProxyAttributes } from './proxy-attributes'
import type { SessionReport } from './api'

describe('parseProxyAttributes', () => {
  it('recognizes mixed-delimiter key/value attributes and groups them', () => {
    const parsed = parseProxyAttributes('user-country-us_city-newyork:type-resi-session-ab12')
    const byKey = Object.fromEntries(parsed.attributes.map((a) => [a.key, a]))
    expect(byKey.country.value).toBe('us')
    expect(byKey.country.category).toBe('geo')
    expect(byKey.city.value).toBe('newyork')
    expect(byKey.type.category).toBe('network')
    expect(byKey.session.category).toBe('session')
    // 'user' is an unrecognized leading token.
    expect(parsed.raw).toContain('user')
  })

  it('returns empty structures for empty input and never throws', () => {
    expect(parseProxyAttributes(null)).toEqual({ attributes: [], raw: [] })
    expect(parseProxyAttributes('')).toEqual({ attributes: [], raw: [] })
    expect(() => parseProxyAttributes('---___:::')).not.toThrow()
  })
})

describe('compareAttributes', () => {
  const report: SessionReport = {
    series: [],
    stickiness: { holds: [], rotations: [], average_hold_seconds: 0, median_hold_seconds: 0 },
    pool_growth: [],
    pool_composition: { mobile: 0.1, residential: 0.8, datacenter: 0.1, unknown: 0 },
    reputation_summary: { total_ips: 0, flagged_ips: 0, flagged_percent: 0, dnsbl_hit_ips: 0 },
    risk_histogram: [],
    ips: [
      { ip: '203.0.113.1', category: 'residential', country: 'US', isp: 'Acme', asn: 'AS1', risk_score: null, greynoise_class: '', dnsbl_listed: false, dnsbl_hits: [], first_seen: '', last_seen: '', hit_count: 9 },
      { ip: '203.0.113.2', category: 'residential', country: 'DE', isp: 'Acme', asn: 'AS1', risk_score: null, greynoise_class: '', dnsbl_listed: false, dnsbl_hits: [], first_seen: '', last_seen: '', hit_count: 1 },
    ],
  }

  it('marks a satisfied country request as a match', () => {
    const rows = compareAttributes(parseProxyAttributes('country-us'), report)
    const country = rows.find((r) => r.label === 'Country')!
    expect(country.requested).toBe('US')
    expect(country.observed).toContain('US')
    expect(country.verdict).toBe('match')
  })

  it('marks a network-type mismatch', () => {
    const rows = compareAttributes(parseProxyAttributes('type-mobile'), report)
    const net = rows.find((r) => r.label === 'Network type')!
    expect(net.verdict).toBe('mismatch') // observed is dominantly residential
  })

  it('shows requested-only rows for attributes with no observed counterpart', () => {
    const rows = compareAttributes(parseProxyAttributes('city-newyork'), report)
    const city = rows.find((r) => r.label === 'City')!
    expect(city.observed).toBe('—')
    expect(city.verdict).toBe('unknown')
  })
})
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd web && npx vitest run src/lib/proxy-attributes.test.ts`
Expected: FAIL — module not found.

- [ ] **Step 3: Implement the module**

Create `web/src/lib/proxy-attributes.ts`:

```ts
import type { IPRow, SessionReport } from './api'

export type AttributeCategory = 'geo' | 'session' | 'network' | 'other'

export interface ParsedAttribute {
  category: AttributeCategory
  key: string
  label: string
  value: string
}

export interface ParsedAttributes {
  attributes: ParsedAttribute[]
  raw: string[]
}

export interface ComparisonRow {
  label: string
  requested: string
  observed: string
  verdict: 'match' | 'partial' | 'mismatch' | 'unknown'
}

interface KeySpec {
  label: string
  category: AttributeCategory
}

// Recognized attribute keys and their aliases.
const KEYS: Record<string, KeySpec> = {
  country: { label: 'Country', category: 'geo' },
  cc: { label: 'Country', category: 'geo' },
  region: { label: 'Region', category: 'geo' },
  state: { label: 'State', category: 'geo' },
  st: { label: 'State', category: 'geo' },
  city: { label: 'City', category: 'geo' },
  session: { label: 'Session', category: 'session' },
  sess: { label: 'Session', category: 'session' },
  sid: { label: 'Session', category: 'session' },
  sticky: { label: 'Sticky', category: 'session' },
  rotate: { label: 'Rotation', category: 'session' },
  rotating: { label: 'Rotation', category: 'session' },
  ttl: { label: 'Session TTL', category: 'session' },
  sesstime: { label: 'Session TTL', category: 'session' },
  zone: { label: 'Network type', category: 'network' },
  type: { label: 'Network type', category: 'network' },
  isp: { label: 'ISP', category: 'network' },
  asn: { label: 'ASN', category: 'network' },
}

const CANONICAL_KEY: Record<string, string> = {
  cc: 'country', st: 'state', sess: 'session', sid: 'session',
  rotating: 'rotate', sesstime: 'ttl', zone: 'type',
}

export function parseProxyAttributes(username: string | null | undefined): ParsedAttributes {
  if (!username) return { attributes: [], raw: [] }
  const tokens = username.split(/[-_:]+/).filter((t) => t.length > 0)
  const attributes: ParsedAttribute[] = []
  const raw: string[] = []
  for (let i = 0; i < tokens.length; i++) {
    const key = tokens[i].toLowerCase()
    const spec = KEYS[key]
    if (spec && i + 1 < tokens.length) {
      attributes.push({ category: spec.category, key: CANONICAL_KEY[key] ?? key, label: spec.label, value: tokens[i + 1] })
      i++
      continue
    }
    raw.push(tokens[i])
  }
  return { attributes, raw }
}

function attribute(parsed: ParsedAttributes, key: string): ParsedAttribute | undefined {
  return parsed.attributes.find((a) => a.key === key)
}

function dominantCountry(ips: IPRow[]): { code: string; share: number } | undefined {
  if (ips.length === 0) return undefined
  const totals = new Map<string, number>()
  let sum = 0
  for (const ip of ips) {
    if (!ip.country) continue
    const weight = ip.hit_count > 0 ? ip.hit_count : 1
    totals.set(ip.country, (totals.get(ip.country) ?? 0) + weight)
    sum += weight
  }
  if (sum === 0) return undefined
  const [code, count] = [...totals.entries()].sort((a, b) => b[1] - a[1])[0]
  return { code, share: count / sum }
}

function dominantNetwork(report: SessionReport): { type: string; share: number } | undefined {
  const c = report.pool_composition
  const entries: Array<[string, number]> = [
    ['mobile', c.mobile], ['residential', c.residential], ['datacenter', c.datacenter], ['unknown', c.unknown],
  ]
  const sum = entries.reduce((acc, [, v]) => acc + v, 0)
  if (sum === 0) return undefined
  const [type, value] = entries.sort((a, b) => b[1] - a[1])[0]
  return { type, share: value / sum }
}

const NETWORK_ALIASES: Record<string, string> = {
  resi: 'residential', residential: 'residential', res: 'residential',
  mob: 'mobile', mobile: 'mobile', dc: 'datacenter', datacenter: 'datacenter', dch: 'datacenter',
}

function pct(share: number): string {
  return `${Math.round(share * 100)}%`
}

export function compareAttributes(parsed: ParsedAttributes, report: SessionReport | undefined): ComparisonRow[] {
  const rows: ComparisonRow[] = []

  const country = attribute(parsed, 'country')
  if (country) {
    const requested = country.value.toUpperCase()
    const observed = report ? dominantCountry(report.ips) : undefined
    rows.push(observed
      ? { label: 'Country', requested, observed: `${observed.code} (${pct(observed.share)})`, verdict: observed.code.toUpperCase() === requested ? (observed.share >= 0.9 ? 'match' : 'partial') : 'mismatch' }
      : { label: 'Country', requested, observed: '—', verdict: 'unknown' })
  }

  const type = attribute(parsed, 'type')
  if (type) {
    const requested = NETWORK_ALIASES[type.value.toLowerCase()] ?? type.value.toLowerCase()
    const observed = report ? dominantNetwork(report) : undefined
    rows.push(observed
      ? { label: 'Network type', requested, observed: `${observed.type} (${pct(observed.share)})`, verdict: observed.type === requested ? (observed.share >= 0.6 ? 'match' : 'partial') : 'mismatch' }
      : { label: 'Network type', requested, observed: '—', verdict: 'unknown' })
  }

  for (const key of ['region', 'state', 'city'] as const) {
    const attr = attribute(parsed, key)
    if (attr) rows.push({ label: attr.label, requested: attr.value, observed: '—', verdict: 'unknown' })
  }

  const isp = attribute(parsed, 'isp')
  if (isp && report) {
    const observed = report.ips[0]?.isp ?? ''
    rows.push({ label: 'ISP', requested: isp.value, observed: observed || '—', verdict: observed && observed.toLowerCase().includes(isp.value.toLowerCase()) ? 'match' : observed ? 'mismatch' : 'unknown' })
  }

  return rows
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd web && npx vitest run src/lib/proxy-attributes.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/proxy-attributes.ts web/src/lib/proxy-attributes.test.ts
git commit -m "feat: add proxy attribute parser and observed comparison"
```

---

## Task 8: Targeting panel component + wire into the detail page

**Files:**
- Create: `web/src/components/report/targeting-panel.tsx`
- Test: `web/src/components/report/targeting-panel.test.tsx`
- Modify: `web/src/lib/api.ts:7-32` (add `proxy_username`)
- Modify: `web/src/pages/session-page.tsx:78-89` (render in Overview)

**Interfaces:**
- Consumes: `parseProxyAttributes`, `compareAttributes` (Task 7); `Session.proxy_username`; `SessionReport`.
- Produces: `TargetingPanel({ session, report }: { session: Session; report: SessionReport | undefined })`.

- [ ] **Step 1: Add the API type field**

In `web/src/lib/api.ts`, add to the `Session` interface (after `proxy_display`):

```ts
  proxy_display: string
  proxy_username?: string | null
```

- [ ] **Step 2: Write the failing component test**

Create `web/src/components/report/targeting-panel.test.tsx`:

```tsx
import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { TargetingPanel } from './targeting-panel'
import type { Session, SessionReport } from '../../lib/api'

const session = { proxy_username: 'country-us-type-resi', proxy_display: 'p.example:8080' } as Session
const report = {
  series: [], stickiness: { holds: [], rotations: [], average_hold_seconds: 0, median_hold_seconds: 0 },
  pool_growth: [], pool_composition: { mobile: 0, residential: 1, datacenter: 0, unknown: 0 },
  reputation_summary: { total_ips: 0, flagged_ips: 0, flagged_percent: 0, dnsbl_hit_ips: 0 },
  risk_histogram: [], ips: [{ ip: '1.1.1.1', category: 'residential', country: 'US', isp: '', asn: '', risk_score: null, greynoise_class: '', dnsbl_listed: false, dnsbl_hits: [], first_seen: '', last_seen: '', hit_count: 5 }],
} as SessionReport

describe('TargetingPanel', () => {
  it('renders parsed attribute chips and a comparison', () => {
    render(<TargetingPanel session={session} report={report} />)
    expect(screen.getByText('Country')).toBeInTheDocument()
    expect(screen.getByText(/US/)).toBeInTheDocument()
  })

  it('renders a fallback when the proxy has no username', () => {
    render(<TargetingPanel session={{ ...session, proxy_username: null }} report={report} />)
    expect(screen.getByText(/no username/i)).toBeInTheDocument()
  })
})
```

- [ ] **Step 3: Run to verify it fails**

Run: `cd web && npx vitest run src/components/report/targeting-panel.test.tsx`
Expected: FAIL — component not found.

- [ ] **Step 4: Implement the component**

Create `web/src/components/report/targeting-panel.tsx`:

```tsx
import type { Session, SessionReport } from '../../lib/api'
import { compareAttributes, parseProxyAttributes } from '../../lib/proxy-attributes'

export function TargetingPanel({ session, report }: { session: Session; report: SessionReport | undefined }) {
  const parsed = parseProxyAttributes(session.proxy_username)
  if (parsed.attributes.length === 0 && parsed.raw.length === 0) {
    return (
      <section className="report-section targeting-panel">
        <h2>Targeting</h2>
        <p className="muted">No username on this proxy, so no targeting attributes are available.</p>
      </section>
    )
  }
  const rows = compareAttributes(parsed, report)
  const groups: Array<['geo' | 'session' | 'network' | 'other', string]> = [
    ['geo', 'Geo'], ['session', 'Session'], ['network', 'Network'], ['other', 'Other'],
  ]
  return (
    <section className="report-section targeting-panel">
      <h2>Targeting</h2>
      <div className="targeting-chips">
        {groups.map(([category, label]) => {
          const items = parsed.attributes.filter((a) => a.category === category)
          if (items.length === 0) return null
          return (
            <div key={category} className="targeting-group">
              <span className="targeting-group-label">{label}</span>
              {items.map((a) => (
                <span key={`${a.key}-${a.value}`} className="chip"><strong>{a.label}</strong> {a.value}</span>
              ))}
            </div>
          )
        })}
        {parsed.raw.length > 0 && (
          <div className="targeting-group">
            <span className="targeting-group-label">Unparsed</span>
            {parsed.raw.map((token, index) => <span key={`${token}-${index}`} className="chip mono">{token}</span>)}
          </div>
        )}
      </div>
      {rows.length > 0 && (
        <table className="targeting-comparison">
          <thead><tr><th>Attribute</th><th>Requested</th><th>Observed</th><th>Result</th></tr></thead>
          <tbody>
            {rows.map((row) => (
              <tr key={row.label}>
                <td>{row.label}</td>
                <td className="mono">{row.requested}</td>
                <td className="mono">{row.observed}</td>
                <td><span className={`verdict verdict-${row.verdict}`}>{row.verdict}</span></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}
```

- [ ] **Step 5: Wire it into the Overview tab**

In `web/src/pages/session-page.tsx`, import it:

```tsx
import { SummaryStrip } from '../components/report/summary-strip'
import { TargetingPanel } from '../components/report/targeting-panel'
```

Render it at the top of the Overview tab content, inside the `ReportBoundary`, before the `report-grid`:

```tsx
        <TabsContent className="tab-content" value="overview">
          <ReportBoundary report={report}>
            {report.data && <div className="report-grid">
```
becomes:
```tsx
        <TabsContent className="tab-content" value="overview">
          <TargetingPanel session={session.data} report={report.data} />
          <ReportBoundary report={report}>
            {report.data && <div className="report-grid">
```

(Placing `TargetingPanel` outside `ReportBoundary` lets requested attributes show even while the report is still loading; `report.data` is `undefined` until then, so the comparison fills in when it resolves.)

- [ ] **Step 6: Run the component test and the page test to verify they pass**

Run: `cd web && npx vitest run src/components/report/targeting-panel.test.tsx src/pages/session-page.test.tsx`
Expected: PASS. If the existing `session-page.test.tsx` needs a `proxy_username` in its fixture, add it as `null` (no behavior change).

- [ ] **Step 7: Add minimal styles**

In `web/src/index.css`, add rules for `.targeting-panel`, `.targeting-chips`, `.targeting-group`, `.chip`, `.targeting-comparison`, and `.verdict-match/.verdict-partial/.verdict-mismatch/.verdict-unknown` (follow existing color tokens used elsewhere in the file for badges/sections). Keep it consistent with the existing `.report-section` styling.

- [ ] **Step 8: Build the web bundle and commit**

```bash
cd web && npm run build && cd ..
git add web/src/lib/api.ts web/src/components/report/targeting-panel.tsx web/src/components/report/targeting-panel.test.tsx web/src/pages/session-page.tsx web/src/index.css
git commit -m "feat: show targeting attributes and requested-vs-observed on detail page"
```

---

## Task 9: Re-enable button + mutation on the detail page

**Files:**
- Modify: `web/src/lib/api.ts:206-218` (add `useReenableSession`)
- Modify: `web/src/pages/session-page.tsx:1-68` (button + confirm + copy)
- Test: `web/src/pages/session-page.test.tsx` (add re-enable interaction)

**Interfaces:**
- Consumes: `POST /api/sessions/{id}/reenable` (Task 5).
- Produces: `useReenableSession()` mutation hook.

- [ ] **Step 1: Write the failing page test**

Add to `web/src/pages/session-page.test.tsx` a test that renders a `stopped` session and asserts a "Re-enable" control is present and posts on confirm. Follow the file's existing render/mock harness (query client + route). Concretely, the assertion core:

```tsx
it('offers re-enable for a stopped session', async () => {
  // render SessionPage with a fetch mock returning a stopped session
  // (reuse this file's existing renderSessionPage helper / fetch stub)
  expect(await screen.findByRole('button', { name: /re-enable/i })).toBeInTheDocument()
})
```

If the file has no reusable render helper, mirror the existing tests' setup (they already mock `fetch` and wrap in `QueryClientProvider` + `MemoryRouter`); return `status: 'stopped'` from the session fetch.

- [ ] **Step 2: Run to verify it fails**

Run: `cd web && npx vitest run src/pages/session-page.test.tsx`
Expected: FAIL — no re-enable button.

- [ ] **Step 3: Add the mutation hook**

In `web/src/lib/api.ts`, after `useStopSession`:

```ts
export function useReenableSession() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api<Session>(`/api/sessions/${id}/reenable`, { method: 'POST' }),
    onSuccess: async (_data, id) => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['sessions'] }),
        queryClient.invalidateQueries({ queryKey: ['sessions', id] }),
        queryClient.invalidateQueries({ queryKey: ['session-report', id] }),
        queryClient.invalidateQueries({ queryKey: ['session-samples', id] }),
      ])
    },
  })
}
```

- [ ] **Step 4: Add the button, confirm, and updated copy**

In `web/src/pages/session-page.tsx`:

Update imports:
```tsx
import { AlertTriangle, Download, RotateCcw, Square } from 'lucide-react'
```
```tsx
import { APIError, useReenableSession, useSession, useSessionReport, useSessionSamples, useStopSession } from '../lib/api'
```

Add the mutation and handlers near `const stop = useStopSession()`:
```tsx
  const stop = useStopSession()
  const reenable = useReenableSession()
```

Update the stop confirm copy (a stopped session can now be re-enabled):
```tsx
  const confirmStop = () => {
    if (window.confirm(`Stop “${session.data.name}”? You can re-enable it later.`)) stop.mutate(id)
  }
  const confirmReenable = () => {
    if (window.confirm(`Re-enable “${session.data.name}”? Sampling resumes; progress counters reset while prior samples are kept.`)) reenable.mutate(id)
  }
```

In the `session-actions` block, add the re-enable button for non-running sessions (beside the existing Stop button):
```tsx
        <div className="session-actions">
          {session.data.status === 'running' && <Button type="button" variant="danger" onClick={confirmStop} disabled={stop.isPending}><Square size={14} fill="currentColor" aria-hidden="true" />{stop.isPending ? 'Stopping…' : 'Stop session'}</Button>}
          {session.data.status !== 'running' && <Button type="button" onClick={confirmReenable} disabled={reenable.isPending}><RotateCcw size={14} aria-hidden="true" />{reenable.isPending ? 'Re-enabling…' : 'Re-enable'}</Button>}
          <a className="button button-secondary" href={`/api/sessions/${id}/export.csv`} download><Download size={16} aria-hidden="true" />Export CSV</a>
        </div>
```

Add an error alert after the stop error alert:
```tsx
      {stop.isError && <Alert className="inline-alert"><AlertTriangle aria-hidden="true" /><div><strong>Could not stop session</strong><p>{errorMessage(stop.error)}</p></div></Alert>}
      {reenable.isError && <Alert className="inline-alert"><AlertTriangle aria-hidden="true" /><div><strong>Could not re-enable session</strong><p>{errorMessage(reenable.error)}</p></div></Alert>}
```

- [ ] **Step 5: Run the page test to verify it passes**

Run: `cd web && npx vitest run src/pages/session-page.test.tsx`
Expected: PASS.

- [ ] **Step 6: Full web test + build**

Run: `cd web && npm test -- --run && npm run build`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add web/src/lib/api.ts web/src/pages/session-page.tsx web/src/pages/session-page.test.tsx
git commit -m "feat: add re-enable action to session detail page"
```

---

## Final Verification

- [ ] **Whole-suite gate**

Run: `make generate && git diff --exit-code`
Expected: no diff.

Run: `make test`
Expected: Go + web tests PASS.

Run: `make test-race`
Expected: PASS.

- [ ] **Manual (compose stack)**

Bring up the stack (`docker compose up --build`), create a session whose proxy username encodes attributes (e.g. `http://country-us-type-resi-session-ab12:pass@host:8080`), let it take a few samples, stop it, open the detail page, confirm the Targeting panel shows chips + a requested-vs-observed comparison, then Re-enable and confirm: the header counters restart at zero, the samples tab shows the sequence continuing past the prior max (no gaps/collisions), and prior samples remain.

## Self-Review

**Spec coverage:**
- Username exposed on detail only, password never returned → Task 6 (+ `assertNoSecret`/leak test).
- Tolerant mixed-delimiter parser, recognized keys grouped, raw passthrough → Task 7.
- Requested-vs-observed comparison from existing report → Task 7 + Task 8.
- Re-enable for stopped and finished, continue sequence, keep data, reset per-run counters → Tasks 1–5.
- `distinct_ips` tied to retained inventory (not reset) → Task 2 SQL (omits `distinct_ips`), documented.
- `max_samples` re-evaluated per run; `max_duration` from new `started_at` → Task 2 (reset `started_at`) + Task 3 (cap uses per-run count, unchanged).
- Frontend button gated on non-running status, confirm dialog, query invalidation, updated stop copy → Task 9.
- Generation determinism, no-secret, single-replica constraints → Global Constraints + per-task `make generate` gates.

**Placeholder scan:** No TBD/TODO. Two frontend test steps (Task 8 Step 7 styles, Task 9 Step 1 test) reference the file's existing render/mock harness rather than reprinting it, because those harnesses already exist in-repo and must be matched, not recreated; the assertion cores are concrete.

**Type consistency:** `SequenceOffset` (`int` on `session.Session`, `int32` in db row/params) is threaded consistently (Tasks 1–3). `Reenable` signatures match across `session.Store` (`ctx,id,at`), `db.Store`, `Control`/`Supervisor` (`ctx,id`), and fakes (Tasks 2/4/5). `parseProxyAttributes`/`compareAttributes`/`TargetingPanel` names and types are consistent across Tasks 7–8. `useReenableSession` returns `Session` matching the endpoint's `200` body (Tasks 5/9).
