package ch

import (
	"context"
	"errors"
	"math"
	"net"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/chmigrate"
)

func TestReaderRejectsInvalidRangesAndBucket(t *testing.T) {
	r := &Reader{}
	from := time.Date(2026, 7, 21, 12, 0, 1, 0, time.UTC)
	to := from.Add(-time.Second)
	if _, err := r.Series(context.Background(), uuid.New(), from, to, time.Second); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("Series reversed range error = %v", err)
	}
	if _, err := r.Series(context.Background(), uuid.New(), to, from, 0); !errors.Is(err, ErrInvalidBucket) {
		t.Fatalf("Series zero bucket error = %v", err)
	}
	if _, err := r.Stickiness(context.Background(), uuid.New(), from, to); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("Stickiness reversed range error = %v", err)
	}
	if _, err := r.PoolGrowth(context.Background(), uuid.New(), from, to); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("PoolGrowth reversed range error = %v", err)
	}
}

func TestReaderRejectsHugePageBeforeQuery(t *testing.T) {
	r := &Reader{}
	_, err := r.Samples(context.Background(), uuid.New(), nil, nil, math.MaxInt)
	if !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("Samples huge page error = %v, want ErrInvalidPage", err)
	}
}

func TestDeleteSessionsRemovesSamplesInOneMutation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, sessionID, _ := integrationReaderFixture(t, ctx)

	if err := r.DeleteSessions(ctx, []uuid.UUID{sessionID}); err != nil {
		t.Fatalf("DeleteSessions: %v", err)
	}
	page, err := r.Samples(ctx, sessionID, nil, nil, 1)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if page.Total != 0 {
		t.Fatalf("total after batch delete = %d, want 0", page.Total)
	}
}

func TestReaderReportsAndDeletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, sessionID, events := integrationReaderFixture(t, ctx)

	t.Run("samples newest first with fixed page size", func(t *testing.T) {
		page, err := r.Samples(ctx, sessionID, nil, nil, 1)
		if err != nil {
			t.Fatalf("Samples: %v", err)
		}
		if page.Page != 1 || page.PageSize != 50 || page.Total != 5 || len(page.Items) != 5 {
			t.Fatalf("page = %#v", page)
		}
		for i, want := range []uint32{5, 4, 3, 2, 1} {
			if got := page.Items[i].SampleSeq; got != want {
				t.Errorf("Items[%d].SampleSeq = %d, want %d", i, got, want)
			}
		}
	})

	t.Run("samples apply optional inclusive time filters", func(t *testing.T) {
		from, to := events[1].SampledAt, events[3].SampledAt
		page, err := r.Samples(ctx, sessionID, &from, &to, 1)
		if err != nil {
			t.Fatalf("Samples: %v", err)
		}
		if page.Total != 3 || len(page.Items) != 3 {
			t.Fatalf("filtered page = %#v", page)
		}
		for i, want := range []uint32{4, 3, 2} {
			if got := page.Items[i].SampleSeq; got != want {
				t.Errorf("Items[%d].SampleSeq = %d, want %d", i, got, want)
			}
		}
	})

	t.Run("stream is chronological and complete", func(t *testing.T) {
		to := events[3].SampledAt
		var got []uint32
		err := r.StreamSamples(ctx, sessionID, nil, &to, func(event Event) error {
			got = append(got, event.SampleSeq)
			return nil
		})
		if err != nil {
			t.Fatalf("StreamSamples: %v", err)
		}
		assertUint32s(t, got, []uint32{1, 2, 3, 4})

		visitErr := errors.New("stop")
		if err := r.StreamSamples(ctx, sessionID, nil, nil, func(Event) error { return visitErr }); !errors.Is(err, visitErr) {
			t.Fatalf("visitor error = %v", err)
		}
	})

	t.Run("series uses successful samples for latency quantiles", func(t *testing.T) {
		points, err := r.Series(ctx, sessionID, events[0].SampledAt, events[4].SampledAt, time.Minute)
		if err != nil {
			t.Fatalf("Series: %v", err)
		}
		if len(points) != 1 {
			t.Fatalf("series points = %#v", points)
		}
		point := points[0]
		assertFloat(t, "success rate", point.SuccessRate, 0.7)
		assertFloat(t, "latency p50", point.LatencyP50MS, 30)
		assertFloat(t, "latency p95", point.LatencyP95MS, 40)
		assertFloat(t, "distinct per sample", point.DistinctPerSample, 0.8)
		if point.IPChanges != 1 || point.Residential != 3 || point.Mobile != 2 || point.Datacenter != 0 || point.Unknown != 0 {
			t.Fatalf("series counts = %#v", point)
		}
	})

	t.Run("stickiness ignores failed samples", func(t *testing.T) {
		got, err := r.Stickiness(ctx, sessionID, events[0].SampledAt, events[4].SampledAt)
		if err != nil {
			t.Fatalf("Stickiness: %v", err)
		}
		if len(got.Holds) != 2 || len(got.Rotations) != 1 {
			t.Fatalf("stickiness = %#v", got)
		}
		if got.Holds[0].IP != "2001:db8::1" || got.Holds[0].Samples != 2 || got.Holds[0].DurationSeconds != 10 {
			t.Errorf("first hold = %#v", got.Holds[0])
		}
		if got.Holds[1].IP != "2001:db8::2" || got.Holds[1].Samples != 2 || got.Holds[1].DurationSeconds != 10 {
			t.Errorf("second hold = %#v", got.Holds[1])
		}
		rotation := got.Rotations[0]
		if rotation.At != events[3].SampledAt || rotation.FromIP != "2001:db8::1" || rotation.ToIP != "2001:db8::2" || rotation.SincePreviousSeconds != 30 {
			t.Errorf("rotation = %#v", rotation)
		}
		assertFloat(t, "average hold", got.AverageHoldSeconds, 10)
		assertFloat(t, "median hold", got.MedianHoldSeconds, 10)
	})

	t.Run("pool growth is chronological and cumulative", func(t *testing.T) {
		got, err := r.PoolGrowth(ctx, sessionID, events[0].SampledAt, events[4].SampledAt)
		if err != nil {
			t.Fatalf("PoolGrowth: %v", err)
		}
		if len(got) != 5 {
			t.Fatalf("growth = %#v", got)
		}
		for i, want := range []uint64{1, 1, 1, 2, 2} {
			if got[i].At != events[i].SampledAt || got[i].DistinctIPs != want {
				t.Errorf("growth[%d] = %#v, want at %s total %d", i, got[i], events[i].SampledAt, want)
			}
		}
	})

	t.Run("delete waits for mutation", func(t *testing.T) {
		if err := r.DeleteSession(ctx, sessionID); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		page, err := r.Samples(ctx, sessionID, nil, nil, 1)
		if err != nil {
			t.Fatalf("Samples after delete: %v", err)
		}
		if page.Total != 0 || len(page.Items) != 0 {
			t.Fatalf("samples remain after synchronous deletion: %#v", page)
		}
	})
}

