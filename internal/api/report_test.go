package api

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/ch"
	cryptox "github.com/timo972/proxy-sampler/internal/crypto"
	"github.com/timo972/proxy-sampler/internal/session"
)

func TestSessionReportComposesAllSectionsAndFlagsIPs(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	started := now.Add(-24 * time.Hour)
	value := sampleSession()
	value.StartedAt = &started
	store := &reportStore{memoryStore: newMemoryStore(), ips: reportIPFixtures(now)}
	store.sessions = []session.Session{value}
	reader := &fakeReportReader{
		series: []ch.SeriesPoint{{At: started, SuccessRate: .75, LatencyP50MS: 10, LatencyP95MS: 20, DistinctPerSample: 1.5, IPChanges: 2, Mobile: 1, Residential: 2, Datacenter: 3, Unknown: 4}},
		stickiness: ch.Stickiness{
			Holds:              []ch.Hold{{IP: "203.0.113.1", StartedAt: started, EndedAt: started.Add(time.Minute), Samples: 2, DurationSeconds: 60}},
			Rotations:          []ch.Rotation{{At: started.Add(2 * time.Minute), FromIP: "203.0.113.1", ToIP: "203.0.113.2", SincePreviousSeconds: 120}},
			AverageHoldSeconds: 60, MedianHoldSeconds: 60,
		},
		growth: []ch.GrowthPoint{{At: started, DistinctIPs: 1}, {At: started.Add(time.Minute), DistinctIPs: 2}},
	}
	server := reportTestServer(t, store, reader)
	server.now = func() time.Time { return now }

	response := request(t, server.Handler(), http.MethodGet, "/api/sessions/"+value.ID.String()+"/report", "")

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if !reader.seriesFrom.Equal(started) || !reader.seriesTo.Equal(now) || reader.bucket != 5*time.Minute {
		t.Errorf("series range/bucket = %s..%s/%s, want %s..%s/5m", reader.seriesFrom, reader.seriesTo, reader.bucket, started, now)
	}
	var body struct {
		Series     []map[string]any `json:"series"`
		Stickiness struct {
			Holds     []map[string]any `json:"holds"`
			Rotations []map[string]any `json:"rotations"`
		} `json:"stickiness"`
		PoolGrowth      []map[string]any `json:"pool_growth"`
		PoolComposition struct {
			Mobile, Residential, Datacenter, Unknown int
		} `json:"pool_composition"`
		ReputationSummary struct {
			TotalIPs       int     `json:"total_ips"`
			FlaggedIPs     int     `json:"flagged_ips"`
			DNSBLHitIPs    int     `json:"dnsbl_hit_ips"`
			FlaggedPercent float64 `json:"flagged_percent"`
		} `json:"reputation_summary"`
		RiskHistogram []struct {
			Label    string
			Min, Max int
			Count    int
		} `json:"risk_histogram"`
		IPs []struct {
			IP          string
			DNSBLListed bool     `json:"dnsbl_listed"`
			DNSBLHits   []string `json:"dnsbl_hits"`
		} `json:"ips"`
	}
	decodeJSON(t, response, &body)
	if len(body.Series) != 1 || len(body.Stickiness.Holds) != 1 || len(body.Stickiness.Rotations) != 1 || len(body.PoolGrowth) != 2 {
		t.Fatalf("report CH sections have lengths series=%d holds=%d rotations=%d growth=%d", len(body.Series), len(body.Stickiness.Holds), len(body.Stickiness.Rotations), len(body.PoolGrowth))
	}
	if body.PoolComposition != (struct{ Mobile, Residential, Datacenter, Unknown int }{1, 2, 1, 2}) {
		t.Errorf("pool composition = %#v, want mobile=1 residential=2 datacenter=1 unknown=2", body.PoolComposition)
	}
	if body.ReputationSummary.TotalIPs != 6 || body.ReputationSummary.FlaggedIPs != 5 || body.ReputationSummary.DNSBLHitIPs != 1 || body.ReputationSummary.FlaggedPercent != 100*5.0/6.0 {
		t.Errorf("reputation summary = %#v", body.ReputationSummary)
	}
	wantLabels := []string{"0-9", "10-19", "20-29", "30-39", "40-49", "50-59", "60-69", "70-79", "80-89", "90-100"}
	if len(body.RiskHistogram) != len(wantLabels) {
		t.Fatalf("risk histogram buckets = %d, want %d", len(body.RiskHistogram), len(wantLabels))
	}
	for i, want := range wantLabels {
		if body.RiskHistogram[i].Label != want {
			t.Errorf("risk bucket %d label = %q, want %q", i, body.RiskHistogram[i].Label, want)
		}
	}
	if body.RiskHistogram[1].Count != 1 || body.RiskHistogram[7].Count != 1 || body.RiskHistogram[9].Count != 1 {
		t.Errorf("risk histogram counts = %#v", body.RiskHistogram)
	}
	if len(body.IPs) != 6 || body.IPs[4].DNSBLHits == nil || len(body.IPs[4].DNSBLHits) != 2 {
		t.Errorf("IP rows = %#v, want six rows and non-null DNSBL hits", body.IPs)
	}
}

