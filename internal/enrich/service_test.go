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
	store := &serviceFakeStore{
		cached: session.Reputation{IP: providerTestIP, Country: "old", RefreshedAt: now.Add(-25 * time.Hour)},
		found:  true,
	}
	provider := &serviceFakeProvider{name: "geo", partial: Partial{Country: "new", HadSignal: true}}
	service := newTestService(t, store, []Provider{provider}, 24*time.Hour, semaphore.NewWeighted(1), now)

	got, err := service.Lookup(context.Background(), providerTestIP)
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 1 || store.saveCalls() != 1 || got.Country != "new" || !got.RefreshedAt.Equal(now) {
		t.Fatalf("calls=%d saves=%d reputation=%#v", provider.calls.Load(), store.saveCalls(), got)
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
	saved := store.lastSaved()
	if saved.ASN != got.ASN || saved.Category != CategoryDatacenter || saved.RiskScore == nil || *saved.RiskScore != 88 {
		t.Fatalf("saved=%#v", saved)
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
	saved     []session.Reputation
}

func (s *serviceFakeStore) ReputationByIP(context.Context, netip.Addr) (session.Reputation, bool, error) {
	return s.cached, s.found, s.lookupErr
}

func (s *serviceFakeStore) SaveReputation(_ context.Context, reputation session.Reputation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
