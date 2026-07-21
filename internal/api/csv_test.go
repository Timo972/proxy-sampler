package api

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/ch"
	"github.com/timo972/proxy-sampler/internal/session"
)

func TestSessionSamplesReturnsFixedNewestFirstPageAndParallelArrays(t *testing.T) {
	value := sampleSession()
	store := &reportStore{memoryStore: newMemoryStore()}
	store.sessions = []session.Session{value}
	newest := sampleEvent(value.ID, 2, time.Date(2026, 7, 21, 12, 1, 0, 0, time.UTC))
	older := sampleEvent(value.ID, 1, time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC))
	reader := &fakeReportReader{samples: ch.SamplePage{Items: []ch.Event{newest, older}, Page: 2, PageSize: 50, Total: 52}}
	from := time.Date(2026, 7, 21, 11, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC)

	response := request(t, reportTestServer(t, store, reader).Handler(), http.MethodGet,
		"/api/sessions/"+value.ID.String()+"/samples?page=2&from="+from.Format(time.RFC3339)+"&to="+to.Format(time.RFC3339), "")

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if reader.samplePage != 2 || reader.sampleFrom == nil || !reader.sampleFrom.Equal(from) || reader.sampleTo == nil || !reader.sampleTo.Equal(to) {
		t.Errorf("reader query = page %d, %v..%v; want page 2, %s..%s", reader.samplePage, reader.sampleFrom, reader.sampleTo, from, to)
	}
	var body struct {
		Items []struct {
			SampleSeq   int64     `json:"sample_seq"`
			ProbeIPs    []*string `json:"probe_ips"`
			ProbeRTTsMS []int     `json:"probe_rtts_ms"`
			ProbeOK     []bool    `json:"probe_ok"`
		} `json:"items"`
		Page     int   `json:"page"`
		PageSize int   `json:"page_size"`
		Total    int64 `json:"total"`
	}
	decodeJSON(t, response, &body)
	if body.Page != 2 || body.PageSize != 50 || body.Total != 52 || len(body.Items) != 2 || body.Items[0].SampleSeq != 2 || body.Items[1].SampleSeq != 1 {
		t.Fatalf("page metadata/order = %#v", body)
	}
	first := body.Items[0]
	if len(first.ProbeIPs) != 3 || len(first.ProbeRTTsMS) != 3 || len(first.ProbeOK) != 3 {
		t.Fatalf("parallel array lengths = %d/%d/%d, want 3/3/3", len(first.ProbeIPs), len(first.ProbeRTTsMS), len(first.ProbeOK))
	}
	if first.ProbeIPs[0] == nil || *first.ProbeIPs[0] != "203.0.113.10" || first.ProbeIPs[1] != nil || !first.ProbeOK[0] || first.ProbeOK[1] {
		t.Errorf("parallel probe arrays = IPs %#v OK %#v", first.ProbeIPs, first.ProbeOK)
	}
}

func TestSessionSamplesDefaultsPageAndValidatesQueries(t *testing.T) {
	value := sampleSession()
	store := &reportStore{memoryStore: newMemoryStore()}
	store.sessions = []session.Session{value}

	t.Run("default page and empty items array", func(t *testing.T) {
		reader := &fakeReportReader{samples: ch.SamplePage{Page: 1, PageSize: 50, Items: []ch.Event{}}}
		response := request(t, reportTestServer(t, store, reader).Handler(), http.MethodGet, "/api/sessions/"+value.ID.String()+"/samples", "")
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d; body=%s", response.Code, response.Body.String())
		}
		if reader.samplePage != 1 {
			t.Errorf("reader page = %d, want 1", reader.samplePage)
		}
		if strings.Contains(response.Body.String(), `"items":null`) {
			t.Errorf("items are null: %s", response.Body.String())
		}
	})

	for _, test := range []struct {
		name, query string
	}{
		{name: "zero page", query: "page=0"},
		{name: "reversed range", query: "from=2026-07-21T12:00:00Z&to=2026-07-21T11:00:00Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := request(t, reportTestServer(t, store, &fakeReportReader{}).Handler(), http.MethodGet, "/api/sessions/"+value.ID.String()+"/samples?"+test.query, "")
			assertAPIError(t, response, http.StatusBadRequest, errorCodeInvalidRequest)
		})
	}

	t.Run("missing session", func(t *testing.T) {
		response := request(t, reportTestServer(t, &reportStore{memoryStore: newMemoryStore()}, &fakeReportReader{}).Handler(), http.MethodGet, "/api/sessions/"+value.ID.String()+"/samples", "")
		assertAPIError(t, response, http.StatusNotFound, errorCodeNotFound)
	})

	t.Run("dependency error", func(t *testing.T) {
		reader := &fakeReportReader{samplesErr: errors.New("clickhouse dsn secret")}
		response := request(t, reportTestServer(t, store, reader).Handler(), http.MethodGet, "/api/sessions/"+value.ID.String()+"/samples", "")
		assertAPIError(t, response, http.StatusServiceUnavailable, errorCodeDependencyUnavailable)
		if strings.Contains(response.Body.String(), "clickhouse dsn secret") {
			t.Errorf("dependency detail leaked: %s", response.Body.String())
		}
	})

	t.Run("reader rejects oversized page", func(t *testing.T) {
		reader := &fakeReportReader{samplesErr: ch.ErrInvalidPage}
		response := request(t, reportTestServer(t, store, reader).Handler(), http.MethodGet, "/api/sessions/"+value.ID.String()+"/samples?page=9223372036854775807", "")
		assertAPIError(t, response, http.StatusBadRequest, errorCodeInvalidRequest)
	})
}