func integrationReaderFixture(t *testing.T, ctx context.Context) (*Reader, uuid.UUID, []Event) {
	t.Helper()
	dsn := os.Getenv("TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("TEST_CLICKHOUSE_DSN is not set")
	}
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse TEST_CLICKHOUSE_DSN: %v", err)
	}
	if err := chmigrate.Up(ctx, opts, discardLogger()); err != nil {
		t.Fatalf("migrate ClickHouse: %v", err)
	}
	r, err := NewReader(ctx, opts)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = r.DeleteSession(cleanupCtx, fixtureSessionID)
		if err := r.Close(); err != nil {
			t.Errorf("Reader.Close: %v", err)
		}
	})
	if err := r.DeleteSession(ctx, fixtureSessionID); err != nil {
		t.Fatalf("clear fixture session: %v", err)
	}

	events := reportFixtureEvents()
	w, err := NewWriter(ctx, opts, discardLogger())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i, event := range events {
		if !w.Enqueue(event) {
			t.Fatalf("enqueue fixture %d rejected", i)
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush fixture: %v", err)
	}
	if err := w.Close(ctx); err != nil {
		t.Fatalf("close fixture writer: %v", err)
	}
	return r, fixtureSessionID, events
}

var fixtureSessionID = uuid.MustParse("66666666-6666-4666-8666-666666666666")

