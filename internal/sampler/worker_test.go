package sampler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/proxy"

	"github.com/timo972/proxy-sampler/internal/ch"
	appcrypto "github.com/timo972/proxy-sampler/internal/crypto"
	"github.com/timo972/proxy-sampler/internal/session"
)

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

func TestPreparedWorkerTickPersistsBeforeEnqueueAndLimitsOrderedProbes(t *testing.T) {
	store := newFakeSessionStore()
	value, cipher := encryptedSession(t, 10)
	value.Snapshot = session.Snapshot{SamplesTaken: 12, ProbesOK: 20, ProbesTotal: 24, DistinctIPs: 1, LastPrimaryIP: netip.MustParseAddr("192.0.2.1")}
	store.sessions[value.ID] = value
	store.ips[value.ID] = []session.IPRecord{{IP: value.Snapshot.LastPrimaryIP}}

	prober := &orderedProber{}
	lookup := &fakeLookup{reputations: map[netip.Addr]session.Reputation{
		netip.MustParseAddr("192.0.2.2"): {IP: netip.MustParseAddr("192.0.2.2"), Category: "residential", RiskScore: intPointer(37)},
	}}
	sink := &fakeEventSink{onEnqueue: func() {
		if store.saveCount.Load() != 1 {
			t.Error("event enqueued before SaveTick")
		}
	}}
	worker := testWorker(store, cipher, prober, lookup, sink)
	prepared, err := worker.Prepare(context.Background(), value)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := prepared.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if got := prober.max.Load(); got > 8 {
		t.Fatalf("maximum in-flight probes = %d, want <= 8", got)
	}
	if got := prober.next.Load(); got != 10 {
		t.Fatalf("probe calls = %d, want 10", got)
	}
	if got := len(lookup.calls); got != 1 {
		t.Fatalf("lookup calls = %d, want one distinct IP", got)
	}
	if len(store.saved) != 1 || len(sink.events) != 1 {
		t.Fatalf("saved ticks/events = %d/%d, want 1/1", len(store.saved), len(sink.events))
	}
	saved := store.saved[0]
	if saved.snapshot.SamplesTaken != 13 || saved.snapshot.ProbesOK != 30 || saved.snapshot.ProbesTotal != 34 || saved.snapshot.DistinctIPs != 2 {
		t.Fatalf("unexpected snapshot: %+v", saved.snapshot)
	}
	if len(saved.hits) != 1 || saved.hits[0].Hits != 10 {
		t.Fatalf("unexpected IP hits: %+v", saved.hits)
	}
	event := sink.events[0]
	if event.SampleSeq != 13 || event.PrimaryCategory != "residential" || event.PrimaryRisk != 37 || event.IPChanged != 1 {
		t.Fatalf("unexpected event: %+v", event)
	}
	for index, ip := range event.ProbeIPs {
		if got, want := ip.String(), "192.0.2.2"; got != want {
			t.Fatalf("probe result %d = %s, want %s", index, got, want)
		}
	}
}

func TestRunOrderedProbesPreservesLogicalIndexes(t *testing.T) {
	results := runOrderedProbes(context.Background(), 10, 8, func(_ context.Context, index int) ProbeResult {
		time.Sleep(time.Duration(10-index) * time.Millisecond)
		return ProbeResult{IP: netip.AddrFrom4([4]byte{192, 0, 2, byte(index + 1)})}
	})
	for index, result := range results {
		want := netip.AddrFrom4([4]byte{192, 0, 2, byte(index + 1)})
		if result.IP != want {
			t.Fatalf("result[%d] = %s, want %s", index, result.IP, want)
		}
	}
}

