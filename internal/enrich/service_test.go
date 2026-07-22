package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/timo972/proxy-sampler/internal/session"
)

func TestServiceFreshCacheBypassesProvidersAndSave(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	cached := session.Reputation{IP: providerTestIP, Country: "cached", RefreshedAt: now.Add(-23 * time.Hour)}
	store := &serviceFakeStore{cached: cached, found: true}
	provider := &serviceFakeProvider{name: "never"}
	service := newTestService(t, store, []Provider{provider}, 24*time.Hour, semaphore.NewWeighted(1), now)

	got, err := service.Lookup(context.Background(), providerTestIP)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, cached) {
		t.Fatalf("Lookup() = %#v, want cached %#v", got, cached)
	}
	if provider.calls.Load() != 0 || store.saveCalls() != 0 {
		t.Fatalf("provider calls=%d save calls=%d", provider.calls.Load(), store.saveCalls())
	}
}

func TestServiceStaleCacheRefreshesAndSaves(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	firstSeen := now.Add(-7 * 24 * time.Hour)
	store := &serviceFakeStore{
		cached: session.Reputation{IP: providerTestIP, Country: "old", FirstSeen: firstSeen, RefreshedAt: now.Add(-25 * time.Hour)},
		found:  true,
	}
	provider := &serviceFakeProvider{name: "geo", partial: Partial{Country: "new", HadSignal: true}}
	service := newTestService(t, store, []Provider{provider}, 24*time.Hour, semaphore.NewWeighted(1), now)

	got, err := service.Lookup(context.Background(), providerTestIP)
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 1 || store.saveCalls() != 1 || got.Country != "new" || !got.RefreshedAt.Equal(now) || !got.FirstSeen.Equal(firstSeen) {
		t.Fatalf("calls=%d saves=%d reputation=%#v", provider.calls.Load(), store.saveCalls(), got)
	}
	if saved := store.lastSaved(); !saved.FirstSeen.Equal(firstSeen) {
		t.Fatalf("saved FirstSeen = %v, want %v", saved.FirstSeen, firstSeen)
	}
}

func TestServiceSharedSemaphoreBoundsConcurrentMissesAcrossServices(t *testing.T) {
	t.Parallel()
	sharedSemaphore := semaphore.NewWeighted(2)
	provider := newBlockingServiceProvider("blocking")
	storeA := &serviceFakeStore{}
	storeB := &serviceFakeStore{}
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	serviceA := newTestService(t, storeA, []Provider{provider}, 24*time.Hour, sharedSemaphore, now)
	serviceB := newTestService(t, storeB, []Provider{provider}, 24*time.Hour, sharedSemaphore, now)

	services := []*Service{serviceA, serviceB, serviceA, serviceB}
	errs := make(chan error, len(services))
	for index, service := range services {
		go func(service *Service, index int) {
			ip := netip.AddrFrom4([4]byte{203, 0, 113, byte(10 + index)})
			_, err := service.Lookup(context.Background(), ip)
			errs <- err
		}(service, index)
	}

	for range 2 {
		select {
		case <-provider.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for calls to enter provider")
		}
	}
	select {
	case <-provider.entered:
		t.Fatal("a third lookup passed the shared concurrency bound")
	case <-time.After(75 * time.Millisecond):
	}
	close(provider.release)
	for range services {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := provider.maxActive.Load(); got != 2 {
		t.Fatalf("maximum active provider calls = %d, want 2", got)
	}
}

func TestServiceCoalescesSimultaneousSameIPMisses(t *testing.T) {
	const callers = 8
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	store := newConcurrentServiceStore(callers)
	provider := newBlockingResultProvider("geo", Partial{Country: "DE", HadSignal: true})
	secondary := &serviceFakeProvider{name: "secondary", partial: Partial{City: "Frankfurt", HadSignal: true}}
	service := newTestService(t, store, []Provider{provider, secondary}, 24*time.Hour, semaphore.NewWeighted(4), now)
	t.Cleanup(provider.releaseAll)

	type lookupResult struct {
		reputation session.Reputation
		err        error
	}
	results := make(chan lookupResult, callers)
	for range callers {
		go func() {
			reputation, err := service.Lookup(context.Background(), providerTestIP)
			results <- lookupResult{reputation: reputation, err: err}
		}()
	}

	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("same-IP provider leader did not start")
	}
	select {
	case <-provider.entered:
		t.Fatal("a simultaneous same-IP follower ran a duplicate provider stack")
	case <-time.After(75 * time.Millisecond):
	}
	provider.releaseAll()
	for range callers {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.reputation.Country != "DE" || result.reputation.City != "Frankfurt" || result.reputation.IP != providerTestIP {
			t.Fatalf("coalesced result = %#v", result.reputation)
		}
	}
	if provider.calls.Load() != 1 || secondary.calls.Load() != 1 || store.saveCalls() != 1 {
		t.Fatalf("provider stack/save calls = %d/%d/%d, want 1/1/1", provider.calls.Load(), secondary.calls.Load(), store.saveCalls())
	}
	assertServiceKeysEmpty(t, service)
}

