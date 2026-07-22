package sampler

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const validTrace = "fl=123f45\nh=speed.cloudflare.com\nip=203.0.113.7\nloc=US\ncolo=IAD\n"

type recordingDialer struct {
	address string

	mu        sync.Mutex
	dials     int
	deadlines []time.Time
}

func (d *recordingDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	d.mu.Lock()
	d.dials++
	if deadline, ok := ctx.Deadline(); ok {
		d.deadlines = append(d.deadlines, deadline)
	}
	d.mu.Unlock()

	return (&net.Dialer{}).DialContext(ctx, network, d.address)
}

func (d *recordingDialer) snapshot() (int, []time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials, append([]time.Time(nil), d.deadlines...)
}

func probeServer(t *testing.T, handler http.Handler) (*httptest.Server, *recordingDialer, string) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	dialer := &recordingDialer{address: server.Listener.Addr().String()}
	return server, dialer, "http://probe.invalid/cdn-cgi/trace"
}

func TestHTTPProberParsesCloudflareTraceAndUsesFreshConnections(t *testing.T) {
	var (
		mu              sync.Mutex
		connectionClose []bool
	)
	_, dialer, target := probeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connectionClose = append(connectionClose, r.Close)
		mu.Unlock()
		time.Sleep(time.Millisecond)
		_, _ = fmt.Fprint(w, validTrace)
	}))

	prober := HTTPProber{}
	for range 2 {
		result := prober.Probe(context.Background(), dialer, target, time.Second)
		if result.Err != nil {
			t.Fatalf("Probe() error = %v", result.Err)
		}
		if result.IP != netip.MustParseAddr("203.0.113.7") {
			t.Fatalf("Probe() IP = %v", result.IP)
		}
		if result.Country != "US" || result.Colo != "IAD" {
			t.Fatalf("Probe() location = %q/%q", result.Country, result.Colo)
		}
		if result.RTT <= 0 {
			t.Fatalf("Probe() RTT = %v, want positive", result.RTT)
		}
	}

	dials, deadlines := dialer.snapshot()
	if dials != 2 {
		t.Fatalf("dial count = %d, want one fresh connection per probe", dials)
	}
	if len(deadlines) != 2 {
		t.Fatalf("deadline count = %d, want 2", len(deadlines))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(connectionClose) != 2 || !connectionClose[0] || !connectionClose[1] {
		t.Fatalf("request Close flags = %v, want [true true]", connectionClose)
	}
}

func TestHTTPProberPreservesEarlierParentDeadline(t *testing.T) {
	_, dialer, target := probeServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, validTrace)
	}))
	parentDeadline := time.Now().Add(250 * time.Millisecond)
	ctx, cancel := context.WithDeadline(context.Background(), parentDeadline)
	defer cancel()

	result := (HTTPProber{}).Probe(ctx, dialer, target, time.Second)
	if result.Err != nil {
		t.Fatalf("Probe() error = %v", result.Err)
	}
	_, deadlines := dialer.snapshot()
	if len(deadlines) != 1 {
		t.Fatalf("deadlines = %v", deadlines)
	}
	if delta := deadlines[0].Sub(parentDeadline); delta < -10*time.Millisecond || delta > 10*time.Millisecond {
		t.Fatalf("dial deadline = %v, parent = %v, delta = %v", deadlines[0], parentDeadline, delta)
	}
}

func TestHTTPProberRejectsNon2xxWithoutParsingBody(t *testing.T) {
	_, dialer, target := probeServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprint(w, validTrace)
	}))

	result := (HTTPProber{}).Probe(context.Background(), dialer, target, time.Second)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "502") {
		t.Fatalf("Probe() error = %v, want status 502", result.Err)
	}
	if result.IP.IsValid() {
		t.Fatalf("Probe() IP = %v on failed status", result.IP)
	}
}

func TestHTTPProberDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	_, dialer, target := probeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/redirected" {
			_, _ = fmt.Fprint(w, validTrace)
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusFound)
	}))
	result := (HTTPProber{}).Probe(context.Background(), dialer, target, time.Second)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "302") {
		t.Fatalf("Probe() error = %v, want status 302", result.Err)
	}
	if hits.Load() != 1 {
		t.Fatalf("request hits = %d, want redirect not followed", hits.Load())
	}
}

func TestHTTPProberRejectsMalformedTrace(t *testing.T) {
	tests := []struct {
		name  string
		trace string
	}{
		{name: "missing IP", trace: "loc=US\ncolo=IAD\n"},
		{name: "malformed IP", trace: "ip=not-an-ip\nloc=US\ncolo=IAD\n"},
		{name: "missing country", trace: "ip=203.0.113.7\ncolo=IAD\n"},
		{name: "malformed country", trace: "ip=203.0.113.7\nloc=USA\ncolo=IAD\n"},
		{name: "missing colo", trace: "ip=203.0.113.7\nloc=US\n"},
		{name: "malformed colo", trace: "ip=203.0.113.7\nloc=US\ncolo=TOOLONG\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, dialer, target := probeServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, tt.trace)
			}))
			result := (HTTPProber{}).Probe(context.Background(), dialer, target, time.Second)
			if result.Err == nil {
				t.Fatalf("Probe() = %#v, want malformed trace error", result)
			}
		})
	}
}

func TestParseTraceRejectsDuplicateKeys(t *testing.T) {
	trace := "ip=203.0.113.7\nip=203.0.113.8\nloc=US\ncolo=IAD\n"
	result := parseTrace(strings.NewReader(trace), time.Millisecond)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "duplicate") {
		t.Fatalf("parseTrace duplicate key = %#v, want malformed duplicate error", result)
	}
	if result.IP.IsValid() {
		t.Fatalf("duplicate trace produced IP %v", result.IP)
	}
}

func TestParseTraceRejectsNegativeRTT(t *testing.T) {
	result := parseTrace(strings.NewReader(validTrace), -time.Nanosecond)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "negative RTT") {
		t.Fatalf("parseTrace negative RTT = %#v, want failure", result)
	}
	if result.IP.IsValid() || result.RTT != -time.Nanosecond {
		t.Fatalf("negative RTT result = %#v, want no successful observation", result)
	}
}

func TestHTTPProberCapsTraceBody(t *testing.T) {
	body := strings.Repeat("padding=x\n", (64<<10)/len("padding=x\n")+1) + validTrace
	_, dialer, target := probeServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, body)
	}))

	result := (HTTPProber{}).Probe(context.Background(), dialer, target, time.Second)
	if result.Err == nil {
		t.Fatalf("Probe() = %#v, want IP beyond body cap to be ignored", result)
	}
}

func TestHTTPProberHonorsContextCancellation(t *testing.T) {
	started := make(chan struct{})
	_, dialer, target := probeServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()

	result := (HTTPProber{}).Probe(ctx, dialer, target, 2*time.Second)
	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("Probe() error = %v, want context.Canceled", result.Err)
	}
}
