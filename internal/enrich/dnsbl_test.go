package enrich

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func TestDNSBLQueriesReversedIPv4AcrossZonesInStableOrder(t *testing.T) {
	t.Parallel()
	resolver := &fakeHostResolver{lookup: func(host string) ([]string, error) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}}
	provider := NewDNSBL(resolver)
	partial, err := provider.Lookup(context.Background(), providerTestIP)
	if err != nil {
		t.Fatal(err)
	}
	wantQueries := []string{
		"7.113.0.203.zen.spamhaus.org",
		"7.113.0.203.b.barracudacentral.org",
		"7.113.0.203.dnsbl.sorbs.net",
		"7.113.0.203.bl.spamcop.net",
		"7.113.0.203.dnsbl-1.uceprotect.net",
	}
	if !reflect.DeepEqual(resolver.queries, wantQueries) {
		t.Fatalf("queries = %#v, want %#v", resolver.queries, wantQueries)
	}
	assertBoolPointer(t, "DNSBLListed", partial.DNSBLListed, false)
	if !partial.HadSignal || partial.DNSBLDynamic || len(partial.DNSBLHits) != 0 || provider.Name() != "dnsbl" {
		t.Fatalf("partial=%#v Name=%q", partial, provider.Name())
	}
}

func TestDNSBLListedAnswerRecordsZone(t *testing.T) {
	t.Parallel()
	resolver := &fakeHostResolver{lookup: func(string) ([]string, error) {
		return []string{"127.0.0.2"}, nil
	}}
	provider := NewDNSBL(resolver)
	provider.zones = []string{"zen.spamhaus.org"}
	partial, err := provider.Lookup(context.Background(), providerTestIP)
	if err != nil {
		t.Fatal(err)
	}
	assertBoolPointer(t, "DNSBLListed", partial.DNSBLListed, true)
	if !reflect.DeepEqual(partial.DNSBLHits, []string{"zen.spamhaus.org"}) || !partial.HadSignal {
		t.Fatalf("partial=%#v", partial)
	}
}

func TestDNSBLSpamhausPBLIsDynamicButNotListed(t *testing.T) {
	t.Parallel()
	for _, answer := range []string{"127.0.0.10", "127.0.0.11"} {
		t.Run(answer, func(t *testing.T) {
			resolver := &fakeHostResolver{lookup: func(string) ([]string, error) {
				return []string{answer}, nil
			}}
			provider := NewDNSBL(resolver)
			provider.zones = []string{"zen.spamhaus.org"}
			partial, err := provider.Lookup(context.Background(), providerTestIP)
			if err != nil {
				t.Fatal(err)
			}
			assertBoolPointer(t, "DNSBLListed", partial.DNSBLListed, false)
			if !partial.DNSBLDynamic || len(partial.DNSBLHits) != 0 || !partial.HadSignal {
				t.Fatalf("partial=%#v", partial)
			}
		})
	}
}

func TestDNSBLRateLimitAnswerIsAnErrorNotAListing(t *testing.T) {
	t.Parallel()
	resolver := &fakeHostResolver{lookup: func(string) ([]string, error) {
		return []string{"127.255.255.1"}, nil
	}}
	provider := NewDNSBL(resolver)
	provider.zones = []string{"zen.spamhaus.org", "zone-ratelimited.test"}
	partial, err := provider.Lookup(context.Background(), providerTestIP)
	if err == nil || !strings.Contains(err.Error(), "zen.spamhaus.org") || !strings.Contains(err.Error(), "zone-ratelimited.test") {
		t.Fatalf("error = %v", err)
	}
	if partial.DNSBLListed != nil || len(partial.DNSBLHits) != 0 || partial.HadSignal {
		t.Fatalf("partial=%#v", partial)
	}
}

func TestDNSBLRejectsNonIPv4ResolverAnswer(t *testing.T) {
	t.Parallel()
	resolver := &fakeHostResolver{lookup: func(string) ([]string, error) {
		return []string{"2001:db8::1"}, nil
	}}
	provider := NewDNSBL(resolver)
	provider.zones = []string{"zone.test"}
	partial, err := provider.Lookup(context.Background(), providerTestIP)
	if err == nil || !strings.Contains(err.Error(), "invalid answer") {
		t.Fatalf("error = %v", err)
	}
	if partial.DNSBLListed != nil || partial.HadSignal || len(partial.DNSBLHits) != 0 {
		t.Fatalf("partial=%#v", partial)
	}
}