func reportFixtureEvents() []Event {
	start := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	rows := []struct {
		ip          string
		ok          uint8
		median      uint32
		category    string
		distinct    uint8
		changed     uint8
		newIPs      uint8
		probeOK     []uint8
		probeIPText []string
	}{
		{"2001:db8::1", 2, 10, "residential", 1, 0, 1, []uint8{1, 1}, []string{"2001:db8::1", "2001:db8::1"}},
		{"2001:db8::1", 1, 20, "residential", 1, 0, 0, []uint8{1, 0}, []string{"2001:db8::1", ""}},
		{"", 0, 999, "residential", 0, 0, 0, []uint8{0, 0}, []string{"", ""}},
		{"2001:db8::2", 2, 30, "mobile", 1, 1, 1, []uint8{1, 1}, []string{"2001:db8::2", "2001:db8::2"}},
		{"2001:db8::2", 2, 40, "mobile", 1, 0, 0, []uint8{1, 1}, []string{"2001:db8::2", "2001:db8::2"}},
	}
	events := make([]Event, len(rows))
	for i, row := range rows {
		probeIPs := make([]net.IP, len(row.probeIPText))
		for j, text := range row.probeIPText {
			probeIPs[j] = net.ParseIP(text)
		}
		events[i] = Event{
			SessionID:       fixtureSessionID,
			SampledAt:       start.Add(time.Duration(i) * 10 * time.Second),
			SampleSeq:       uint32(i + 1),
			ProbesAttempted: 2,
			ProbesOK:        row.ok,
			PrimaryIP:       net.ParseIP(row.ip),
			DistinctIPs:     row.distinct,
			IPChanged:       row.changed,
			NewIPs:          row.newIPs,
			RTTMinMS:        row.median,
			RTTMedMS:        row.median,
			RTTMaxMS:        row.median,
			EgressCountry:   "DE",
			PrimaryCategory: row.category,
			ProbeIPs:        probeIPs,
			ProbeRTTsMS:     []uint32{row.median, row.median},
			ProbeOK:         row.probeOK,
		}
	}
	return events
}

func assertFloat(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.000001 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func assertUint32s(t *testing.T, got, want []uint32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("values = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("values[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestSeriesForSessionsEmptyIDs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, _, _ := integrationReaderFixture(t, ctx)

	points, err := r.SeriesForSessions(ctx, nil, time.Now().Add(-time.Hour), time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("SeriesForSessions: %v", err)
	}
	if len(points) != 0 {
		t.Fatalf("expected no points for empty ids, got %d", len(points))
	}
}

func TestSeriesForSessionsAggregatesAcrossSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dsn := os.Getenv("TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("TEST_CLICKHOUSE_DSN is not set")
	}
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse TEST_CLICKHOUSE_DSN: %v", err)
	}
	if err := chmigrate.Up(ctx, opts, discardLogger()); err != nil {
		t.Fatalf("migrate ClickHouse: %v", err)
	}
	r, err := NewReader(ctx, opts)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("Reader.Close: %v", err)
		}
	})

	s1, s2 := uuid.New(), uuid.New()
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = r.DeleteSession(cleanupCtx, s1)
		_ = r.DeleteSession(cleanupCtx, s2)
	})

	base := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	events := seriesForSessionsFixtureEvents(s1, s2, base)

	w, err := NewWriter(ctx, opts, discardLogger())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i, event := range events {
		if !w.Enqueue(event) {
			t.Fatalf("enqueue event %d rejected", i)
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush events: %v", err)
	}
	if err := w.Close(ctx); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	points, err := r.SeriesForSessions(ctx, []uuid.UUID{s1, s2}, base.Add(-time.Hour), base.Add(time.Hour), 5*time.Minute)
	if err != nil {
		t.Fatalf("SeriesForSessions: %v", err)
	}
	if len(points) == 0 {
		t.Fatal("expected aggregated series points across both sessions")
	}
}

func seriesForSessionsFixtureEvents(s1, s2 uuid.UUID, base time.Time) []Event {
	makeEvent := func(sessionID uuid.UUID, seq uint32, offset time.Duration) Event {
		return Event{
			SessionID:       sessionID,
			SampledAt:       base.Add(offset),
			SampleSeq:       seq,
			ProbesAttempted: 2,
			ProbesOK:        2,
			PrimaryIP:       net.ParseIP("2001:db8::1"),
			DistinctIPs:     1,
			IPChanged:       0,
			NewIPs:          1,
			RTTMinMS:        10,
			RTTMedMS:        10,
			RTTMaxMS:        10,
			EgressCountry:   "DE",
			PrimaryCategory: "residential",
			ProbeIPs:        []net.IP{net.ParseIP("2001:db8::1"), net.ParseIP("2001:db8::1")},
			ProbeRTTsMS:     []uint32{10, 10},
			ProbeOK:         []uint8{1, 1},
		}
	}
	return []Event{
		makeEvent(s1, 1, 0),
		makeEvent(s1, 2, 10*time.Second),
		makeEvent(s2, 1, 0),
		makeEvent(s2, 2, 10*time.Second),
	}
}
