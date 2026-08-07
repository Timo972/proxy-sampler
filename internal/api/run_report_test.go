package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/ch"
	cryptox "github.com/timo972/proxy-sampler/internal/crypto"
	"github.com/timo972/proxy-sampler/internal/session"
	"github.com/timo972/proxy-sampler/internal/variation"
)

func TestRunReportReturnsPoolRollup(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	handler := testRunHandlerWithReader(t, store, runStore, &fakeControl{}, stubReader{})
	request(t, handler, http.MethodPost, "/api/runs",
		`{"name":"r","template":"p://u-{c}:pw@gate.example:1080","axes":{"c":{"kind":"list","values":["de"]}},"mode":"sticky","cadence_seconds":30}`)
	runID := runStore.runs[0].ID.String()
	resp := request(t, handler, http.MethodGet, "/api/runs/"+runID+"/report", "")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `"estimated_pool_size"`) {
		t.Fatalf("body missing pool report fields: %s", resp.Body.String())
	}
}

func TestExportRunCSVReturns503WhenPoolQueryFailsBeforeRows(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	runStore.streamErr = errors.New("pool query failed")
	handler := testRunHandlerWithReader(t, store, runStore, &fakeControl{}, stubReader{})
	request(t, handler, http.MethodPost, "/api/runs",
		`{"name":"r","template":"p://u-{c}:pw@gate.example:1080","axes":{"c":{"kind":"list","values":["de"]}},"mode":"sticky","cadence_seconds":30}`)
	runID := runStore.runs[0].ID.String()

	resp := request(t, handler, http.MethodGet, "/api/runs/"+runID+"/export.csv", "")
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (query failed before any row); body=%s", resp.Code, resp.Body.String())
	}
}

func TestExportRunCSVStreamsDedupedPoolRows(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	handler := testRunHandlerWithReader(t, store, runStore, &fakeControl{}, stubReader{})
	request(t, handler, http.MethodPost, "/api/runs",
		`{"name":"poolcheck","template":"p://u-{c}:pw@gate.example:1080","axes":{"c":{"kind":"list","values":["de"]}},"mode":"sticky","cadence_seconds":30}`)
	runID := runStore.runs[0].ID
	risk := 82
	runStore.poolIPs[runID] = []variation.IPRow{
		{IP: "203.0.113.7", Category: "residential", Country: "DE", ISP: "ISP-1", ASN: "AS1", RiskScore: &risk, GreyNoiseClass: "benign", DNSBLListed: true, DNSBLHits: []string{"zen", "spamhaus"}, HitCount: 9},
	}

	resp := request(t, handler, http.MethodGet, "/api/runs/"+runID.String()+"/export.csv", "")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.Code, resp.Body.String())
	}
	if ct := resp.Header().Get("Content-Type"); ct != "text/csv" {
		t.Fatalf("content-type = %q, want text/csv", ct)
	}
	body := resp.Body.String()
	if !strings.Contains(body, "ip,category,country,isp,asn,risk_score,greynoise_class,dnsbl_listed,dnsbl_hits,hit_count") {
		t.Fatalf("missing header row: %s", body)
	}
	// risk_score rendered, dnsbl_hits joined with "|", hit_count present.
	if !strings.Contains(body, "203.0.113.7,residential,DE,ISP-1,AS1,82,benign,true,zen|spamhaus,9") {
		t.Fatalf("data row not streamed as expected: %s", body)
	}
}