func TestExportSessionCSVWritesChronologicalPerProbeRowsAndQuotesErrors(t *testing.T) {
	value := sampleSession()
	value.Name = "  Team / Frankfurt,\nProxy  "
	store := &reportStore{memoryStore: newMemoryStore()}
	store.sessions = []session.Session{value}
	first := sampleEvent(value.ID, 1, time.Date(2026, 7, 21, 12, 0, 0, 123456789, time.UTC))
	first.Error = "upstream reset, retry\nfailed"
	second := sampleEvent(value.ID, 2, time.Date(2026, 7, 21, 12, 1, 0, 0, time.UTC))
	reader := &fakeReportReader{stream: []ch.Event{first, second}}

	response := request(t, reportTestServer(t, store, reader).Handler(), http.MethodGet, "/api/sessions/"+value.ID.String()+"/export.csv", "")

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	wantDisposition := `attachment; filename="Team-Frankfurt-Proxy-` + value.ID.String() + `.csv"`
	if got := response.Header().Get("Content-Disposition"); got != wantDisposition {
		t.Errorf("Content-Disposition = %q, want %q", got, wantDisposition)
	}
	parsed, err := csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v\n%s", err, response.Body.String())
	}
	wantHeader := []string{"sample_seq", "sampled_at", "probe_index", "probe_ok", "probe_ip", "probe_rtt_ms", "primary_ip", "sample_probes_ok", "sample_probes_attempted", "rtt_min_ms", "rtt_med_ms", "rtt_max_ms", "ip_changed", "new_ips", "primary_category", "primary_risk", "error"}
	if strings.Join(parsed[0], ",") != strings.Join(wantHeader, ",") {
		t.Fatalf("CSV header = %#v, want %#v", parsed[0], wantHeader)
	}
	if len(parsed) != 7 {
		t.Fatalf("CSV rows including header = %d, want 7\n%s", len(parsed), response.Body.String())
	}
	if parsed[1][0] != "1" || parsed[1][2] != "0" || parsed[4][0] != "2" {
		t.Errorf("CSV chronology/index = first %#v, fourth %#v", parsed[1], parsed[4])
	}
	if parsed[2][3] != "false" || parsed[2][4] != "" {
		t.Errorf("failed probe fields = %#v, want false and empty IP", parsed[2])
	}
	if parsed[1][1] != "2026-07-21T12:00:00.123456789Z" || parsed[1][16] != first.Error {
		t.Errorf("timestamp/error = %q/%q", parsed[1][1], parsed[1][16])
	}
	if !strings.Contains(response.Body.String(), `"upstream reset, retry`+"\n"+`failed"`) {
		t.Errorf("CSV did not quote comma/newline error: %s", response.Body.String())
	}
	if reader.streamCalls != 1 || reader.sampleCalls != 0 {
		t.Errorf("reader calls stream/samples = %d/%d, want 1/0", reader.streamCalls, reader.sampleCalls)
	}
}

