package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/timo972/proxy-sampler/internal/session"
)

type Service struct {
	store     session.Store
	providers []Provider
	ttl       time.Duration
	sem       *semaphore.Weighted
	now       func() time.Time
}

// NewService builds a cache-aware enrichment service. sem must be the shared
// process-wide enrichment semaphore owned by the composition root.
func NewService(store session.Store, providers []Provider, ttl time.Duration, sem *semaphore.Weighted) (*Service, error) {
	if store == nil {
		return nil, errors.New("enrichment store is required")
	}
	if ttl <= 0 {
		return nil, errors.New("enrichment TTL must be positive")
	}
	if sem == nil {
		return nil, errors.New("process-wide enrichment semaphore is required")
	}
	return &Service{
		store: store, providers: append([]Provider(nil), providers...), ttl: ttl, sem: sem,
		now: time.Now,
	}, nil
}

// DefaultProviders returns the production provider stack in persistence-stable
// order. Nil dependencies select the shared hardened HTTP client and the
// process default DNS resolver.
func DefaultProviders(client *http.Client, resolver HostResolver) []Provider {
	client = providerHTTPClient(client)
	return []Provider{
		NewIPAPI(client),
		NewProxyCheck(client),
		NewGreyNoise(client),
		NewStopForumSpam(client),
		NewDNSBL(resolver),
	}
}

func (s *Service) Lookup(ctx context.Context, ip netip.Addr) (session.Reputation, error) {
	cached, ok, err := s.store.ReputationByIP(ctx, ip)
	if err != nil {
		return session.Reputation{}, fmt.Errorf("read reputation cache: %w", err)
	}
	if ok && s.now().Sub(cached.RefreshedAt) < s.ttl {
		return cached, nil
	}

	if err := s.sem.Acquire(ctx, 1); err != nil {
		return session.Reputation{}, fmt.Errorf("acquire enrichment slot: %w", err)
	}
	defer s.sem.Release(1)

	var merged Partial
	var errs []error
	for _, provider := range s.providers {
		partial, err := provider.Lookup(ctx, ip)
		merged.Merge(partial)
		if err != nil {
			if merged.Errors == nil {
				merged.Errors = make(map[string]string)
			}
			merged.Errors[provider.Name()] = err.Error()
			errs = append(errs, fmt.Errorf("%s: %w", provider.Name(), err))
		}
	}

	raw, err := marshalProviderRaw(merged.Raw)
	if err != nil {
		errs = append(errs, err)
	}
	refreshedAt := s.now()
	reputation := toReputation(ip, merged, Classify(merged), refreshedAt, raw)
	if err := s.store.SaveReputation(ctx, reputation); err != nil {
		errs = append(errs, fmt.Errorf("save reputation: %w", err))
		return session.Reputation{}, errors.Join(errs...)
	}
	return reputation, errors.Join(errs...)
}

func marshalProviderRaw(raw map[string]json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return json.RawMessage(`{}`), fmt.Errorf("encode provider payloads: %w", err)
	}
	return encoded, nil
}

func toReputation(ip netip.Addr, partial Partial, category string, refreshedAt time.Time, raw json.RawMessage) session.Reputation {
	return session.Reputation{
		IP: ip, Country: partial.Country, Region: partial.Region, City: partial.City,
		ISP: partial.ISP, ASN: partial.ASN, IsMobile: partial.IsMobile,
		IPAPIProxy: partial.IPAPIProxy, IPAPIHosting: partial.IPAPIHosting,
		ProxyCheckType: partial.ProxyCheckType, ProxyCheckProxy: partial.ProxyCheckProxy,
		RiskScore: partial.RiskScore, GreyNoiseClass: partial.GreyNoiseClass,
		SFSAppears: partial.SFSAppears, SFSFrequency: partial.SFSFrequency,
		DNSBLListed: partial.DNSBLListed, DNSBLHits: append([]string(nil), partial.DNSBLHits...),
		Category: category, Raw: raw, FirstSeen: refreshedAt, RefreshedAt: refreshedAt,
	}
}
