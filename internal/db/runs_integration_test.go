package db_test

import (
	"context"
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

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