func TestSessionReportUsesHourlyBucketsBeyond48HoursAndExplicitRange(t *testing.T) {
	from := time.Date(2026, 7, 18, 8, 0, 0, 0, time.UTC)
	to := from.Add(49 * time.Hour)
	value := sampleSession()
	store := &reportStore{memoryStore: newMemoryStore()}
	store.sessions = []session.Session{value}
	reader := &fakeReportReader{}

	response := request(t, reportTestServer(t, store, reader).Handler(), http.MethodGet,
		"/api/sessions/"+value.ID.String()+"/report?from="+from.Format(time.RFC3339)+"&to="+to.Format(time.RFC3339), "")

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if !reader.seriesFrom.Equal(from) || !reader.seriesTo.Equal(to) || reader.bucket != time.Hour {
		t.Errorf("series range/bucket = %s..%s/%s, want explicit range/1h", reader.seriesFrom, reader.seriesTo, reader.bucket)
	}
}

func TestSessionReportUsesFiveMinuteBucketsAtExactly48Hours(t *testing.T) {
	from := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	to := from.Add(48 * time.Hour)
	value := sampleSession()
	store := &reportStore{memoryStore: newMemoryStore()}
	store.sessions = []session.Session{value}
	reader := &fakeReportReader{}

	response := request(t, reportTestServer(t, store, reader).Handler(), http.MethodGet,
		"/api/sessions/"+value.ID.String()+"/report?from="+from.Format(time.RFC3339)+"&to="+to.Format(time.RFC3339), "")

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if reader.bucket != 5*time.Minute {
		t.Fatalf("exact 48-hour bucket = %s, want 5m", reader.bucket)
	}
}

func TestSessionReportReturnsEmptyArraysAndStableErrors(t *testing.T) {
	t.Run("empty arrays", func(t *testing.T) {
		value := sampleSession()
		store := &reportStore{memoryStore: newMemoryStore()}
		store.sessions = []session.Session{value}
		response := request(t, reportTestServer(t, store, &fakeReportReader{}).Handler(), http.MethodGet, "/api/sessions/"+value.ID.String()+"/report", "")
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d; body=%s", response.Code, response.Body.String())
		}
		var body map[string]any
		decodeJSON(t, response, &body)
		for _, key := range []string{"series", "pool_growth", "risk_histogram", "ips"} {
			if body[key] == nil {
				t.Errorf("%s = null, want array", key)
			}
		}
		stickiness := body["stickiness"].(map[string]any)
		if stickiness["holds"] == nil || stickiness["rotations"] == nil {
			t.Errorf("stickiness arrays = %#v, want non-null", stickiness)
		}
	})

	t.Run("reversed range", func(t *testing.T) {
		value := sampleSession()
		store := &reportStore{memoryStore: newMemoryStore()}
		store.sessions = []session.Session{value}
		response := request(t, reportTestServer(t, store, &fakeReportReader{}).Handler(), http.MethodGet,
			"/api/sessions/"+value.ID.String()+"/report?from=2026-07-21T12:00:00Z&to=2026-07-21T11:00:00Z", "")
		assertAPIError(t, response, http.StatusBadRequest, errorCodeInvalidRequest)
	})

	t.Run("missing session", func(t *testing.T) {
		response := request(t, reportTestServer(t, &reportStore{memoryStore: newMemoryStore()}, &fakeReportReader{}).Handler(), http.MethodGet,
			"/api/sessions/"+uuid.NewString()+"/report", "")
		assertAPIError(t, response, http.StatusNotFound, errorCodeNotFound)
	})

	t.Run("dependency error", func(t *testing.T) {
		value := sampleSession()
		store := &reportStore{memoryStore: newMemoryStore()}
		store.sessions = []session.Session{value}
		reader := &fakeReportReader{seriesErr: errors.New("clickhouse password=secret")}
		response := request(t, reportTestServer(t, store, reader).Handler(), http.MethodGet, "/api/sessions/"+value.ID.String()+"/report", "")
		assertAPIError(t, response, http.StatusServiceUnavailable, errorCodeDependencyUnavailable)
		assertNoSecret(t, response.Body.String())
	})
}

