package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
)

const greyNoiseBase = "https://api.greynoise.io/v3/community/"

type GreyNoise struct {
	client  *http.Client
	baseURL string
}

func NewGreyNoise(client *http.Client) *GreyNoise {
	return &GreyNoise{client: providerHTTPClient(client), baseURL: greyNoiseBase}
}

func (p *GreyNoise) Name() string { return "greynoise" }

func (p *GreyNoise) Lookup(ctx context.Context, ip netip.Addr) (Partial, error) {
	if !ip.Is4() {
		return Partial{}, nil
	}
	headers := make(http.Header)
	headers.Set("Accept", "application/json")
	body, err := providerGET(ctx, p.client, p.Name(), p.baseURL+ip.String(), headers)
	if err != nil {
		// The GreyNoise community API returns 404 for an IP it has not observed
		// scanning the internet — the common case, not an error. Treat it as a
		// benign "no signal" result instead of failing enrichment.
		var httpErr *providerHTTPError
		if errors.As(err, &httpErr) && httpErr.statusCode == http.StatusNotFound {
			return Partial{}, nil
		}
		return Partial{}, err
	}
	var response struct {
		IP             string `json:"ip"`
		Noise          bool   `json:"noise"`
		RIOT           bool   `json:"riot"`
		Classification string `json:"classification"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return Partial{}, fmt.Errorf("%s: decode response: %w", p.Name(), err)
	}
	if response.Classification == "" {
		return Partial{}, fmt.Errorf("%s: response missing classification", p.Name())
	}
	return Partial{
		GreyNoiseClass: response.Classification,
		Raw:            map[string]json.RawMessage{p.Name(): body}, HadSignal: true,
	}, nil
}