func TestRunReportBoundsObservationsAndIPDetails(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	handler := testRunHandlerWithReader(t, store, runStore, &fakeControl{}, stubReader{})
	request(t, handler, http.MethodPost, "/api/runs",
		`{"name":"pool","template":"p://u-{c}:pw@gate.example:1080","axes":{"c":{"kind":"list","values":["de"]}},"mode":"pool","cadence_seconds":30}`)
	runID := runStore.runs[0].ID
	childID := runStore.children[runID][0].Session.ID

	// Seed more distinct exit IPs than the response cap allows.
	obs := make([]variation.IPObservation, maxReportIPRows+50)
	for i := range obs {
		obs[i] = variation.IPObservation{
			SessionID:  childID,
			IP:         netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}),
			HitCount:   int64(i + 1),
			Reputation: &session.Reputation{Country: "DE", Category: "residential"},
		}
	}
	runStore.observations[runID] = obs

	resp := request(t, handler, http.MethodGet, "/api/runs/"+runID.String()+"/report", "")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	if runStore.lastObservationLimit != maxReportObservations {
		t.Fatalf("observation limit passed = %d, want %d", runStore.lastObservationLimit, maxReportObservations)
	}
	var body struct {
		Ips []json.RawMessage `json:"ips"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Ips) != maxReportIPRows {
		t.Fatalf("ip rows = %d, want capped at %d", len(body.Ips), maxReportIPRows)
	}
}

func TestRunReportNotFound(t *testing.T) {
	handler := testRunHandlerWithReader(t, newMemoryStore(), newMemoryRunStore(), &fakeControl{}, stubReader{})
	resp := request(t, handler, http.MethodGet, "/api/runs/"+uuid.NewString()+"/report", "")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.Code)
	}
}

func TestExportRunCSVWritesPoolIPHeaderRow(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	handler := testRunHandlerWithReader(t, store, runStore, &fakeControl{}, stubReader{})
	request(t, handler, http.MethodPost, "/api/runs",
		`{"name":"r","template":"p://u-{c}:pw@gate.example:1080","axes":{"c":{"kind":"list","values":["de"]}},"mode":"sticky","cadence_seconds":30}`)
	runID := runStore.runs[0].ID.String()
	resp := request(t, handler, http.MethodGet, "/api/runs/"+runID+"/export.csv", "")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Type"); got != "text/csv" {
		t.Fatalf("content-type = %q, want text/csv", got)
	}
	if disposition := resp.Header().Get("Content-Disposition"); !strings.Contains(disposition, "-pool.csv") {
		t.Fatalf("content-disposition = %q, want *-pool.csv", disposition)
	}
	if !strings.HasPrefix(resp.Body.String(), "ip,category,country,isp,asn,risk_score,greynoise_class,dnsbl_listed,dnsbl_hits,hit_count") {
		t.Fatalf("body missing CSV header: %s", resp.Body.String())
	}
}

func TestExportRunCSVNotFound(t *testing.T) {
	handler := testRunHandlerWithReader(t, newMemoryStore(), newMemoryRunStore(), &fakeControl{}, stubReader{})
	resp := request(t, handler, http.MethodGet, "/api/runs/"+uuid.NewString()+"/export.csv", "")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.Code)
	}
}

type stubReader struct{}

func (stubReader) Ping(context.Context) error { return nil }
func (stubReader) Samples(context.Context, uuid.UUID, *time.Time, *time.Time, int) (ch.SamplePage, error) {
	return ch.SamplePage{}, nil
}
func (stubReader) StreamSamples(context.Context, uuid.UUID, *time.Time, *time.Time, func(ch.Event) error) error {
	return nil
}
func (stubReader) Series(context.Context, uuid.UUID, time.Time, time.Time, time.Duration) ([]ch.SeriesPoint, error) {
	return nil, nil
}
func (stubReader) Stickiness(context.Context, uuid.UUID, time.Time, time.Time) (ch.Stickiness, error) {
	return ch.Stickiness{}, nil
}
func (stubReader) PoolGrowth(context.Context, uuid.UUID, time.Time, time.Time) ([]ch.GrowthPoint, error) {
	return nil, nil
}
func (stubReader) SeriesForSessions(context.Context, []uuid.UUID, time.Time, time.Time, time.Duration) ([]ch.SeriesPoint, error) {
	return nil, nil
}

func testRunHandlerWithReader(t *testing.T, store session.Store, runStore variation.Store, control *fakeControl, reader Reader) http.Handler {
	t.Helper()
	cipher, err := cryptox.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, control, cipher, Defaults{ProbeTarget: testProbeTarget, DialTimeout: 10 * time.Second}, runStore, 128, reader)
	return server.Handler()
}
