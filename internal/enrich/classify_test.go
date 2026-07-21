package enrich

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		p    Partial
		want string
	}{
		{"asn hosting wins", Partial{ASN: "AS62633 HostRush", HadSignal: true}, "datacenter"},
		{"database mart", Partial{ASN: "AS401479 Database Mart, LLC", HadSignal: true}, "datacenter"},
		{"proxycheck dch", Partial{ProxyCheckType: "DCH", HadSignal: true}, "datacenter"},
		{"mobile flag", Partial{IsMobile: ptr(true), HadSignal: true}, "mobile"},
		{"wireless", Partial{ProxyCheckType: "Wireless", HadSignal: true}, "mobile"},
		{"ordinary isp", Partial{ASN: "AS7922 Comcast Cable", HadSignal: true}, "residential"},
		{"pbl dynamic", Partial{DNSBLDynamic: true, HadSignal: true}, "residential"},
		{"no successful lookup", Partial{}, "unknown"},
		{
			"datacenter evidence precedes mobile",
			Partial{ASN: "AS62633 HostRush", IsMobile: ptr(true), ProxyCheckType: "Wireless", HadSignal: true},
			"datacenter",
		},
		{
			"datacenter proxycheck type precedes mobile",
			Partial{IsMobile: ptr(true), ProxyCheckType: "Business", HadSignal: true},
			"datacenter",
		},
		{"false mobile remains residential", Partial{IsMobile: ptr(false), HadSignal: true}, "residential"},
		{"successful empty lookup", Partial{HadSignal: true}, "residential"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.p); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestClassifyDatacenterProxyCheckTypesCaseInsensitive(t *testing.T) {
	types := []string{"DCH", "Business", "Hosting", "Corporate", "Education"}

	for _, proxyType := range types {
		t.Run(proxyType, func(t *testing.T) {
			p := Partial{ProxyCheckType: mixedCase(proxyType), HadSignal: true}
			if got := Classify(p); got != "datacenter" {
				t.Fatalf("Classify(%q) = %q, want datacenter", p.ProxyCheckType, got)
			}
		})
	}
}

func TestClassifyDatacenterASNTokensCaseInsensitive(t *testing.T) {
	tokens := []string{
		"amazon", "aws", "google cloud", "microsoft", "azure", "digitalocean",
		"linode", "vultr", "ovh", "hetzner", "hostrush", "database mart",
		"hosting", "cloud", "colo", "datacenter", "data center", "server",
	}

	for _, token := range tokens {
		t.Run(token, func(t *testing.T) {
			p := Partial{ASN: "AS64500 " + mixedCase(token) + " services", HadSignal: true}
			if got := Classify(p); got != "datacenter" {
				t.Fatalf("Classify(%q) = %q, want datacenter", p.ASN, got)
			}
		})
	}
}

func TestMerge(t *testing.T) {
	dst := Partial{
		Country:         "DE",
		Region:          "Berlin",
		City:            "Berlin",
		ISP:             "Example ISP",
		ASN:             "AS64500 Example ISP",
		IsMobile:        ptr(false),
		IPAPIProxy:      ptr(false),
		IPAPIHosting:    ptr(false),
		ProxyCheckType:  "Residential",
		ProxyCheckProxy: ptr(false),
		RiskScore:       ptr(0),
		GreyNoiseClass:  "unknown",
		SFSAppears:      ptr(false),
		SFSFrequency:    ptr(0),
		DNSBLListed:     ptr(false),
		DNSBLHits:       []string{"zone-a", "zone-b"},
		Raw:             map[string]json.RawMessage{"ipapi": json.RawMessage(`{"old":true}`)},
		HadSignal:       true,
		Errors:          map[string]string{"old-provider": "old error"},
	}

	dst.Merge(Partial{
		Country:         "US",
		IsMobile:        ptr(true),
		IPAPIProxy:      ptr(true),
		IPAPIHosting:    ptr(true),
		ProxyCheckType:  "DCH",
		ProxyCheckProxy: ptr(true),
		RiskScore:       ptr(75),
		GreyNoiseClass:  "malicious",
		SFSAppears:      ptr(true),
		SFSFrequency:    ptr(12),
		DNSBLListed:     ptr(true),
		DNSBLHits:       []string{"zone-b", "zone-c", "zone-c"},
		DNSBLDynamic:    true,
		Raw: map[string]json.RawMessage{
			"proxycheck": json.RawMessage(`{"proxy":"yes"}`),
			"ipapi":      json.RawMessage(`{"new":true}`),
		},
		HadSignal: true,
		Errors:    map[string]string{"new-provider": "new error"},
	})

	if dst.Country != "US" || dst.Region != "Berlin" || dst.City != "Berlin" || dst.ISP != "Example ISP" || dst.ASN != "AS64500 Example ISP" {
		t.Fatalf("string fields merged incorrectly: %+v", dst)
	}
	assertPointerValue(t, "IsMobile", dst.IsMobile, true)
	assertPointerValue(t, "IPAPIProxy", dst.IPAPIProxy, true)
	assertPointerValue(t, "IPAPIHosting", dst.IPAPIHosting, true)
	assertPointerValue(t, "ProxyCheckProxy", dst.ProxyCheckProxy, true)
	assertPointerValue(t, "RiskScore", dst.RiskScore, 75)
	assertPointerValue(t, "SFSAppears", dst.SFSAppears, true)
	assertPointerValue(t, "SFSFrequency", dst.SFSFrequency, 12)
	assertPointerValue(t, "DNSBLListed", dst.DNSBLListed, true)
	if dst.ProxyCheckType != "DCH" || dst.GreyNoiseClass != "malicious" {
		t.Fatalf("provider string fields merged incorrectly: %+v", dst)
	}
	if !dst.DNSBLDynamic || !dst.HadSignal {
		t.Fatalf("boolean evidence was not retained: %+v", dst)
	}
	if want := []string{"zone-a", "zone-b", "zone-c"}; !reflect.DeepEqual(dst.DNSBLHits, want) {
		t.Fatalf("DNSBLHits = %#v, want %#v", dst.DNSBLHits, want)
	}
	if got := string(dst.Raw["ipapi"]); got != `{"new":true}` {
		t.Fatalf("Raw[ipapi] = %s, want incoming payload", got)
	}
	if got := string(dst.Raw["proxycheck"]); got != `{"proxy":"yes"}` {
		t.Fatalf("Raw[proxycheck] = %s, want incoming payload", got)
	}
	if got := dst.Errors["old-provider"]; got != "old error" {
		t.Fatalf("Errors[old-provider] = %q, want old error", got)
	}
	if got := dst.Errors["new-provider"]; got != "new error" {
		t.Fatalf("Errors[new-provider] = %q, want new error", got)
	}
}

func TestMergeMissingFieldsDoNotOverwrite(t *testing.T) {
	dst := Partial{
		Country:         "DE",
		Region:          "Berlin",
		City:            "Berlin",
		ISP:             "Example ISP",
		ASN:             "AS64500 Example ISP",
		IsMobile:        ptr(false),
		IPAPIProxy:      ptr(false),
		IPAPIHosting:    ptr(false),
		ProxyCheckType:  "Residential",
		ProxyCheckProxy: ptr(false),
		RiskScore:       ptr(0),
		GreyNoiseClass:  "unknown",
		SFSAppears:      ptr(false),
		SFSFrequency:    ptr(0),
		DNSBLListed:     ptr(false),
		DNSBLDynamic:    true,
	}
	want := dst

	dst.Merge(Partial{})

	if !reflect.DeepEqual(dst, want) {
		t.Fatalf("Merge(empty) changed destination:\n got: %#v\nwant: %#v", dst, want)
	}
}

func ptr[T any](value T) *T {
	return &value
}

func mixedCase(value string) string {
	runes := []rune(value)
	for i, r := range runes {
		if i%2 == 0 && r >= 'a' && r <= 'z' {
			runes[i] = r - ('a' - 'A')
		} else if i%2 == 1 && r >= 'A' && r <= 'Z' {
			runes[i] = r + ('a' - 'A')
		}
	}
	return string(runes)
}

func assertPointerValue[T comparable](t *testing.T, name string, got *T, want T) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want pointer to %v", name, got, want)
	}
}