func TestWorkerPrepareReturnsInitializationErrorsSynchronously(t *testing.T) {
	store := newFakeSessionStore()
	value, cipher := encryptedSession(t, 1)
	worker := testWorker(store, cipher, fixedProber{}, &fakeLookup{}, &fakeEventSink{})

	badCredentials := value
	badCredentials.ProxyCiphertext = append([]byte(nil), value.ProxyCiphertext...)
	badCredentials.ProxyCiphertext[0] ^= 0xff
	if _, err := worker.Prepare(context.Background(), badCredentials); err == nil {
		t.Fatal("Prepare bad credentials = nil")
	}

	worker.dialerFactory = func(string, time.Duration) (proxy.ContextDialer, error) {
		return nil, errors.New("dialer rejected URL")
	}
	if _, err := worker.Prepare(context.Background(), value); err == nil {
		t.Fatal("Prepare dialer error = nil")
	}

	worker.dialerFactory = func(string, time.Duration) (proxy.ContextDialer, error) { return noopDialer{}, nil }
	store.ipsErr = errors.New("postgres unavailable")
	if _, err := worker.Prepare(context.Background(), value); err == nil {
		t.Fatal("Prepare SessionIPs error = nil")
	}
}

func TestPreparedWorkerRunSamplesImmediatelyThenOnCadence(t *testing.T) {
	store := newFakeSessionStore()
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	clock := &manualClock{now: time.Now(), ticks: make(chan time.Time, 1)}
	worker := testWorker(store, cipher, fixedProber{result: ProbeResult{IP: netip.MustParseAddr("192.0.2.8")}}, &fakeLookup{}, &fakeEventSink{})
	worker.clock = clock
	prepared, err := worker.Prepare(context.Background(), value)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { prepared.Run(ctx); close(done) }()
	waitForCount(t, &store.saveCount, 1)
	clock.ticks <- clock.now.Add(value.Cadence)
	waitForCount(t, &store.saveCount, 2)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after cancellation")
	}
}

func TestPreparedWorkerTickRetainsPartialEnrichmentAndDoesNotPublishFailedSave(t *testing.T) {
	store := newFakeSessionStore()
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	ip := netip.MustParseAddr("192.0.2.2")
	lookup := &fakeLookup{reputations: map[netip.Addr]session.Reputation{ip: {IP: ip, Category: "datacenter", RiskScore: intPointer(88)}}, err: errors.New("provider unavailable")}
	sink := &fakeEventSink{}
	worker := testWorker(store, cipher, fixedProber{result: ProbeResult{IP: ip, RTT: time.Millisecond}}, lookup, sink)
	prepared, err := worker.Prepare(context.Background(), value)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	store.saveErr = errors.New("postgres unavailable")
	if err := prepared.tick(context.Background()); err == nil {
		t.Fatal("tick error = nil, want SaveTick error")
	}
	if len(sink.events) != 0 || prepared.sampleSeq != 0 || len(prepared.seen) != 0 {
		t.Fatalf("failed save published state: events=%d seq=%d seen=%d", len(sink.events), prepared.sampleSeq, len(prepared.seen))
	}
	store.saveErr = nil
	if err := prepared.tick(context.Background()); err != nil {
		t.Fatalf("retry tick: %v", err)
	}
	if got := sink.events[0]; got.PrimaryCategory != "datacenter" || got.PrimaryRisk != 88 || got.SampleSeq != 1 {
		t.Fatalf("partial enrichment not retained: %+v", got)
	}
}

func TestPreparedWorkerTickUsesUnknownWhenEnrichmentFailsWithoutPartialData(t *testing.T) {
	store := newFakeSessionStore()
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	ip := netip.MustParseAddr("192.0.2.9")
	sink := &fakeEventSink{}
	worker := testWorker(store, cipher, fixedProber{result: ProbeResult{IP: ip}}, &fakeLookup{err: errors.New("lookup failed")}, sink)
	prepared, err := worker.Prepare(context.Background(), value)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sink.events[0].PrimaryCategory; got != "unknown" {
		t.Fatalf("primary category = %q, want unknown", got)
	}
}

