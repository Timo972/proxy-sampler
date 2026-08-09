package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

var providerTestIP = netip.MustParseAddr("203.0.113.7")

func TestIPAPILookupParsesPayloadAndBuildsRequest(t *testing.T) {
	t.Parallel()
	payload := `{"status":"success","countryCode":"US","regionName":"Virginia","city":"Ashburn","isp":"HostRush","as":"AS62633 HostRush","mobile":false,"proxy":false,"hosting":false}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/203.0.113.7" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got, want := r.URL.Query().Get("fields"), "status,countryCode,regionName,city,isp,as,mobile,proxy,hosting"; got != want {
			t.Errorf("fields = %q, want %q", got, want)
		}
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(server.Close)

	provider := NewIPAPI(server.Client())
	provider.baseURL = server.URL + "/json/"
	partial, err := provider.Lookup(context.Background(), providerTestIP)
	if err != nil {
		t.Fatal(err)
	}
	// Country stores the ISO code so it can be matched against provider targeting.
	if partial.Country != "US" || partial.Region != "Virginia" || partial.City != "Ashburn" || partial.ISP != "HostRush" || partial.ASN != "AS62633 HostRush" {
		t.Fatalf("unexpected location/network fields: %#v", partial)
	}
	assertBoolPointer(t, "IsMobile", partial.IsMobile, false)
	assertBoolPointer(t, "IPAPIProxy", partial.IPAPIProxy, false)
	assertBoolPointer(t, "IPAPIHosting", partial.IPAPIHosting, false)
	assertRawPayload(t, partial, "ip-api", payload)
	if !partial.HadSignal || provider.Name() != "ip-api" {
		t.Fatalf("HadSignal=%v Name=%q", partial.HadSignal, provider.Name())
	}
}

func TestProxyCheckLookupParsesPayloadAndBuildsRequest(t *testing.T) {
	t.Parallel()
	payload := `{"status":"ok","203.0.113.7":{"type":"Wireless","proxy":"yes","risk":"73"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/203.0.113.7" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("vpn") != "1" || r.URL.Query().Get("risk") != "1" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(server.Close)

	provider := NewProxyCheck(server.Client())
	provider.baseURL = server.URL + "/v2/"
	partial, err := provider.Lookup(context.Background(), providerTestIP)
	if err != nil {
		t.Fatal(err)
	}
	if partial.ProxyCheckType != "Wireless" {
		t.Fatalf("type = %q", partial.ProxyCheckType)
	}
	assertBoolPointer(t, "ProxyCheckProxy", partial.ProxyCheckProxy, true)
	assertIntPointer(t, "RiskScore", partial.RiskScore, 73)
	assertRawPayload(t, partial, "proxycheck", payload)
	if !partial.HadSignal || provider.Name() != "proxycheck" {
		t.Fatalf("HadSignal=%v Name=%q", partial.HadSignal, provider.Name())
	}
}

func TestProxyCheckLookupAcceptsNumericRisk(t *testing.T) {
	t.Parallel()
	// Production returns risk as a JSON number, not a quoted string.
	payload := `{"status":"ok","203.0.113.7":{"type":"Wireless","proxy":"yes","risk":73}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(server.Close)

	provider := NewProxyCheck(server.Client())
	provider.baseURL = server.URL + "/v2/"
	partial, err := provider.Lookup(context.Background(), providerTestIP)
	if err != nil {
		t.Fatalf("Lookup with numeric risk: %v", err)
	}
	assertIntPointer(t, "RiskScore", partial.RiskScore, 73)
}

func TestGreyNoiseTreats404AsBenign(t *testing.T) {
	t.Parallel()
	// GreyNoise community returns 404 for an IP it has not observed.
	body := `{"ip":"203.0.113.7","noise":false,"riot":false,"message":"IP not observed scanning the internet."}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	provider := NewGreyNoise(server.Client())
	provider.baseURL = server.URL + "/v3/community/"
	partial, err := provider.Lookup(context.Background(), providerTestIP)
	if err != nil {
		t.Fatalf("404 should be benign, got error: %v", err)
	}
	if partial.HadSignal || partial.GreyNoiseClass != "" {
		t.Fatalf("partial = %#v, want empty no-signal", partial)
	}
}

func TestGreyNoiseLookupParsesPayloadAndBuildsRequest(t *testing.T) {
	t.Parallel()
	payload := `{"ip":"203.0.113.7","noise":true,"riot":false,"classification":"malicious"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/community/203.0.113.7" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(server.Close)

	provider := NewGreyNoise(server.Client())
	provider.baseURL = server.URL + "/v3/community/"
	partial, err := provider.Lookup(context.Background(), providerTestIP)
	if err != nil {
		t.Fatal(err)
	}
	if partial.GreyNoiseClass != "malicious" || !partial.HadSignal || provider.Name() != "greynoise" {
		t.Fatalf("partial=%#v Name=%q", partial, provider.Name())
	}
	assertRawPayload(t, partial, "greynoise", payload)
}

func TestGreyNoiseSkipsIPv6WithoutHTTPCall(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	t.Cleanup(server.Close)
	provider := NewGreyNoise(server.Client())
	provider.baseURL = server.URL + "/"

	partial, err := provider.Lookup(context.Background(), netip.MustParseAddr("2001:db8::7"))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || partial.HadSignal || len(partial.Raw) != 0 {
		t.Fatalf("calls=%d partial=%#v", calls.Load(), partial)
	}
}

func TestStopForumSpamLookupParsesPayloadAndBuildsRequest(t *testing.T) {
	t.Parallel()
	payload := `{"success":1,"ip":{"appears":1,"frequency":42,"lastseen":"2026-07-20 10:00:00"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("ip") != providerTestIP.String() || !r.URL.Query().Has("json") {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(server.Close)

	provider := NewStopForumSpam(server.Client())
	provider.baseURL = server.URL + "/api"
	partial, err := provider.Lookup(context.Background(), providerTestIP)
	if err != nil {
		t.Fatal(err)
	}
	assertBoolPointer(t, "SFSAppears", partial.SFSAppears, true)
	assertIntPointer(t, "SFSFrequency", partial.SFSFrequency, 42)
	assertRawPayload(t, partial, "stopforumspam", payload)
	if !partial.HadSignal || provider.Name() != "stopforumspam" {
		t.Fatalf("HadSignal=%v Name=%q", partial.HadSignal, provider.Name())
	}
}

func TestStopForumSpamMissingFieldsAreNotAuthoritativeSignals(t *testing.T) {
	t.Parallel()
	payload := `{"success":1,"ip":{}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(server.Close)
	provider := NewStopForumSpam(server.Client())
	provider.baseURL = server.URL

	partial, err := provider.Lookup(context.Background(), providerTestIP)
	if err != nil {
		t.Fatal(err)
	}
	if partial.SFSAppears != nil || partial.SFSFrequency != nil || partial.HadSignal {
		t.Fatalf("missing fields became authoritative values: %#v", partial)
	}
	assertRawPayload(t, partial, "stopforumspam", payload)
}

func TestProvidersRejectUnsuccessfulProviderStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		payload  string
		provider func(*http.Client) Provider
	}{
		{"ip-api", `{"status":"fail","message":"private range"}`, func(client *http.Client) Provider {
			p := NewIPAPI(client)
			return p
		}},
		{"proxycheck", `{"status":"error","message":"denied"}`, func(client *http.Client) Provider {
			p := NewProxyCheck(client)
			return p
		}},
		{"stopforumspam", `{"success":0}`, func(client *http.Client) Provider {
			p := NewStopForumSpam(client)
			return p
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.payload))
			}))
			defer server.Close()
			provider := test.provider(server.Client())
			setProviderBaseURL(provider, server.URL+"/")
			if _, err := provider.Lookup(context.Background(), providerTestIP); err == nil {
				t.Fatal("expected provider-level status error")
			}
		})
	}
}

func TestProxyCheckRejectsOutOfRangeRisk(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","203.0.113.7":{"type":"Residential","proxy":"no","risk":"101"}}`))
	}))
	t.Cleanup(server.Close)
	provider := NewProxyCheck(server.Client())
	provider.baseURL = server.URL + "/"
	if _, err := provider.Lookup(context.Background(), providerTestIP); err == nil || !strings.Contains(err.Error(), "risk") {
		t.Fatalf("error = %v", err)
	}
}