func TestServiceCoalescesIPv4MappedAndNativeAddresses(t *testing.T) {
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	store := newConcurrentServiceStore(2)
	provider := newBlockingResultProvider("geo", Partial{Country: "DE", HadSignal: true})
	service := newTestService(t, store, []Provider{provider}, time.Hour, semaphore.NewWeighted(2), now)
	t.Cleanup(provider.releaseAll)

	results := make(chan session.Reputation, 2)
	errs := make(chan error, 2)
	for _, ip := range []netip.Addr{providerTestIP, netip.MustParseAddr("::ffff:203.0.113.7")} {
		go func(ip netip.Addr) {
			reputation, err := service.Lookup(context.Background(), ip)
			results <- reputation
			errs <- err
		}(ip)
	}
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("mapped/native enrichment leader did not start")
	}
	select {
	case <-provider.entered:
		t.Fatal("mapped and native IPv4 forms ran separate provider stacks")
	case <-time.After(75 * time.Millisecond):
	}
	provider.releaseAll()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if result := <-results; result.IP != providerTestIP || result.Country != "DE" {
			t.Fatalf("mapped/native coalesced result = %#v", result)
		}
	}
	if provider.calls.Load() != 1 || store.saveCalls() != 1 {
		t.Fatalf("mapped/native provider/save calls = %d/%d, want 1/1", provider.calls.Load(), store.saveCalls())
	}
	assertServiceKeysEmpty(t, service)
}

func TestServiceCanceledSameIPWaiterDoesNotAffectLeader(t *testing.T) {
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	store := newConcurrentServiceStore(0)
	provider := newBlockingResultProvider("geo", Partial{Country: "DE", HadSignal: true})
	service := newTestService(t, store, []Provider{provider}, time.Hour, semaphore.NewWeighted(1), now)
	t.Cleanup(provider.releaseAll)

	leaderResult := make(chan error, 1)
	go func() {
		_, err := service.Lookup(context.Background(), providerTestIP)
		leaderResult <- err
	}()
	<-provider.entered
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterResult := make(chan error, 1)
	go func() {
		_, err := service.Lookup(waiterCtx, providerTestIP)
		waiterResult <- err
	}()
	cancelWaiter()
	select {
	case err := <-waiterResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled same-IP waiter deadlocked")
	}
	provider.releaseAll()
	if err := <-leaderResult; err != nil {
		t.Fatal(err)
	}
	got, err := service.Lookup(context.Background(), providerTestIP)
	if err != nil || got.Country != "DE" {
		t.Fatalf("fresh lookup after canceled waiter = %#v, %v", got, err)
	}
	if provider.calls.Load() != 1 || store.saveCalls() != 1 {
		t.Fatalf("provider/save calls after canceled waiter = %d/%d, want 1/1", provider.calls.Load(), store.saveCalls())
	}
	assertServiceKeysEmpty(t, service)
}