func TestPreparedWorkerCountsPersistedEventsRejectedBySink(t *testing.T) {
	store := newFakeSessionStore()
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	sink := &fakeEventSink{reject: true}
	worker := testWorker(store, cipher, fixedProber{result: ProbeResult{IP: netip.MustParseAddr("192.0.2.10")}}, &fakeLookup{}, sink)
	prepared, err := worker.Prepare(context.Background(), value)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := worker.DroppedEvents(); got != 1 {
		t.Fatalf("dropped events = %d, want 1", got)
	}
	if store.saveCount.Load() != 1 {
		t.Fatal("rejected ClickHouse event was not persisted to Postgres first")
	}
}

func TestPreparedWorkerRunHonorsPersistedCapsAndCancellation(t *testing.T) {
	t.Run("max samples after persisted tick", func(t *testing.T) {
		store := newFakeSessionStore()
		value, cipher := encryptedSession(t, 1)
		max := 1
		value.MaxSamples = &max
		store.sessions[value.ID] = value
		worker := testWorker(store, cipher, fixedProber{result: ProbeResult{IP: netip.MustParseAddr("192.0.2.3")}}, &fakeLookup{}, &fakeEventSink{})
		prepared, err := worker.Prepare(context.Background(), value)
		if err != nil {
			t.Fatal(err)
		}
		prepared.Run(context.Background())
		if store.finishCount.Load() != 1 || store.saveCount.Load() != 1 {
			t.Fatalf("finish/save = %d/%d", store.finishCount.Load(), store.saveCount.Load())
		}
	})

	t.Run("persisted duration cap before first tick", func(t *testing.T) {
		store := newFakeSessionStore()
		value, cipher := encryptedSession(t, 1)
		started := time.Unix(100, 0)
		maximum := 10 * time.Second
		value.StartedAt, value.MaxDuration = &started, &maximum
		store.sessions[value.ID] = value
		worker := testWorker(store, cipher, fixedProber{}, &fakeLookup{}, &fakeEventSink{})
		worker.clock = fixedClock{now: started.Add(maximum)}
		prepared, err := worker.Prepare(context.Background(), value)
		if err != nil {
			t.Fatal(err)
		}
		prepared.Run(context.Background())
		if store.finishCount.Load() != 1 || store.saveCount.Load() != 0 {
			t.Fatalf("finish/save = %d/%d", store.finishCount.Load(), store.saveCount.Load())
		}
	})

	t.Run("root cancellation preserves running status", func(t *testing.T) {
		store := newFakeSessionStore()
		value, cipher := encryptedSession(t, 1)
		store.sessions[value.ID] = value
		worker := testWorker(store, cipher, fixedProber{result: ProbeResult{IP: netip.MustParseAddr("192.0.2.4")}}, &fakeLookup{}, &fakeEventSink{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		prepared, err := worker.Prepare(context.Background(), value)
		if err != nil {
			t.Fatal(err)
		}
		prepared.Run(ctx)
		if store.finishCount.Load() != 0 || store.stopCount.Load() != 0 {
			t.Fatalf("cancellation changed status")
		}
	})
}

func TestPreparedWorkerRunRetriesFinishWithoutFurtherSampling(t *testing.T) {
	store := newFakeSessionStore()
	value, cipher := encryptedSession(t, 1)
	maximum := 1
	value.MaxSamples = &maximum
	store.sessions[value.ID] = value
	store.finishErrs = []error{errors.New("postgres temporarily unavailable"), nil}
	clock := &manualClock{
		now:   time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC),
		ticks: make(chan time.Time, 1), afterCalls: make(chan time.Duration, 2),
	}
	prober := &orderedProber{}
	worker := testWorker(store, cipher, prober, &fakeLookup{}, &fakeEventSink{})
	worker.clock = clock
	prepared, err := worker.Prepare(context.Background(), value)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { prepared.Run(context.Background()); close(done) }()

	select {
	case <-clock.afterCalls:
	case <-done:
		t.Fatal("Run exited after the first failed Finish instead of remaining supervised")
	case <-time.After(time.Second):
		t.Fatal("Run neither scheduled a Finish retry nor exited")
	}
	if got := store.finishAttempts.Load(); got != 1 {
		t.Fatalf("Finish attempts before retry = %d, want 1", got)
	}
	if got := store.saveCount.Load(); got != 1 || prober.next.Load() != 1 {
		t.Fatalf("saved ticks/probes before retry = %d/%d, want 1/1", got, prober.next.Load())
	}

	clock.ticks <- clock.now.Add(time.Second)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after Finish retry succeeded")
	}
	if got := store.finishAttempts.Load(); got != 2 || store.finishCount.Load() != 1 {
		t.Fatalf("Finish attempts/successes = %d/%d, want 2/1", got, store.finishCount.Load())
	}
	if got := store.saveCount.Load(); got != 1 || prober.next.Load() != 1 {
		t.Fatalf("Finish retry performed additional ticks/probes = %d/%d", got, prober.next.Load())
	}
}

