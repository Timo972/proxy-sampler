package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const (
	ipAPIBase           = "http://ip-api.com/json/"
	maxProviderBody     = 1 << 20
	maxHTTPErrorExcerpt = 256
)

var hardenedHTTPClient = &http.Client{Timeout: 10 * time.Second}

type IPAPI struct {
	client  *http.Client
	baseURL string
}

func NewIPAPI(client *http.Client) *IPAPI {
	return &IPAPI{client: providerHTTPClient(client), baseURL: ipAPIBase}
}

func (p *IPAPI) Name() string { return "ip-api" }

func (p *IPAPI) Lookup(ctx context.Context, ip netip.Addr) (Partial, error) {
	requestURL := p.baseURL + ip.String() + "?fields=status,countryCode,regionName,city,isp,as,mobile,proxy,hosting"
	body, err := providerGET(ctx, p.client, p.Name(), requestURL, nil)
	if err != nil {
		return Partial{}, err
	}
	var response struct {
		Status      string `json:"status"`
		CountryCode string `json:"countryCode"`
		RegionName  string `json:"regionName"`
		City        string `json:"city"`
		ISP         string `json:"isp"`
		ASN         string `json:"as"`
		Mobile      *bool  `json:"mobile"`
		Proxy       *bool  `json:"proxy"`
		Hosting     *bool  `json:"hosting"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return Partial{}, fmt.Errorf("%s: decode response: %w", p.Name(), err)
	}
	if response.Status != "success" {
		return Partial{}, fmt.Errorf("%s: provider status %q", p.Name(), response.Status)
	}
	// Store the ISO 3166-1 alpha-2 code (e.g. "DE"), not the full English name,
	// so it can be compared against the ISO codes proxy providers use as
	// country targeting tokens (the run report's honor rate).
	return Partial{
		Country: response.CountryCode, Region: response.RegionName, City: response.City,
		ISP: response.ISP, ASN: response.ASN, IsMobile: response.Mobile,
		IPAPIProxy: response.Proxy, IPAPIHosting: response.Hosting,
		Raw: map[string]json.RawMessage{p.Name(): body}, HadSignal: true,
	}, nil
}

func providerHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		return hardenedHTTPClient
	}
	return client
}

func providerGET(ctx context.Context, client *http.Client, provider, requestURL string, headers http.Header) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: build request", provider)
	}
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, &providerRequestError{provider: provider, cause: err}
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		excerpt, _ := io.ReadAll(io.LimitReader(response.Body, maxHTTPErrorExcerpt))
		cleaned := strings.TrimSpace(string(excerpt))
		if cleaned == "" {
			return nil, fmt.Errorf("%s: HTTP status %d", provider, response.StatusCode)
		}
		return nil, fmt.Errorf("%s: HTTP status %d: %s", provider, response.StatusCode, strconv.QuoteToASCII(cleaned))
	}

	limited := io.LimitReader(response.Body, maxProviderBody+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("%s: read response: %w", provider, err)
	}
	if len(body) > maxProviderBody {
		return nil, fmt.Errorf("%s: response exceeds %d bytes", provider, maxProviderBody)
	}
	return body, nil
}

type providerRequestError struct {
	provider string
	cause    error
}

func (e *providerRequestError) Error() string { return e.provider + ": request failed" }

func (e *providerRequestError) Unwrap() error { return e.cause }