func TestServiceCanceledLeaderReleasesSameIPFollower(t *testing.T) {
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	store := newConcurrentServiceStore(0)
	provider := &cancelThenSucceedProvider{firstEntered: make(chan struct{}), partial: Partial{Country: "DE", HadSignal: true}}
	service := newTestService(t, store, []Provider{provider}, time.Hour, semaphore.NewWeighted(1), now)
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderResult := make(chan error, 1)
	go func() {
		_, err := service.Lookup(leaderCtx, providerTestIP)
		leaderResult <- err
	}()
	<-provider.firstEntered
	followerResult := make(chan struct {
		reputation session.Reputation
		err        error
	}, 1)
	go func() {
		reputation, err := service.Lookup(context.Background(), providerTestIP)
		followerResult <- struct {
			reputation session.Reputation
			err        error
		}{reputation: reputation, err: err}
	}()
	cancelLeader()
	if err := <-leaderResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled leader error = %v, want context.Canceled", err)
	}
	select {
	case result := <-followerResult:
		if result.err != nil || result.reputation.Country != "DE" {
			t.Fatalf("follower after canceled leader = %#v, %v", result.reputation, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("same-IP follower deadlocked behind canceled leader")
	}
	if provider.calls.Load() != 2 || store.saveCalls() != 1 {
		t.Fatalf("provider/save calls after canceled leader = %d/%d, want 2/1", provider.calls.Load(), store.saveCalls())
	}
	assertServiceKeysEmpty(t, service)
}

func TestServiceProviderErrorStillSavesMergedFieldsAndClassification(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	risk := 88
	store := &serviceFakeStore{}
	providers := []Provider{
		&serviceFakeProvider{name: "geo", partial: Partial{
			ASN: "AS62633 HostRush", HadSignal: true,
			Raw: map[string]json.RawMessage{"geo": json.RawMessage(`{"asn":"AS62633 HostRush"}`)},
		}},
		&serviceFakeProvider{name: "risk", partial: Partial{
			RiskScore: &risk, HadSignal: true,
			Raw: map[string]json.RawMessage{"risk": json.RawMessage(`{"risk":88}`)},
		}, err: errors.New("quota exhausted")},
	}
	service := newTestService(t, store, providers, 24*time.Hour, semaphore.NewWeighted(1), now)

	got, err := service.Lookup(context.Background(), providerTestIP)
	if err == nil || !strings.Contains(err.Error(), "risk: quota exhausted") {
		t.Fatalf("error = %v", err)
	}
	if got.ASN != "AS62633 HostRush" || got.RiskScore == nil || *got.RiskScore != 88 || got.Category != CategoryDatacenter {
		t.Fatalf("reputation=%#v", got)
	}
	if !got.FirstSeen.Equal(now) || !got.RefreshedAt.Equal(now) {
		t.Fatalf("new miss timestamps = FirstSeen %v RefreshedAt %v, want %v", got.FirstSeen, got.RefreshedAt, now)
	}
	saved := store.lastSaved()
	if saved.ASN != got.ASN || saved.Category != CategoryDatacenter || saved.RiskScore == nil || *saved.RiskScore != 88 {
		t.Fatalf("saved=%#v", saved)
	}
	if !saved.FirstSeen.Equal(now) || !saved.RefreshedAt.Equal(now) {
		t.Fatalf("saved new-miss timestamps = FirstSeen %v RefreshedAt %v, want %v", saved.FirstSeen, saved.RefreshedAt, now)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(saved.Raw, &raw); err != nil || len(raw) != 2 {
		t.Fatalf("raw=%s decode error=%v", saved.Raw, err)
	}
}

func TestServiceCacheReadFailureAbortsBeforeQuotaOrSave(t *testing.T) {
	t.Parallel()
	store := &serviceFakeStore{lookupErr: errors.New("database unavailable")}
	provider := &serviceFakeProvider{name: "never"}
	service := newTestService(t, store, []Provider{provider}, time.Hour, semaphore.NewWeighted(1), time.Now())

	_, err := service.Lookup(context.Background(), providerTestIP)
	if err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf("error = %v", err)
	}
	if provider.calls.Load() != 0 || store.saveCalls() != 0 {
		t.Fatalf("provider calls=%d save calls=%d", provider.calls.Load(), store.saveCalls())
	}
}

func TestServiceSaveFailureJoinsProviderErrorAndReleasesSemaphore(t *testing.T) {
	t.Parallel()
	store := &serviceFakeStore{saveErr: errors.New("database write failed")}
	provider := &serviceFakeProvider{name: "risk", err: errors.New("quota exhausted")}
	sem := semaphore.NewWeighted(1)
	service := newTestService(t, store, []Provider{provider}, time.Hour, sem, time.Now())

	_, err := service.Lookup(context.Background(), providerTestIP)
	if err == nil || !strings.Contains(err.Error(), "risk: quota exhausted") || !strings.Contains(err.Error(), "save reputation: database write failed") {
		t.Fatalf("joined error = %v", err)
	}
	if !sem.TryAcquire(1) {
		t.Fatal("service did not release semaphore after save failure")
	}
	sem.Release(1)
}

func TestServiceCanceledSemaphoreAcquireDoesNoProviderOrSaveWork(t *testing.T) {
	t.Parallel()
	store := &serviceFakeStore{}
	provider := &serviceFakeProvider{name: "never"}
	sem := semaphore.NewWeighted(1)
	if err := sem.Acquire(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	defer sem.Release(1)
	service := newTestService(t, store, []Provider{provider}, time.Hour, sem, time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := service.Lookup(ctx, providerTestIP)
	if err == nil || !strings.Contains(err.Error(), "acquire enrichment slot") {
		t.Fatalf("error = %v", err)
	}
	if provider.calls.Load() != 0 || store.saveCalls() != 0 {
		t.Fatalf("provider calls=%d save calls=%d", provider.calls.Load(), store.saveCalls())
	}
}

func TestNewServiceRejectsNilSemaphore(t *testing.T) {
	t.Parallel()
	if _, err := NewService(&serviceFakeStore{}, nil, time.Hour, nil); err == nil || !strings.Contains(err.Error(), "semaphore") {
		t.Fatalf("error = %v", err)
	}
}

func TestDefaultProvidersHaveStableOrderAndSharedHardenedClient(t *testing.T) {
	t.Parallel()
	providers := DefaultProviders(nil, nil)
	var names []string
	for _, provider := range providers {
		names = append(names, provider.Name())
	}
	if want := []string{"ip-api", "proxycheck", "greynoise", "stopforumspam", "dnsbl"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("provider names = %#v, want %#v", names, want)
	}
	clients := []*http.Client{
		providers[0].(*IPAPI).client,
		providers[1].(*ProxyCheck).client,
		providers[2].(*GreyNoise).client,
		providers[3].(*StopForumSpam).client,
	}
	for index, client := range clients {
		if client != hardenedHTTPClient || client.Timeout != 10*time.Second {
			t.Fatalf("provider %d does not use shared hardened client", index)
		}
	}
}

func newTestService(t *testing.T, store session.Store, providers []Provider, ttl time.Duration, sem *semaphore.Weighted, now time.Time) *Service {
	t.Helper()
	service, err := NewService(store, providers, ttl, sem)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	return service
}

type serviceFakeStore struct {
	session.Store
	mu        sync.Mutex
	cached    session.Reputation
	found     bool
	lookupErr error
	saveErr   error
	saved     []session.Reputation
}

func (s *serviceFakeStore) ReputationByIP(context.Context, netip.Addr) (session.Reputation, bool, error) {
	return s.cached, s.found, s.lookupErr
}

func (s *serviceFakeStore) SaveReputation(_ context.Context, reputation session.Reputation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.saved = append(s.saved, reputation)
	return nil
}

func (s *serviceFakeStore) saveCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saved)
}

func (s *serviceFakeStore) lastSaved() session.Reputation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved[len(s.saved)-1]
}

