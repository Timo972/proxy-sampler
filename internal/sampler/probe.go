// Package sampler probes proxy egress and deterministically aggregates observations.
package sampler

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

const maxTraceBody = 64 << 10

// ProbeResult is one proxy egress observation.
type ProbeResult struct {
	IP      netip.Addr
	Country string
	Colo    string
	RTT     time.Duration
	Err     error
}

// Prober measures one proxy egress observation.
type Prober interface {
	Probe(context.Context, proxy.ContextDialer, string, time.Duration) ProbeResult
}

// HTTPProber reads the Cloudflare trace endpoint through a proxy dialer.
type HTTPProber struct {
	now func() time.Time
}

// Probe executes one isolated HTTP request through d.
func (p HTTPProber) Probe(ctx context.Context, d proxy.ContextDialer, target string, timeout time.Duration) ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	transport := &http.Transport{
		DialContext: func(_ context.Context, network, address string) (net.Conn, error) {
			return d.DialContext(ctx, network, address)
		},
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: timeout,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return ProbeResult{Err: fmt.Errorf("build probe request: %w", err)}
	}

	start := p.timeNow()
	resp, err := client.Do(req)
	rtt := p.timeNow().Sub(start)
	if err != nil {
		return ProbeResult{RTT: rtt, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return ProbeResult{RTT: rtt, Err: fmt.Errorf("probe returned HTTP status %d", resp.StatusCode)}
	}

	return parseTrace(io.LimitReader(resp.Body, maxTraceBody), rtt)
}

func (p HTTPProber) timeNow() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func parseTrace(r io.Reader, rtt time.Duration) ProbeResult {
	values := make(map[string]string, 3)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if ok {
			values[key] = strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return ProbeResult{RTT: rtt, Err: fmt.Errorf("read probe trace: %w", err)}
	}

	ip, err := netip.ParseAddr(values["ip"])
	if err != nil {
		return ProbeResult{RTT: rtt, Err: fmt.Errorf("invalid probe IP %q: %w", values["ip"], err)}
	}
	country := values["loc"]
	if !isUpperAlphaCode(country, 2) {
		return ProbeResult{RTT: rtt, Err: fmt.Errorf("invalid probe country %q", country)}
	}
	colo := values["colo"]
	if !isUpperAlphaCode(colo, 3) {
		return ProbeResult{RTT: rtt, Err: fmt.Errorf("invalid probe colo %q", colo)}
	}

	return ProbeResult{IP: ip.Unmap(), Country: country, Colo: colo, RTT: rtt}
}

func isUpperAlphaCode(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, char := range value {
		if char < 'A' || char > 'Z' {
			return false
		}
	}
	return true
}