func TestPreparedWorkerRunPersistentFinishFailureWaitsForRootCancellation(t *testing.T) {
	store := newFakeSessionStore()
	value, cipher := encryptedSession(t, 1)
	maximum := 1
	value.MaxSamples = &maximum
	store.sessions[value.ID] = value
	store.finishErr = errors.New("postgres unavailable")
	clock := &manualClock{
		now:   time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC),
		ticks: make(chan time.Time, 1), afterCalls: make(chan time.Duration, 3),
	}
	prober := &orderedProber{}
	worker := testWorker(store, cipher, prober, &fakeLookup{}, &fakeEventSink{})
	worker.clock = clock
	prepared, err := worker.Prepare(context.Background(), value)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { prepared.Run(ctx); close(done) }()

	select {
	case <-clock.afterCalls:
	case <-done:
		t.Fatal("Run exited after persistent Finish failure")
	case <-time.After(time.Second):
		t.Fatal("Run did not schedule its first Finish retry")
	}
	clock.ticks <- clock.now.Add(time.Second)
	select {
	case <-clock.afterCalls:
	case <-done:
		t.Fatal("Run exited after the second Finish failure")
	case <-time.After(time.Second):
		t.Fatal("Run did not schedule its second Finish retry")
	}
	if got := store.saveCount.Load(); got != 1 || prober.next.Load() != 1 {
		t.Fatalf("persistent Finish retries performed additional ticks/probes = %d/%d", got, prober.next.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after root cancellation")
	}
	stored, err := store.SessionByID(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != session.StatusRunning || store.finishCount.Load() != 0 {
		t.Fatalf("persistent Finish cancellation changed durable state to %q with %d successes", stored.Status, store.finishCount.Load())
	}
}

type savedTick struct {
	snapshot session.Snapshot
	hits     []session.IPHit
}

type fakeSessionStore struct {
	mu                                sync.Mutex
	sessions                          map[uuid.UUID]session.Session
	ips                               map[uuid.UUID][]session.IPRecord
	saved                             []savedTick
	operations                        []string
	saveErr, sessionErr, ipsErr       error
	sessionErrs                       []error
	finishErr                         error
	finishErrs                        []error
	stopErr                           error
	stopErrs                          []error
	rejectCanceledStop                bool
	ipsStarted, ipsRelease            chan struct{}
	ipsStartOnce                      sync.Once
	runningStarted, runningRelease    chan struct{}
	runningStartOnce                  sync.Once
	stopStarted, stopRelease          chan struct{}
	stopStartOnce                     sync.Once
	saveCount, stopCount, finishCount atomic.Int64
	finishAttempts                    atomic.Int64
	stopAttempts                      atomic.Int64
	sessionAttempts                   atomic.Int64
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{sessions: map[uuid.UUID]session.Session{}, ips: map[uuid.UUID][]session.IPRecord{}}
}
func (s *fakeSessionStore) Create(_ context.Context, v session.Session) (session.Session, error) {
	s.sessions[v.ID] = v
	return v, nil
}
func (s *fakeSessionStore) Sessions(context.Context) ([]session.Session, error) { return nil, nil }
func (s *fakeSessionStore) SessionByID(_ context.Context, id uuid.UUID) (session.Session, error) {
	s.sessionAttempts.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessionErrs) > 0 {
		err := s.sessionErrs[0]
		s.sessionErrs = s.sessionErrs[1:]
		if err != nil {
			return session.Session{}, err
		}
	}
	if s.sessionErr != nil {
		return session.Session{}, s.sessionErr
	}
	v, ok := s.sessions[id]
	if !ok {
		return session.Session{}, session.ErrNotFound
	}
	return v, nil
}
func (s *fakeSessionStore) RunningSessions(context.Context) ([]session.Session, error) {
	s.mu.Lock()
	var result []session.Session
	for _, v := range s.sessions {
		if v.Status == session.StatusRunning {
			result = append(result, v)
		}
	}
	s.mu.Unlock()
	if s.runningStarted != nil {
		s.runningStartOnce.Do(func() { close(s.runningStarted) })
	}
	if s.runningRelease != nil {
		<-s.runningRelease
	}
	return result, nil
}
func (s *fakeSessionStore) Stop(ctx context.Context, id uuid.UUID, _ time.Time) error {
	s.stopAttempts.Add(1)
	if s.stopStarted != nil {
		s.stopStartOnce.Do(func() { close(s.stopStarted) })
	}
	if s.stopRelease != nil {
		<-s.stopRelease
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rejectCanceledStop && ctx.Err() != nil {
		return ctx.Err()
	}
	if len(s.stopErrs) > 0 {
		err := s.stopErrs[0]
		s.stopErrs = s.stopErrs[1:]
		if err != nil {
			return err
		}
	}
	if s.stopErr != nil {
		return s.stopErr
	}
	v, ok := s.sessions[id]
	if !ok || v.Status != session.StatusRunning {
		return session.ErrNotRunning
	}
	v.Status = session.StatusStopped
	s.sessions[id] = v
	s.stopCount.Add(1)
	s.operations = append(s.operations, "stop")
	return nil
}
func (s *fakeSessionStore) Finish(_ context.Context, id uuid.UUID, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishAttempts.Add(1)
	if len(s.finishErrs) > 0 {
		err := s.finishErrs[0]
		s.finishErrs = s.finishErrs[1:]
		if err != nil {
			return err
		}
	}
	if s.finishErr != nil {
		return s.finishErr
	}
	v, ok := s.sessions[id]
	if !ok || v.Status != session.StatusRunning {
		return session.ErrNotRunning
	}
	v.Status = session.StatusFinished
	s.sessions[id] = v
	s.finishCount.Add(1)
	return nil
}
func (s *fakeSessionStore) Reenable(context.Context, uuid.UUID, time.Time) error { return nil }
func (s *fakeSessionStore) Delete(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	s.operations = append(s.operations, "pg-delete")
	return nil
}
func (s *fakeSessionStore) SaveTick(_ context.Context, id uuid.UUID, snapshot session.Snapshot, hits []session.IPHit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveCount.Add(1)
	if s.saveErr != nil {
		return s.saveErr
	}
	s.saved = append(s.saved, savedTick{snapshot: snapshot, hits: append([]session.IPHit(nil), hits...)})
	v := s.sessions[id]
	v.Snapshot = snapshot
	s.sessions[id] = v
	return nil
}
func (s *fakeSessionStore) SessionIPs(_ context.Context, id uuid.UUID) ([]session.IPRecord, error) {
	if s.ipsStarted != nil {
		s.ipsStartOnce.Do(func() { close(s.ipsStarted) })
	}
	if s.ipsRelease != nil {
		<-s.ipsRelease
	}
	if s.ipsErr != nil {
		return nil, s.ipsErr
	}
	return append([]session.IPRecord(nil), s.ips[id]...), nil
}
func (s *fakeSessionStore) ReputationByIP(context.Context, netip.Addr) (session.Reputation, bool, error) {
	return session.Reputation{}, false, nil
}
func (s *fakeSessionStore) SaveReputation(context.Context, session.Reputation) error { return nil }

type fakeEventSink struct {
	mu         sync.Mutex
	events     []ch.Event
	operations *[]string
	onEnqueue  func()
	flushErr   error
	reject     bool
}

func (s *fakeEventSink) Enqueue(event ch.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.onEnqueue != nil {
		s.onEnqueue()
	}
	if s.reject {
		return false
	}
	s.events = append(s.events, event)
	return true
}
func (s *fakeEventSink) Flush(context.Context) error {
	if s.operations != nil {
		*s.operations = append(*s.operations, "flush")
	}
	return s.flushErr
}

type fakeLookup struct {
	mu          sync.Mutex
	calls       []netip.Addr
	reputations map[netip.Addr]session.Reputation
	err         error
}

func (l *fakeLookup) Lookup(_ context.Context, ip netip.Addr) (session.Reputation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, ip)
	return l.reputations[ip], l.err
}

