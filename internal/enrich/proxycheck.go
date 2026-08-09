package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
)

const proxyCheckBase = "https://proxycheck.io/v2/"

type ProxyCheck struct {
	client  *http.Client
	baseURL string
}

func NewProxyCheck(client *http.Client) *ProxyCheck {
	return &ProxyCheck{client: providerHTTPClient(client), baseURL: proxyCheckBase}
}

func (p *ProxyCheck) Name() string { return "proxycheck" }

func (p *ProxyCheck) Lookup(ctx context.Context, ip netip.Addr) (Partial, error) {
	body, err := providerGET(ctx, p.client, p.Name(), p.baseURL+ip.String()+"?vpn=1&risk=1", nil)
	if err != nil {
		return Partial{}, err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Partial{}, fmt.Errorf("%s: decode response: %w", p.Name(), err)
	}
	var status string
	if err := json.Unmarshal(envelope["status"], &status); err != nil || status != "ok" {
		return Partial{}, fmt.Errorf("%s: provider status %q", p.Name(), status)
	}
	entryJSON, ok := envelope[ip.String()]
	if !ok {
		return Partial{}, fmt.Errorf("%s: response missing IP entry", p.Name())
	}
	var entry struct {
		Type  string `json:"type"`
		Proxy string `json:"proxy"`
		// proxycheck returns risk as a JSON number, but has historically also
		// returned it quoted; accept either.
		Risk json.RawMessage `json:"risk"`
	}
	if err := json.Unmarshal(entryJSON, &entry); err != nil {
		return Partial{}, fmt.Errorf("%s: decode IP entry: %w", p.Name(), err)
	}
	proxy, err := parseYesNo(entry.Proxy)
	if err != nil {
		return Partial{}, fmt.Errorf("%s: proxy: %w", p.Name(), err)
	}
	risk, err := parseProxyCheckRisk(entry.Risk)
	if err != nil {
		return Partial{}, fmt.Errorf("%s: %w", p.Name(), err)
	}
	return Partial{
		ProxyCheckType: entry.Type, ProxyCheckProxy: &proxy, RiskScore: risk,
		Raw: map[string]json.RawMessage{p.Name(): body}, HadSignal: true,
	}, nil
}

// parseProxyCheckRisk accepts a proxycheck risk value as either a JSON number
// (73) or a quoted string ("73"). A missing/null risk yields a nil score rather
// than an error, so the proxy/type signal is still recorded.
func parseProxyCheckRisk(raw json.RawMessage) (*int, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	trimmed = strings.Trim(trimmed, `"`)
	risk, err := strconv.Atoi(trimmed)
	if err != nil || risk < 0 || risk > 100 {
		return nil, fmt.Errorf("risk must be an integer from 0 to 100")
	}
	return &risk, nil
}

func parseYesNo(value string) (bool, error) {
	switch value {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	default:
		return false, fmt.Errorf("unexpected value %q", value)
	}
}