type serviceFakeProvider struct {
	name    string
	partial Partial
	err     error
	calls   atomic.Int32
}

func (p *serviceFakeProvider) Name() string { return p.name }

func (p *serviceFakeProvider) Lookup(context.Context, netip.Addr) (Partial, error) {
	p.calls.Add(1)
	return p.partial, p.err
}

type blockingServiceProvider struct {
	name      string
	entered   chan struct{}
	release   chan struct{}
	active    atomic.Int32
	maxActive atomic.Int32
}

type concurrentServiceStore struct {
	session.Store
	mu             sync.Mutex
	values         map[netip.Addr]session.Reputation
	saves          int
	barrierWant    int
	barrierArrived int
	barrierRelease chan struct{}
}

func assertServiceKeysEmpty(t *testing.T, service *Service) {
	t.Helper()
	service.keyMu.Lock()
	defer service.keyMu.Unlock()
	if len(service.keys) != 0 {
		t.Fatalf("enrichment keyed coordination retained %d entries", len(service.keys))
	}
}

func newConcurrentServiceStore(barrierWant int) *concurrentServiceStore {
	store := &concurrentServiceStore{values: make(map[netip.Addr]session.Reputation), barrierWant: barrierWant}
	if barrierWant > 0 {
		store.barrierRelease = make(chan struct{})
	}
	return store
}

