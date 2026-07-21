package enrich

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
)

var defaultDNSBLZones = []string{
	"zen.spamhaus.org",
	"b.barracudacentral.org",
	"dnsbl.sorbs.net",
	"bl.spamcop.net",
	"dnsbl-1.uceprotect.net",
}

type HostResolver interface {
	LookupHost(context.Context, string) ([]string, error)
}

type DNSBL struct {
	resolver HostResolver
	zones    []string
}

func NewDNSBL(resolver HostResolver) *DNSBL {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &DNSBL{resolver: resolver, zones: append([]string(nil), defaultDNSBLZones...)}
}

func (p *DNSBL) Name() string { return "dnsbl" }

func (p *DNSBL) Lookup(ctx context.Context, ip netip.Addr) (Partial, error) {
	ip = ip.Unmap()
	if !ip.Is4() {
		return Partial{}, nil
	}

	listed := false
	var partial Partial
	var errs []error
	reversed := reverseIPv4(ip)
	for _, zone := range p.zones {
		host := reversed + "." + zone
		answers, err := p.resolver.LookupHost(ctx, host)
		if err != nil {
			var dnsError *net.DNSError
			if errors.As(err, &dnsError) && dnsError.IsNotFound {
				partial.HadSignal = true
				continue
			}
			errs = append(errs, fmt.Errorf("%s: %w", zone, err))
			continue
		}

		zoneRatelimited := false
		for _, answer := range answers {
			answerIP, err := netip.ParseAddr(answer)
			if err != nil || !answerIP.Unmap().Is4() {
				errs = append(errs, fmt.Errorf("%s: invalid answer", zone))
				zoneRatelimited = true
				break
			}
			bytes := answerIP.Unmap().As4()
			if bytes[0] == 127 && bytes[1] == 255 && bytes[2] == 255 {
				errs = append(errs, fmt.Errorf("%s: provider ratelimit response", zone))
				zoneRatelimited = true
				break
			}
		}
		if zoneRatelimited {
			continue
		}

		partial.HadSignal = true
		zoneListed := false
		for _, answer := range answers {
			if zone == "zen.spamhaus.org" && (answer == "127.0.0.10" || answer == "127.0.0.11") {
				partial.DNSBLDynamic = true
				continue
			}
			zoneListed = true
		}
		if zoneListed {
			listed = true
			partial.DNSBLHits = append(partial.DNSBLHits, zone)
		}
	}
	if partial.HadSignal {
		partial.DNSBLListed = &listed
	}
	sort.Strings(partial.DNSBLHits)
	return partial, errors.Join(errs...)
}

func reverseIPv4(ip netip.Addr) string {
	bytes := ip.As4()
	return strings.Join([]string{
		fmt.Sprint(bytes[3]), fmt.Sprint(bytes[2]), fmt.Sprint(bytes[1]), fmt.Sprint(bytes[0]),
	}, ".")
}