type fixedProber struct{ result ProbeResult }

func (p fixedProber) Probe(context.Context, proxy.ContextDialer, string, time.Duration) ProbeResult {
	return p.result
}

type orderedProber struct{ next, inFlight, max atomic.Int64 }

func (p *orderedProber) Probe(context.Context, proxy.ContextDialer, string, time.Duration) ProbeResult {
	in := p.inFlight.Add(1)
	for {
		old := p.max.Load()
		if in <= old || p.max.CompareAndSwap(old, in) {
			break
		}
	}
	index := p.next.Add(1) - 1
	time.Sleep(time.Duration(10-index%3) * time.Millisecond)
	p.inFlight.Add(-1)
	return ProbeResult{IP: netip.MustParseAddr("192.0.2.2"), Country: "DE", RTT: time.Duration(index+1) * time.Millisecond}
}

type noopDialer struct{}

func (noopDialer) Dial(string, string) (net.Conn, error) { return nil, errors.New("unused") }
func (noopDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("unused")
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time                     { return c.now }
func (fixedClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

type manualClock struct {
	now        time.Time
	ticks      chan time.Time
	afterCalls chan time.Duration
}

func (c *manualClock) Now() time.Time { return c.now }
func (c *manualClock) After(delay time.Duration) <-chan time.Time {
	if c.afterCalls != nil {
		c.afterCalls <- delay
	}
	return c.ticks
}

func waitForCount(t *testing.T, count *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for count.Load() < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := count.Load(); got != want {
		t.Fatalf("count = %d, want %d", got, want)
	}
}

func encryptedSession(t *testing.T, probes int) (session.Session, *appcrypto.Cipher) {
	t.Helper()
	cipher, err := appcrypto.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, err := cipher.Encrypt("socks5://user:pass@example.com:1080")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	return session.Session{ID: uuid.New(), ProxyCiphertext: ciphertext, ProxyNonce: nonce, Status: session.StatusRunning, Cadence: time.Hour, ProbesPerSample: probes, ProbeTarget: "https://example.test", DialTimeout: time.Second, StartedAt: &started}, cipher
}

func testWorker(store session.Store, cipher *appcrypto.Cipher, prober Prober, lookup ReputationLookup, sink EventSink) *Worker {
	w := NewWorker(store, cipher, prober, lookup, sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.dialerFactory = func(string, time.Duration) (proxy.ContextDialer, error) { return noopDialer{}, nil }
	w.clock = realClock{}
	return w
}

func intPointer(v int) *int { return &v }
