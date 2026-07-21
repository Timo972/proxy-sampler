package enrich

import (
	"context"
	"encoding/json"
	"net/netip"
	"time"
)

// Provider enriches an IP address with one provider's available signals.
type Provider interface {
	Name() string
	Lookup(ctx context.Context, ip netip.Addr) (Partial, error)
}

// Partial is the mergeable subset of reputation data returned by providers.
// Pointer-valued scalar fields distinguish an observed false or zero from a
// field a provider did not supply.
type Partial struct {
	Country string
	Region  string
	City    string
	ISP     string
	ASN     string

	IsMobile     *bool
	IPAPIProxy   *bool
	IPAPIHosting *bool

	ProxyCheckType  string
	ProxyCheckProxy *bool
	RiskScore       *int

	GreyNoiseClass string

	SFSAppears   *bool
	SFSFrequency *int

	DNSBLListed  *bool
	DNSBLHits    []string
	DNSBLDynamic bool

	Raw       map[string]json.RawMessage
	HadSignal bool
	Errors    map[string]string
}

// Merge incorporates the fields represented by incoming. Missing incoming
// fields leave the receiver unchanged.
func (p *Partial) Merge(incoming Partial) {
	mergeString(&p.Country, incoming.Country)
	mergeString(&p.Region, incoming.Region)
	mergeString(&p.City, incoming.City)
	mergeString(&p.ISP, incoming.ISP)
	mergeString(&p.ASN, incoming.ASN)

	mergeBool(&p.IsMobile, incoming.IsMobile)
	mergeBool(&p.IPAPIProxy, incoming.IPAPIProxy)
	mergeBool(&p.IPAPIHosting, incoming.IPAPIHosting)

	mergeString(&p.ProxyCheckType, incoming.ProxyCheckType)
	mergeBool(&p.ProxyCheckProxy, incoming.ProxyCheckProxy)
	mergeInt(&p.RiskScore, incoming.RiskScore)

	mergeString(&p.GreyNoiseClass, incoming.GreyNoiseClass)

	mergeBool(&p.SFSAppears, incoming.SFSAppears)
	mergeInt(&p.SFSFrequency, incoming.SFSFrequency)

	mergeBool(&p.DNSBLListed, incoming.DNSBLListed)
	p.DNSBLHits = unionStrings(p.DNSBLHits, incoming.DNSBLHits)
	p.DNSBLDynamic = p.DNSBLDynamic || incoming.DNSBLDynamic

	if len(incoming.Raw) > 0 {
		if p.Raw == nil {
			p.Raw = make(map[string]json.RawMessage, len(incoming.Raw))
		}
		for provider, payload := range incoming.Raw {
			p.Raw[provider] = payload
		}
	}

	p.HadSignal = p.HadSignal || incoming.HadSignal
	if len(incoming.Errors) > 0 {
		if p.Errors == nil {
			p.Errors = make(map[string]string, len(incoming.Errors))
		}
		for provider, message := range incoming.Errors {
			p.Errors[provider] = message
		}
	}
}

// Result is a classified, timestamped set of merged provider data.
type Result struct {
	Partial     Partial
	Category    string
	RefreshedAt time.Time
}

func mergeString(destination *string, incoming string) {
	if incoming != "" {
		*destination = incoming
	}
}

func mergeBool(destination **bool, incoming *bool) {
	if incoming != nil {
		*destination = incoming
	}
}

func mergeInt(destination **int, incoming *int) {
	if incoming != nil {
		*destination = incoming
	}
}

func unionStrings(existing, incoming []string) []string {
	if len(incoming) == 0 {
		return existing
	}

	seen := make(map[string]struct{}, len(existing)+len(incoming))
	result := make([]string, 0, len(existing)+len(incoming))
	for _, values := range [][]string{existing, incoming} {
		for _, value := range values {
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}
