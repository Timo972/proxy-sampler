package db_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresdb "github.com/timo972/proxy-sampler/internal/db"
	"github.com/timo972/proxy-sampler/internal/migrate"
	"github.com/timo972/proxy-sampler/internal/session"
)

func TestSessionRoundTripDoesNotRequirePlaintextProxy(t *testing.T) {
	store := testStore(t)
	want := testSession()

	got, err := store.Create(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	assertSession(t, got, want)

	byID, err := store.SessionByID(context.Background(), want.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertSession(t, byID, want)

	all, err := store.Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("sessions=%#v", all)
	}
	assertSession(t, all[0], want)
}

func TestRunningSessionsExcludesStoppedRows(t *testing.T) {
	store := testStore(t)
	running := insertTestSession(t, store)
	stopped := testSession()
	stopped.ID = uuid.New()
	stopped.Name = "stopped"
	stopped.Status = session.StatusStopped
	at := stopped.CreatedAt.Add(time.Minute)
	stopped.StoppedAt = &at
	if _, err := store.Create(context.Background(), stopped); err != nil {
		t.Fatal(err)
	}

	got, err := store.RunningSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != running.ID {
		t.Fatalf("running sessions=%#v", got)
	}
}

func TestStopOnlyTransitionsRunningSessions(t *testing.T) {
	store := testStore(t)
	s := insertTestSession(t, store)
	at := s.CreatedAt.Add(time.Minute)

	if err := store.Stop(context.Background(), s.ID, at); err != nil {
		t.Fatal(err)
	}
	got, err := store.SessionByID(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != session.StatusStopped || got.StoppedAt == nil || !got.StoppedAt.Equal(at) {
		t.Fatalf("stopped session=%#v", got)
	}
	if err := store.Stop(context.Background(), s.ID, at.Add(time.Minute)); !errors.Is(err, session.ErrNotRunning) {
		t.Fatalf("second stop error=%v", err)
	}
}

func TestFinishOnlyTransitionsRunningSessions(t *testing.T) {
	store := testStore(t)
	s := insertTestSession(t, store)
	at := s.CreatedAt.Add(2 * time.Minute)

	if err := store.Finish(context.Background(), s.ID, at); err != nil {
		t.Fatal(err)
	}
	got, err := store.SessionByID(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != session.StatusFinished || got.StoppedAt == nil || !got.StoppedAt.Equal(at) {
		t.Fatalf("finished session=%#v", got)
	}
	if err := store.Finish(context.Background(), s.ID, at.Add(time.Minute)); !errors.Is(err, session.ErrNotRunning) {
		t.Fatalf("second finish error=%v", err)
	}
}

func TestSaveTickUpdatesSnapshotAndIPInventory(t *testing.T) {
	store := testStore(t)
	s := insertTestSession(t, store)
	ip := netip.MustParseAddr("203.0.113.7")
	now := time.Now().UTC().Truncate(time.Millisecond)
	err := store.SaveTick(context.Background(), s.ID, session.Snapshot{
		SamplesTaken: 1, ProbesOK: 2, ProbesTotal: 3, DistinctIPs: 1,
		LastSampleAt: &now, LastPrimaryIP: ip, LastRTT: durationPtr(125 * time.Millisecond),
	}, []session.IPHit{{IP: ip, SeenAt: now, Hits: 2}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.SessionByID(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Snapshot.SamplesTaken != 1 || got.Snapshot.ProbesOK != 2 {
		t.Fatalf("snapshot=%#v", got.Snapshot)
	}
	ips, err := store.SessionIPs(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 1 || ips[0].IP != ip || ips[0].HitCount != 2 {
		t.Fatalf("ips=%#v", ips)
	}

	later := now.Add(time.Second)
	if err := store.SaveTick(context.Background(), s.ID, session.Snapshot{
		SamplesTaken: 2, ProbesOK: 3, ProbesTotal: 5, DistinctIPs: 1,
		LastSampleAt: &later, LastPrimaryIP: ip,
	}, []session.IPHit{{IP: ip, SeenAt: later, Hits: 1}}); err != nil {
		t.Fatal(err)
	}
	ips, err = store.SessionIPs(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 1 || ips[0].HitCount != 3 || !ips[0].FirstSeen.Equal(now) || !ips[0].LastSeen.Equal(later) {
		t.Fatalf("updated ips=%#v", ips)
	}
}

func TestSaveTickRollsBackSnapshotWhenIPWriteFails(t *testing.T) {
	store := testStore(t)
	s := insertTestSession(t, store)
	now := time.Now().UTC().Truncate(time.Millisecond)
	err := store.SaveTick(context.Background(), s.ID, session.Snapshot{
		SamplesTaken: 1, ProbesOK: 1, ProbesTotal: 1, DistinctIPs: 1, LastSampleAt: &now,
	}, []session.IPHit{{SeenAt: now, Hits: 1}})
	if err == nil {
		t.Fatal("expected invalid IP write to fail")
	}

	got, err := store.SessionByID(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Snapshot.SamplesTaken != 0 || got.Snapshot.ProbesTotal != 0 {
		t.Fatalf("snapshot was not rolled back: %#v", got.Snapshot)
	}
}

func TestReputationUpsertPreservesFirstSeenAndAdvancesRefreshedAt(t *testing.T) {
	store := testStore(t)
	ip := netip.MustParseAddr("198.51.100.9")
	if _, found, err := store.ReputationByIP(context.Background(), ip); err != nil || found {
		t.Fatalf("missing reputation found=%v err=%v", found, err)
	}
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	refreshed := first.Add(time.Minute)
	reputation := session.Reputation{
		IP: ip, Country: "DE", Region: "Berlin", City: "Berlin", ISP: "Example ISP", ASN: "AS64500",
		IsMobile: boolPtr(false), IPAPIProxy: boolPtr(true), IPAPIHosting: boolPtr(false),
		ProxyCheckType: "residential", ProxyCheckProxy: boolPtr(true), RiskScore: intPtr(42),
		GreyNoiseClass: "benign", SFSAppears: boolPtr(false), SFSFrequency: intPtr(0),
		DNSBLListed: boolPtr(true), DNSBLHits: []string{"zen.example", "spam.example"},
		Category: "residential-proxy", Raw: json.RawMessage(`{"source":"first"}`),
		FirstSeen: first, RefreshedAt: first,
	}
	if err := store.SaveReputation(context.Background(), reputation); err != nil {
		t.Fatal(err)
	}
	reputation.Country = "NL"
	reputation.Raw = json.RawMessage(`{"source":"refresh"}`)
	reputation.FirstSeen = refreshed
	reputation.RefreshedAt = refreshed
	if err := store.SaveReputation(context.Background(), reputation); err != nil {
		t.Fatal(err)
	}

	got, found, err := store.ReputationByIP(context.Background(), ip)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("reputation not found")
	}
	if !got.FirstSeen.Equal(first) || !got.RefreshedAt.Equal(refreshed) || got.Country != "NL" {
		t.Fatalf("reputation times/data=%#v", got)
	}
	if !reflect.DeepEqual(got.DNSBLHits, reputation.DNSBLHits) || !jsonEqual(got.Raw, reputation.Raw) {
		t.Fatalf("reputation payload=%#v", got)
	}
}

func TestDeleteCascadesSessionIPsButPreservesGlobalReputation(t *testing.T) {
	store := testStore(t)
	s := insertTestSession(t, store)
	ip := netip.MustParseAddr("192.0.2.44")
	now := time.Now().UTC().Truncate(time.Millisecond)
	reputation := session.Reputation{
		IP: ip, Category: "hosting", Raw: json.RawMessage(`{"source":"test"}`),
		FirstSeen: now, RefreshedAt: now,
	}
	if err := store.SaveReputation(context.Background(), reputation); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTick(context.Background(), s.ID, session.Snapshot{
		SamplesTaken: 1, ProbesOK: 1, ProbesTotal: 1, DistinctIPs: 1,
		LastSampleAt: &now, LastPrimaryIP: ip,
	}, []session.IPHit{{IP: ip, SeenAt: now, Hits: 1}}); err != nil {
		t.Fatal(err)
	}
	withCategory, err := store.SessionByID(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if withCategory.Snapshot.LastCategory != "hosting" {
		t.Fatalf("last category=%q", withCategory.Snapshot.LastCategory)
	}
	ips, err := store.SessionIPs(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 1 || ips[0].Reputation == nil || ips[0].Reputation.Category != "hosting" {
		t.Fatalf("session IP reputation=%#v", ips)
	}

	if err := store.Delete(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	ips, err = store.SessionIPs(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 0 {
		t.Fatalf("session IPs after delete=%#v", ips)
	}
	if _, found, err := store.ReputationByIP(context.Background(), ip); err != nil || !found {
		t.Fatalf("global reputation found=%v err=%v", found, err)
	}
	if _, err := store.SessionByID(context.Background(), s.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("deleted session error=%v", err)
	}
}

func testStore(t *testing.T) session.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL")
	}
	ctx := context.Background()
	if err := migrate.Up(ctx, dsn, slog.Default()); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE sampling_sessions, ip_reputation_cache CASCADE"); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "TRUNCATE sampling_sessions, ip_reputation_cache CASCADE")
		pool.Close()
	})
	return postgresdb.NewStore(pool)
}

func insertTestSession(t *testing.T, store session.Store) session.Session {
	t.Helper()
	s := testSession()
	created, err := store.Create(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func testSession() session.Session {
	now := time.Now().UTC().Truncate(time.Millisecond)
	started := now.Add(time.Second)
	maxSamples := 20
	maxDuration := 10 * time.Minute
	return session.Session{
		ID: uuid.New(), Name: "office proxy", ProxyCiphertext: []byte{0x01, 0x02, 0x03},
		ProxyNonce: []byte{0x04, 0x05}, ProxyDisplay: "http://user:***@proxy.example:8080",
		Mode: session.ModeSticky, Cadence: 30 * time.Second, ProbesPerSample: 3,
		ProbeTarget: "https://example.com/trace", DialTimeout: 5 * time.Second,
		MaxSamples: &maxSamples, MaxDuration: &maxDuration, Status: session.StatusRunning,
		CreatedAt: now, StartedAt: &started,
	}
}

func assertSession(t *testing.T, got, want session.Session) {
	t.Helper()
	if got.ID != want.ID || got.Name != want.Name || !bytes.Equal(got.ProxyCiphertext, want.ProxyCiphertext) ||
		!bytes.Equal(got.ProxyNonce, want.ProxyNonce) || got.ProxyDisplay != want.ProxyDisplay || got.Mode != want.Mode ||
		got.Cadence != want.Cadence || got.ProbesPerSample != want.ProbesPerSample || got.ProbeTarget != want.ProbeTarget ||
		got.DialTimeout != want.DialTimeout || !reflect.DeepEqual(got.MaxSamples, want.MaxSamples) ||
		!reflect.DeepEqual(got.MaxDuration, want.MaxDuration) || got.Status != want.Status ||
		!got.CreatedAt.Equal(want.CreatedAt) || !timePtrEqual(got.StartedAt, want.StartedAt) ||
		!timePtrEqual(got.StoppedAt, want.StoppedAt) {
		t.Fatalf("session mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func jsonEqual(a, b []byte) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

func durationPtr(v time.Duration) *time.Duration { return &v }
func boolPtr(v bool) *bool                       { return &v }
func intPtr(v int) *int                          { return &v }
