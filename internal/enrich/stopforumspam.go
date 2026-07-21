package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
)

const stopForumSpamURL = "https://api.stopforumspam.org/api"

type StopForumSpam struct {
	client  *http.Client
	baseURL string
}

func NewStopForumSpam(client *http.Client) *StopForumSpam {
	return &StopForumSpam{client: providerHTTPClient(client), baseURL: stopForumSpamURL}
}

func (p *StopForumSpam) Name() string { return "stopforumspam" }

func (p *StopForumSpam) Lookup(ctx context.Context, ip netip.Addr) (Partial, error) {
	requestURL, err := url.Parse(p.baseURL)
	if err != nil {
		return Partial{}, fmt.Errorf("%s: parse endpoint: %w", p.Name(), err)
	}
	query := requestURL.Query()
	query.Set("ip", ip.String())
	query.Set("json", "")
	requestURL.RawQuery = query.Encode()
	body, err := providerGET(ctx, p.client, p.Name(), requestURL.String(), nil)
	if err != nil {
		return Partial{}, err
	}
	var response struct {
		Success int `json:"success"`
		IP      struct {
			Appears   int    `json:"appears"`
			Frequency int    `json:"frequency"`
			LastSeen  string `json:"lastseen"`
		} `json:"ip"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return Partial{}, fmt.Errorf("%s: decode response: %w", p.Name(), err)
	}
	if response.Success != 1 {
		return Partial{}, fmt.Errorf("%s: provider status unsuccessful", p.Name())
	}
	appears := response.IP.Appears != 0
	frequency := response.IP.Frequency
	return Partial{
		SFSAppears: &appears, SFSFrequency: &frequency,
		Raw: map[string]json.RawMessage{p.Name(): body}, HadSignal: true,
	}, nil
}
