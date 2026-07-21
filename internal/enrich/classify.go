package enrich

import "strings"

const (
	CategoryMobile      = "mobile"
	CategoryResidential = "residential"
	CategoryDatacenter  = "datacenter"
	CategoryUnknown     = "unknown"
)

var datacenterASNTokens = [...]string{
	"amazon",
	"aws",
	"google cloud",
	"microsoft",
	"azure",
	"digitalocean",
	"linode",
	"vultr",
	"ovh",
	"hetzner",
	"hostrush",
	"database mart",
	"hosting",
	"cloud",
	"colo",
	"datacenter",
	"data center",
	"server",
}

// Classify derives a deterministic category from merged provider evidence.
// Datacenter evidence intentionally takes precedence over mobile evidence.
func Classify(partial Partial) string {
	if !partial.HadSignal {
		return CategoryUnknown
	}

	proxyCheckType := strings.ToLower(strings.TrimSpace(partial.ProxyCheckType))
	if isDatacenterType(proxyCheckType) || hasDatacenterASN(partial.ASN) {
		return CategoryDatacenter
	}

	if (partial.IsMobile != nil && *partial.IsMobile) || proxyCheckType == "wireless" {
		return CategoryMobile
	}

	return CategoryResidential
}

func isDatacenterType(proxyCheckType string) bool {
	switch proxyCheckType {
	case "dch", "business", "hosting", "corporate", "education":
		return true
	default:
		return false
	}
}

func hasDatacenterASN(asn string) bool {
	normalized := strings.ToLower(asn)
	for _, token := range datacenterASNTokens {
		if strings.Contains(normalized, token) {
			return true
		}
	}
	return false
}
