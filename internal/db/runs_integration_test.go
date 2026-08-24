package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/timo972/proxy-sampler/internal/session"
	"github.com/timo972/proxy-sampler/internal/variation"
)

func nowUTC() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

func TestCreateRunPersistsChildren(t *testing.T) {
	sessionStore := testStore(t)
	store := sessionStore.(variation.Store)
	ctx := context.Background()

	run := variation.Run{
		ID: uuid.New(), Name: "poolcheck", TemplateCiphertext: []byte("c"),
		TemplateNonce: []byte("n"), TemplateDisplay: "gate:1080",
		Axes:      json.RawMessage(`{"country":{"kind":"list","values":["de"]}}`),
		CreatedAt: nowUTC(),
	}
	child := testSession()
	child.ID = uuid.New()
	children := []variation.ChildSession{{
		Session: child, Params: json.RawMessage(`{"country":"de"}`), CellKey: `{"country":"de"}`,
	}}
	if err := store.CreateRun(ctx, run, children); err != nil {
		t.Fatal(err)
	}

	summary, err := store.RunByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.VariantCount != 1 {
		t.Fatalf("variant count = %d, want 1", summary.VariantCount)
	}
	if summary.Status != variation.RunRunning {
		t.Fatalf("status = %q, want running", summary.Status)
	}

	sessions, err := store.RunSessions(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].CellKey != `{"country":"de"}` {
		t.Fatalf("run sessions = %#v", sessions)
	}
	if sessions[0].TargetCountry != "DE" {
		t.Fatalf("run session target country = %q, want DE (from child session row)", sessions[0].TargetCountry)
	}

	// Child is excluded from the standalone session list.
	standalone, err := sessionStore.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range standalone {
		if s.ID == child.ID {
			t.Fatal("child session leaked into standalone Sessions()")
		}
	}
}

