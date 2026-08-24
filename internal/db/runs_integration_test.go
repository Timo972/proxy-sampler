package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/timo972/proxy-sampler/migrations"

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
	// Creation always generates both names, exactly as CreateRun does.
	generatedChild.Name = variation.VariantName("Alpha", generated)
	customChild.Name = variation.VariantName("Alpha", map[string]string{"country": "us"})
	children := []variation.ChildSession{
		{Session: generatedChild, Params: json.RawMessage(`{"country":"de"}`), CellKey: `{"country":"de"}`},
		{Session: customChild, Params: json.RawMessage(`{"country":"us"}`), CellKey: `{"country":"us"}`},
	}
	if err := store.CreateRun(ctx, run, children); err != nil {
		t.Fatal(err)
	}
	// The second child becomes custom the only way it can in production: a
	// session rename, which is what records its provenance.
	if err := sessionStore.Rename(ctx, customChild.ID, "My custom probe"); err != nil {
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
	// Mirrors what the session rename actually writes, provenance included.
	if _, err := tx.Exec(ctx,
		"UPDATE sampling_sessions SET name = $1, name_customized = true WHERE id = $2",
		"My concurrent name", child.ID,
	); err != nil {
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

// TestRenameRunPreservesACustomNameThatLooksGenerated is the case text matching
// cannot get right: a child renamed by hand to a name that a *later* run name
// would generate. Renaming the run to that name and then onward must not
// reclassify the custom child as generated and overwrite it.
func TestRenameRunPreservesACustomNameThatLooksGenerated(t *testing.T) {
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

	// The operator picks a name that happens to match what the generator would
	// produce for the run name "Beta".
	custom := variation.VariantName("Beta", params)
	if err := sessionStore.Rename(ctx, child.ID, custom); err != nil {
		t.Fatal(err)
	}

	// Renaming to "Beta" must leave it alone (the old name no longer matches)...
	if err := store.RenameRun(ctx, run.ID, "Beta"); err != nil {
		t.Fatal(err)
	}
	if got := childName(t, store, run.ID); got != custom {
		t.Fatalf("after the first rename child = %q, want %q preserved", got, custom)
	}

	// ...and so must renaming onward to "Gamma", even though the child's custom
	// name is now exactly what "Beta" would have generated.
	if err := store.RenameRun(ctx, run.ID, "Gamma"); err != nil {
		t.Fatal(err)
	}
	if got := childName(t, store, run.ID); got != custom {
		t.Errorf("after the second rename child = %q, want %q preserved", got, custom)
	}
}

func childName(t *testing.T, store variation.Store, runID uuid.UUID) string {
	t.Helper()
	sessions, err := store.RunSessions(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("run sessions = %d, want 1", len(sessions))
	}
	return sessions[0].Name
}

// TestNameCustomizedBackfillClassifiesExistingRows exercises the migration's
// backfill, which is the only chance existing rows get to have their provenance
// inferred. It runs the real statements from the migration file against rows
// reset to the pre-migration default, so the SQL that reproduces VariantName
// cannot drift from VariantName itself unnoticed.
func TestNameCustomizedBackfillClassifiesExistingRows(t *testing.T) {
	sessionStore := testStore(t)
	store := sessionStore.(variation.Store)
	ctx := context.Background()

	multi := map[string]string{"region": "eu", "asn": "64500"}
	run := variation.Run{
		ID: uuid.New(), Name: "Alpha", TemplateCiphertext: []byte("c"),
		TemplateNonce: []byte("n"), TemplateDisplay: "gate:1080",
		Axes: json.RawMessage(`{}`), CreatedAt: nowUTC(),
	}
	// mixedCase and nonASCII are the cases where a locale-aware cluster
	// collation orders keys differently from the bytewise sort.Strings that
	// VariantName uses: under en_US.UTF-8 "a" sorts before "Z", and "ä" sorts
	// beside "a" rather than after "z".
	mixedCase := map[string]string{"Z": "1", "a": "2"}
	nonASCII := map[string]string{"ä": "1", "z": "2"}
	generated, multiKey, noParams := testSession(), testSession(), testSession()
	mixedCaseChild, nonASCIIChild, custom := testSession(), testSession(), testSession()
	generated.ID, multiKey.ID, noParams.ID = uuid.New(), uuid.New(), uuid.New()
	mixedCaseChild.ID, nonASCIIChild.ID, custom.ID = uuid.New(), uuid.New(), uuid.New()
	generated.Name = variation.VariantName("Alpha", map[string]string{"region": "eu"})
	multiKey.Name = variation.VariantName("Alpha", multi)
	noParams.Name = variation.VariantName("Alpha", nil)
	mixedCaseChild.Name = variation.VariantName("Alpha", mixedCase)
	nonASCIIChild.Name = variation.VariantName("Alpha", nonASCII)
	custom.Name = "My custom probe"
	if err := store.CreateRun(ctx, run, []variation.ChildSession{
		{Session: generated, Params: json.RawMessage(`{"region":"eu"}`), CellKey: `{"region":"eu"}`},
		{Session: multiKey, Params: json.RawMessage(`{"region":"eu","asn":"64500"}`), CellKey: `{}`},
		{Session: noParams, Params: json.RawMessage(`{}`), CellKey: `{}`},
		{Session: mixedCaseChild, Params: json.RawMessage(`{"Z":"1","a":"2"}`), CellKey: `{}`},
		{Session: nonASCIIChild, Params: json.RawMessage(`{"ä":"1","z":"2"}`), CellKey: `{}`},
		{Session: custom, Params: json.RawMessage(`{"region":"us"}`), CellKey: `{"region":"us"}`},
	}); err != nil {
		t.Fatal(err)
	}
	standalone := insertTestSession(t, sessionStore)

	pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// Reset every row to the column default, reproducing the state the backfill
	// finds when the migration first runs.
	if _, err := pool.Exec(ctx, "UPDATE sampling_sessions SET name_customized = false"); err != nil {
		t.Fatal(err)
	}
	for _, statement := range backfillStatements(t) {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("backfill statement failed: %v\n%s", err, statement)
		}
	}

	for _, tt := range []struct {
		name string
		id   uuid.UUID
		want bool
	}{
		{"generated child", generated.ID, false},
		{"generated child with several params", multiKey.ID, false},
		{"generated child with no params", noParams.ID, false},
		{"generated child with mixed-case keys", mixedCaseChild.ID, false},
		{"generated child with non-ASCII keys", nonASCIIChild.ID, false},
		{"hand-renamed child", custom.ID, true},
		{"standalone session", standalone.ID, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var customized bool
			if err := pool.QueryRow(ctx, "SELECT name_customized FROM sampling_sessions WHERE id = $1", tt.id).Scan(&customized); err != nil {
				t.Fatal(err)
			}
			if customized != tt.want {
				t.Errorf("name_customized = %v, want %v", customized, tt.want)
			}
		})
	}
}

// backfillStatements returns the migration's Up statements other than the ALTER
// that adds the column, read from the migration file itself so the test cannot
// drift from the SQL that actually ships.
func backfillStatements(t *testing.T) []string {
	t.Helper()
	raw, err := migrations.FS.ReadFile("postgres/20260824120000_session_name_customized.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, _, found := strings.Cut(string(raw), "-- +goose Down")
	if !found {
		t.Fatal("migration has no Down section; the file layout changed")
	}
	var out []string
	for _, statement := range strings.Split(up, ";") {
		if strings.Contains(statement, "ALTER TABLE") || strings.TrimSpace(stripSQLComments(statement)) == "" {
			continue
		}
		out = append(out, statement)
	}
	if len(out) != 2 {
		t.Fatalf("found %d backfill statements, want 2; the migration changed shape", len(out))
	}
	return out
}

func stripSQLComments(statement string) string {
	var kept []string
	for _, line := range strings.Split(statement, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}