func (s *concurrentServiceStore) ReputationByIP(ctx context.Context, ip netip.Addr) (session.Reputation, bool, error) {
	s.mu.Lock()
	if s.barrierRelease != nil && s.barrierArrived < s.barrierWant {
		s.barrierArrived++
		if s.barrierArrived == s.barrierWant {
			close(s.barrierRelease)
		}
	}
	release := s.barrierRelease
	s.mu.Unlock()
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return session.Reputation{}, false, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[ip]
	return value, ok, nil
}

func (s *concurrentServiceStore) SaveReputation(ctx context.Context, reputation session.Reputation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[reputation.IP] = reputation
	s.saves++
	return nil
}

func (s *concurrentServiceStore) saveCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

type blockingResultProvider struct {
	name        string
	partial     Partial
	entered     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
	calls       atomic.Int32
}

func newBlockingResultProvider(name string, partial Partial) *blockingResultProvider {
	return &blockingResultProvider{name: name, partial: partial, entered: make(chan struct{}, 16), release: make(chan struct{})}
}

func (p *blockingResultProvider) Name() string { return p.name }

func (p *blockingResultProvider) Lookup(ctx context.Context, _ netip.Addr) (Partial, error) {
	p.calls.Add(1)
	p.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return Partial{}, ctx.Err()
	case <-p.release:
		return p.partial, nil
	}
}

func (p *blockingResultProvider) releaseAll() { p.releaseOnce.Do(func() { close(p.release) }) }

type cancelThenSucceedProvider struct {
	firstEntered chan struct{}
	partial      Partial
	calls        atomic.Int32
}

func (p *cancelThenSucceedProvider) Name() string { return "cancel-then-succeed" }

func (p *cancelThenSucceedProvider) Lookup(ctx context.Context, _ netip.Addr) (Partial, error) {
	if p.calls.Add(1) == 1 {
		close(p.firstEntered)
		<-ctx.Done()
		return Partial{}, ctx.Err()
	}
	return p.partial, nil
}

func newBlockingServiceProvider(name string) *blockingServiceProvider {
	return &blockingServiceProvider{name: name, entered: make(chan struct{}, 8), release: make(chan struct{})}
}

func (p *blockingServiceProvider) Name() string { return p.name }

func (p *blockingServiceProvider) Lookup(ctx context.Context, _ netip.Addr) (Partial, error) {
	active := p.active.Add(1)
	defer p.active.Add(-1)
	for {
		maximum := p.maxActive.Load()
		if active <= maximum || p.maxActive.CompareAndSwap(maximum, active) {
			break
		}
	}
	p.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return Partial{}, ctx.Err()
	case <-p.release:
		return Partial{HadSignal: true}, nil
	}
}