func TestDNSBLAllZoneErrorsLeaveListingUnknown(t *testing.T) {
	t.Parallel()
	resolver := &fakeHostResolver{lookup: func(string) ([]string, error) {
		return nil, errors.New("resolver unavailable")
	}}
	provider := NewDNSBL(resolver)
	provider.zones = []string{"zone-a.test", "zone-b.test"}
	partial, err := provider.Lookup(context.Background(), providerTestIP)
	if err == nil || !strings.Contains(err.Error(), "zone-a.test") || !strings.Contains(err.Error(), "zone-b.test") {
		t.Fatalf("error = %v", err)
	}
	if partial.DNSBLListed != nil || partial.HadSignal || len(partial.DNSBLHits) != 0 {
		t.Fatalf("all failures must leave listing unknown: %#v", partial)
	}
}

func TestDNSBLNXDOMAINPlusErrorIsAuthoritativeNotListed(t *testing.T) {
	t.Parallel()
	resolver := &fakeHostResolver{lookup: func(host string) ([]string, error) {
		if strings.HasSuffix(host, ".zone-none.test") {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return nil, errors.New("resolver unavailable")
	}}
	provider := NewDNSBL(resolver)
	provider.zones = []string{"zone-error.test", "zone-none.test"}
	partial, err := provider.Lookup(context.Background(), providerTestIP)
	if err == nil || !strings.Contains(err.Error(), "zone-error.test") {
		t.Fatalf("error = %v", err)
	}
	assertBoolPointer(t, "DNSBLListed", partial.DNSBLListed, false)
	if !partial.HadSignal || len(partial.DNSBLHits) != 0 {
		t.Fatalf("partial=%#v", partial)
	}
}

func TestDNSBLReturnsSortedPartialHitsAlongsideZoneErrors(t *testing.T) {
	t.Parallel()
	resolver := &fakeHostResolver{lookup: func(host string) ([]string, error) {
		switch {
		case strings.HasSuffix(host, ".zone-z.test"), strings.HasSuffix(host, ".zone-a.test"):
			return []string{"127.0.0.2"}, nil
		case strings.HasSuffix(host, ".zone-error.test"):
			return nil, errors.New("resolver unavailable")
		default:
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
	}}
	provider := NewDNSBL(resolver)
	provider.zones = []string{"zone-z.test", "zone-error.test", "zone-a.test", "zone-none.test"}
	partial, err := provider.Lookup(context.Background(), providerTestIP)
	if err == nil || !strings.Contains(err.Error(), "zone-error.test") || !strings.Contains(err.Error(), "resolver unavailable") {
		t.Fatalf("error = %v", err)
	}
	assertBoolPointer(t, "DNSBLListed", partial.DNSBLListed, true)
	if want := []string{"zone-a.test", "zone-z.test"}; !reflect.DeepEqual(partial.DNSBLHits, want) {
		t.Fatalf("hits = %#v, want %#v", partial.DNSBLHits, want)
	}
	if !partial.HadSignal {
		t.Fatal("successful zones must produce a signal despite a partial error")
	}
}

func TestDNSBLSkipsIPv6WithoutResolverCalls(t *testing.T) {
	t.Parallel()
	resolver := &fakeHostResolver{lookup: func(string) ([]string, error) {
		return nil, errors.New("must not be called")
	}}
	provider := NewDNSBL(resolver)
	partial, err := provider.Lookup(context.Background(), netip.MustParseAddr("2001:db8::7"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resolver.queries) != 0 || partial.HadSignal || partial.DNSBLListed != nil {
		t.Fatalf("queries=%#v partial=%#v", resolver.queries, partial)
	}
}

type fakeHostResolver struct {
	queries []string
	lookup  func(string) ([]string, error)
}

func (r *fakeHostResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	r.queries = append(r.queries, host)
	return r.lookup(host)
}