func TestProviderHTTPErrorUsesBoundedExcerpt(t *testing.T) {
	t.Parallel()
	tail := "TAIL-MUST-NOT-APPEAR"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(strings.Repeat("x", 2048) + tail))
	}))
	t.Cleanup(server.Close)
	provider := NewIPAPI(server.Client())
	provider.baseURL = server.URL + "/"

	_, err := provider.Lookup(context.Background(), providerTestIP)
	if err == nil {
		t.Fatal("expected HTTP status error")
	}
	if strings.Contains(err.Error(), tail) || len(err.Error()) > 400 {
		t.Fatalf("error is not safely bounded: len=%d error=%q", len(err.Error()), err)
	}
}

func TestProviderRejectsOversizedSuccessfulBody(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxProviderBody+1)))
	}))
	t.Cleanup(server.Close)
	provider := NewIPAPI(server.Client())
	provider.baseURL = server.URL + "/"

	_, err := provider.Lookup(context.Background(), providerTestIP)
	if err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("error = %v", err)
	}
}

func TestProviderRequestErrorDoesNotExposeURLOrCredentials(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial %s", request.URL.String())
	})}
	provider := NewIPAPI(client)
	provider.baseURL = "http://provider-user:provider-password@provider.example/private/"

	_, err := provider.Lookup(context.Background(), providerTestIP)
	if err == nil {
		t.Fatal("expected request failure")
	}
	for _, secret := range []string{"provider.example", "provider-user", "provider-password", "/private/"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error exposed request URL or credentials: %q", err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func assertRawPayload(t *testing.T, partial Partial, provider, want string) {
	t.Helper()
	got, ok := partial.Raw[provider]
	if !ok {
		t.Fatalf("raw payload missing provider %q", provider)
	}
	if !json.Valid(got) || string(got) != want {
		t.Fatalf("raw[%q] = %q, want %q", provider, got, want)
	}
}

func assertBoolPointer(t *testing.T, name string, got *bool, want bool) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want pointer to %v", name, got, want)
	}
}

func assertIntPointer(t *testing.T, name string, got *int, want int) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want pointer to %d", name, got, want)
	}
}

func setProviderBaseURL(provider Provider, baseURL string) {
	switch value := provider.(type) {
	case *IPAPI:
		value.baseURL = baseURL
	case *ProxyCheck:
		value.baseURL = baseURL
	case *StopForumSpam:
		value.baseURL = baseURL
	}
}