func reportIPFixtures(now time.Time) []session.IPRecord {
	boolPtr := func(value bool) *bool { return &value }
	intPtr := func(value int) *int { return &value }
	record := func(raw, category string, reputation session.Reputation) session.IPRecord {
		ip := netip.MustParseAddr(raw)
		reputation.IP = ip
		reputation.Category = category
		return session.IPRecord{IP: ip, FirstSeen: now.Add(-time.Hour), LastSeen: now, HitCount: 3, Reputation: &reputation}
	}
	return []session.IPRecord{
		record("203.0.113.1", "mobile", session.Reputation{ProxyCheckProxy: boolPtr(true), RiskScore: intPtr(10)}),
		record("203.0.113.2", "residential", session.Reputation{RiskScore: intPtr(70)}),
		record("203.0.113.3", "residential", session.Reputation{GreyNoiseClass: "malicious"}),
		record("203.0.113.4", "datacenter", session.Reputation{SFSAppears: boolPtr(true)}),
		record("203.0.113.5", "unknown", session.Reputation{DNSBLListed: boolPtr(true), DNSBLHits: []string{"zone-a", "zone-b"}, RiskScore: intPtr(100)}),
		{IP: netip.MustParseAddr("203.0.113.6"), FirstSeen: now, LastSeen: now},
	}
}

type reportStore struct {
	*memoryStore
	ips    []session.IPRecord
	ipsErr error
	ping   func(context.Context) error
}

func (s *reportStore) SessionIPs(context.Context, uuid.UUID) ([]session.IPRecord, error) {
	return append([]session.IPRecord(nil), s.ips...), s.ipsErr
}

func (s *reportStore) Ping(ctx context.Context) error {
	if s.ping != nil {
		return s.ping(ctx)
	}
	return nil
}

type fakeReportReader struct {
	series                                                     []ch.SeriesPoint
	stickiness                                                 ch.Stickiness
	growth                                                     []ch.GrowthPoint
	samples                                                    ch.SamplePage
	stream                                                     []ch.Event
	streamFn                                                   func(context.Context, func(ch.Event) error) error
	seriesErr, stickinessErr, growthErr, samplesErr, streamErr error
	ping                                                       func(context.Context) error
	seriesFrom, seriesTo                                       time.Time
	bucket                                                     time.Duration
	sampleFrom, sampleTo                                       *time.Time
	samplePage                                                 int
	sampleCalls, streamCalls                                   int
}

func (r *fakeReportReader) Ping(ctx context.Context) error {
	if r.ping != nil {
		return r.ping(ctx)
	}
	return nil
}

func (r *fakeReportReader) Samples(_ context.Context, _ uuid.UUID, from, to *time.Time, page int) (ch.SamplePage, error) {
	r.sampleCalls++
	r.sampleFrom, r.sampleTo, r.samplePage = from, to, page
	return r.samples, r.samplesErr
}

func (r *fakeReportReader) StreamSamples(ctx context.Context, _ uuid.UUID, _, _ *time.Time, visit func(ch.Event) error) error {
	r.streamCalls++
	if r.streamFn != nil {
		return r.streamFn(ctx, visit)
	}
	for _, event := range r.stream {
		if err := visit(event); err != nil {
			return err
		}
	}
	return r.streamErr
}

func (r *fakeReportReader) Series(_ context.Context, _ uuid.UUID, from, to time.Time, bucket time.Duration) ([]ch.SeriesPoint, error) {
	r.seriesFrom, r.seriesTo, r.bucket = from, to, bucket
	return append([]ch.SeriesPoint(nil), r.series...), r.seriesErr
}

func (r *fakeReportReader) Stickiness(context.Context, uuid.UUID, time.Time, time.Time) (ch.Stickiness, error) {
	return r.stickiness, r.stickinessErr
}

func (r *fakeReportReader) PoolGrowth(context.Context, uuid.UUID, time.Time, time.Time) ([]ch.GrowthPoint, error) {
	return append([]ch.GrowthPoint(nil), r.growth...), r.growthErr
}

func reportTestServer(t *testing.T, store session.Store, reader *fakeReportReader) *Server {
	t.Helper()
	cipher, err := cryptox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(store, &fakeControl{}, cipher, Defaults{ProbeTarget: testProbeTarget, DialTimeout: 10 * time.Second}, nil, 128, reader)
}