func TestExportSessionCSVStreamsBeforeSourceCompletes(t *testing.T) {
	value := sampleSession()
	store := &reportStore{memoryStore: newMemoryStore()}
	store.sessions = []session.Session{value}
	visited := make(chan struct{})
	release := make(chan struct{})
	reader := &fakeReportReader{streamFn: func(ctx context.Context, visit func(ch.Event) error) error {
		if err := visit(sampleEvent(value.ID, 1, time.Now().UTC())); err != nil {
			return err
		}
		close(visited)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	server := httptest.NewServer(reportTestServer(t, store, reader).Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/api/sessions/" + value.ID.String() + "/export.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	select {
	case <-visited:
	case <-time.After(time.Second):
		t.Fatal("stream source did not visit the first event")
	}
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("read streamed header: %v", err)
	}
	if !strings.HasPrefix(line, "sample_seq,sampled_at,") {
		t.Fatalf("streamed first line = %q", line)
	}
	close(release)
}

func TestExportSessionCSVValidatesBeforeStreamingAndMapsInitialFailure(t *testing.T) {
	value := sampleSession()
	store := &reportStore{memoryStore: newMemoryStore()}
	store.sessions = []session.Session{value}

	t.Run("reversed range", func(t *testing.T) {
		reader := &fakeReportReader{}
		response := request(t, reportTestServer(t, store, reader).Handler(), http.MethodGet,
			"/api/sessions/"+value.ID.String()+"/export.csv?from=2026-07-21T12:00:00Z&to=2026-07-21T11:00:00Z", "")
		assertAPIError(t, response, http.StatusBadRequest, errorCodeInvalidRequest)
		if reader.streamCalls != 0 {
			t.Errorf("stream calls = %d, want 0", reader.streamCalls)
		}
	})

	t.Run("missing session", func(t *testing.T) {
		response := request(t, reportTestServer(t, &reportStore{memoryStore: newMemoryStore()}, &fakeReportReader{}).Handler(), http.MethodGet, "/api/sessions/"+value.ID.String()+"/export.csv", "")
		assertAPIError(t, response, http.StatusNotFound, errorCodeNotFound)
	})

	t.Run("dependency error before rows", func(t *testing.T) {
		reader := &fakeReportReader{streamErr: errors.New("clickhouse://user:password@host")}
		response := request(t, reportTestServer(t, store, reader).Handler(), http.MethodGet, "/api/sessions/"+value.ID.String()+"/export.csv", "")
		assertAPIError(t, response, http.StatusServiceUnavailable, errorCodeDependencyUnavailable)
		if strings.Contains(response.Body.String(), "password") {
			t.Errorf("dependency detail leaked: %s", response.Body.String())
		}
	})
}

func TestExportSessionCSVDoesNotAppendJSONAfterMidstreamFailure(t *testing.T) {
	value := sampleSession()
	store := &reportStore{memoryStore: newMemoryStore()}
	store.sessions = []session.Session{value}
	reader := &fakeReportReader{
		stream:    []ch.Event{sampleEvent(value.ID, 1, time.Now().UTC())},
		streamErr: errors.New("clickhouse://user:password@host"),
	}

	response := request(t, reportTestServer(t, store, reader).Handler(), http.MethodGet, "/api/sessions/"+value.ID.String()+"/export.csv", "")

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want already-started 200; body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "internal_error") || strings.Contains(response.Body.String(), "password") {
		t.Errorf("midstream failure corrupted or leaked into CSV: %s", response.Body.String())
	}
}

func sampleEvent(id uuid.UUID, sequence uint32, sampledAt time.Time) ch.Event {
	return ch.Event{
		SessionID: id, SampleSeq: sequence, SampledAt: sampledAt,
		ProbesAttempted: 3, ProbesOK: 2, PrimaryIP: net.ParseIP("203.0.113.10"),
		DistinctIPs: 2, IPChanged: 1, NewIPs: 1,
		RTTMinMS: 10, RTTMedMS: 20, RTTMaxMS: 30,
		EgressCountry: "DE", PrimaryCategory: "residential", PrimaryRisk: 42,
		ProbeIPs:    []net.IP{net.ParseIP("203.0.113.10"), net.IPv6unspecified, net.ParseIP("203.0.113.11")},
		ProbeRTTsMS: []uint32{10, 0, 30}, ProbeOK: []uint8{1, 0, 1},
	}
}