func TestStreamPoolIPsDedupesAndSumsHits(t *testing.T) {
	sessionStore := testStore(t)
	store := sessionStore.(variation.Store)
	ctx := context.Background()

	run := variation.Run{ID: uuid.New(), Name: "pool", TemplateCiphertext: []byte("c"), TemplateNonce: []byte("n"), TemplateDisplay: "g", Axes: json.RawMessage(`{}`), CreatedAt: nowUTC()}
	c1, c2 := testSession(), testSession()
	c1.ID, c2.ID = uuid.New(), uuid.New()
	if err := store.CreateRun(ctx, run, []variation.ChildSession{
		{Session: c1, Params: json.RawMessage(`{}`), CellKey: `{}`},
		{Session: c2, Params: json.RawMessage(`{}`), CellKey: `{}`},
	}); err != nil {
		t.Fatal(err)
	}

	shared := netip.MustParseAddr("203.0.113.9")
	unique := netip.MustParseAddr("203.0.113.10")
	at := nowUTC()
	// Both children observe the shared IP; only c2 observes the unique IP.
	if err := sessionStore.SaveTick(ctx, c1.ID, c1.Snapshot, []session.IPHit{{IP: shared, SeenAt: at, Hits: 3}}); err != nil {
		t.Fatal(err)
	}
	if err := sessionStore.SaveTick(ctx, c2.ID, c2.Snapshot, []session.IPHit{{IP: shared, SeenAt: at, Hits: 2}, {IP: unique, SeenAt: at, Hits: 1}}); err != nil {
		t.Fatal(err)
	}

	byIP := map[string]int64{}
	if err := store.StreamPoolIPs(ctx, run.ID, func(row variation.IPRow) error {
		byIP[row.IP] = row.HitCount
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(byIP) != 2 {
		t.Fatalf("streamed %d distinct IPs, want 2 (shared IP deduped)", len(byIP))
	}
	if byIP[shared.String()] != 5 {
		t.Fatalf("shared IP hit_count = %d, want 5 (3+2 summed across children)", byIP[shared.String()])
	}
	if byIP[unique.String()] != 1 {
		t.Fatalf("unique IP hit_count = %d, want 1", byIP[unique.String()])
	}
}

func TestRunStatusDerivation(t *testing.T) {
	store := testStore(t).(variation.Store)
	ctx := context.Background()
	run := variation.Run{ID: uuid.New(), Name: "r", TemplateCiphertext: []byte("c"), TemplateNonce: []byte("n"), TemplateDisplay: "g", Axes: json.RawMessage(`{}`), CreatedAt: nowUTC()}
	stopped := testSession()
	stopped.ID = uuid.New()
	stopped.Status = session.StatusStopped
	if err := store.CreateRun(ctx, run, []variation.ChildSession{{Session: stopped, Params: json.RawMessage(`{}`), CellKey: `{}`}}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.RunByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Status != variation.RunStopped {
		t.Fatalf("status = %q, want stopped", summary.Status)
	}
}

// TestRenameRunCascadesToGeneratedVariantNamesOnly proves the cascade rule
// against real SQL: children still carrying the name generated from the old run
// name follow the rename, and a hand-renamed child keeps its own name.
func TestRenameRunCascadesToGeneratedVariantNamesOnly(t *testing.T) {
	sessionStore := testStore(t)
	store := sessionStore.(variation.Store)
	ctx := context.Background()

	generated := map[string]string{"country": "de"}
	run := variation.Run{
		ID: uuid.New(), Name: "Alpha", TemplateCiphertext: []byte("c"),
		TemplateNonce: []byte("n"), TemplateDisplay: "gate:1080",
		Axes:      json.RawMessage(`{"country":{"kind":"list","values":["de","us"]}}`),
		CreatedAt: nowUTC(),
	}
	generatedChild, customChild := testSession(), testSession()
	generatedChild.ID, customChild.ID = uuid.New(), uuid.New()
	generatedChild.Name = variation.VariantName("Alpha", generated)
	customChild.Name = "My custom probe"
	children := []variation.ChildSession{
		{Session: generatedChild, Params: json.RawMessage(`{"country":"de"}`), CellKey: `{"country":"de"}`},
		{Session: customChild, Params: json.RawMessage(`{"country":"us"}`), CellKey: `{"country":"us"}`},
	}
	if err := store.CreateRun(ctx, run, children); err != nil {
		t.Fatal(err)
	}

	if err := store.RenameRun(ctx, run.ID, "Beta"); err != nil {
		t.Fatal(err)
	}

	summary, err := store.RunByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Name != "Beta" {
		t.Errorf("run name = %q, want Beta", summary.Name)
	}
	names := map[uuid.UUID]string{}
	sessions, err := store.RunSessions(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		names[s.SessionID] = s.Name
	}
	if got, want := names[generatedChild.ID], variation.VariantName("Beta", generated); got != want {
		t.Errorf("generated variant name = %q, want %q", got, want)
	}
	if got := names[customChild.ID]; got != "My custom probe" {
		t.Errorf("hand-renamed variant = %q, want it preserved", got)
	}
}

func TestRenameRunUnknownRun(t *testing.T) {
	sessionStore := testStore(t)
	store := sessionStore.(variation.Store)

	err := store.RenameRun(context.Background(), uuid.New(), "Beta")
	if !errors.Is(err, variation.ErrRunNotFound) {
		t.Fatalf("error = %v, want ErrRunNotFound", err)
	}
}

// TestRenameSessionUpdatesStandaloneSession covers the session-level rename the
// run cascade builds on.
func TestRenameSessionUpdatesStandaloneSession(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	s := insertTestSession(t, store)

	if err := store.Rename(ctx, s.ID, "Frankfurt residential"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.SessionByID(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Name != "Frankfurt residential" {
		t.Errorf("name = %q, want the new name", reloaded.Name)
	}
}

// TestRenameSessionSurvivesASamplerTick guards the one interaction between
// renaming and the sampler: a running session keeps its new name when the
// worker writes its next snapshot.
func TestRenameSessionSurvivesASamplerTick(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	s := insertTestSession(t, store)
	ip := netip.MustParseAddr("203.0.113.7")
	now := nowUTC()

	if err := store.Rename(ctx, s.ID, "Renamed mid-flight"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTick(ctx, s.ID, session.Snapshot{
		SamplesTaken: 1, ProbesOK: 2, ProbesTotal: 3, DistinctIPs: 1,
		LastSampleAt: &now, LastPrimaryIP: ip,
	}, []session.IPHit{{IP: ip, SeenAt: now, Hits: 2}}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := store.SessionByID(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Name != "Renamed mid-flight" {
		t.Errorf("name = %q, want the rename to survive the tick", reloaded.Name)
	}
	if reloaded.Snapshot.SamplesTaken != 1 {
		t.Errorf("samples taken = %d, want the tick to still have been applied", reloaded.Snapshot.SamplesTaken)
	}
}

func TestRenameSessionUnknownSession(t *testing.T) {
	store := testStore(t)

	err := store.Rename(context.Background(), uuid.New(), "Anything")
	if !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// TestRenameRunPreservesAChildRenamedDuringTheCascade drives the real race the
// cascade has to survive: a session-level rename that commits after RenameRun
// has read the child's generated name but before it writes the new one.
//
// The interleaving is forced rather than timed. A separate transaction holds a
// row lock on the child, so RenameRun blocks on its UPDATE after the read; the
// holder then renames the child and commits. Under READ COMMITTED, Postgres
// re-evaluates the blocked UPDATE against the newly committed row, so a
// conditional update skips the child while an unconditional one clobbers it.
func TestRenameRunPreservesAChildRenamedDuringTheCascade(t *testing.T) {
	sessionStore := testStore(t)
	store := sessionStore.(variation.Store)
	ctx := context.Background()

	params := map[string]string{"region": "eu"}
	run := variation.Run{
		ID: uuid.New(), Name: "Alpha", TemplateCiphertext: []byte("c"),
		TemplateNonce: []byte("n"), TemplateDisplay: "gate:1080",
		Axes:      json.RawMessage(`{"region":{"kind":"list","values":["eu"]}}`),
		CreatedAt: nowUTC(),
	}
	child := testSession()
	child.ID = uuid.New()
	child.Name = variation.VariantName("Alpha", params)
	if err := store.CreateRun(ctx, run, []variation.ChildSession{{
		Session: child, Params: json.RawMessage(`{"region":"eu"}`), CellKey: `{"region":"eu"}`,
	}}); err != nil {
		t.Fatal(err)
	}

	holder, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Take the child's row lock without changing its value, so RenameRun still
	// reads the generated name and only blocks once it tries to write.
	if _, err := tx.Exec(ctx, "UPDATE sampling_sessions SET name = name WHERE id = $1", child.ID); err != nil {
		t.Fatal(err)
	}

	renamed := make(chan error, 1)
	go func() { renamed <- store.RenameRun(ctx, run.ID, "Beta") }()

	waitForBlockedCascadeWrite(t, ctx, holder)
	if _, err := tx.Exec(ctx, "UPDATE sampling_sessions SET name = $1 WHERE id = $2", "My concurrent name", child.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-renamed:
		if err != nil {
			t.Fatalf("rename run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("RenameRun did not finish after the lock was released")
	}

	sessions, err := store.RunSessions(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("run sessions = %d, want 1", len(sessions))
	}
	if sessions[0].Name != "My concurrent name" {
		t.Errorf("child name = %q, want the concurrent rename preserved", sessions[0].Name)
	}
	summary, err := store.RunByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Name != "Beta" {
		t.Errorf("run name = %q, want Beta (the run rename still applies)", summary.Name)
	}
}

// waitForBlockedCascadeWrite blocks until the cascade's own child update is
// waiting on a lock, so the test does not race the goroutine it is interleaving
// with. It matches the statement text (sqlc keeps the query name in the SQL) so
// a blocked query from another package sharing this database cannot be mistaken
// for the one under test.
func waitForBlockedCascadeWrite(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND state = 'active'
			  AND query LIKE '%RenameGeneratedRunSession%'`,
		).Scan(&waiting)
		if err == nil && waiting > 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the cascade's child update never blocked; the interleaving did not happen")
}
