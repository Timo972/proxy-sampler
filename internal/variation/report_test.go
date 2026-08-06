package variation

import (
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/session"
)

func rep(country, isp, category string) *session.Reputation {
	return &session.Reputation{Country: country, ISP: isp, Category: category}
}

func TestBuildPoolReportDedupesAndCounts(t *testing.T) {
	s1, s2 := uuid.New(), uuid.New()
	ipA := netip.MustParseAddr("1.1.1.1")
	ipB := netip.MustParseAddr("2.2.2.2")
	variants := []VariantSession{
		{SessionID: s1, CellKey: `{"country":"de"}`, Params: json.RawMessage(`{"country":"de"}`), Snapshot: session.Snapshot{SamplesTaken: 3}},
		{SessionID: s2, CellKey: `{"country":"de"}`, Params: json.RawMessage(`{"country":"de"}`), Snapshot: session.Snapshot{SamplesTaken: 3}},
	}
	obs := []IPObservation{
		{SessionID: s1, IP: ipA, HitCount: 4, Reputation: rep("DE", "ISP-1", "residential")},
		{SessionID: s2, IP: ipA, HitCount: 2, Reputation: rep("DE", "ISP-1", "residential")}, // shared IP
		{SessionID: s2, IP: ipB, HitCount: 1, Reputation: rep("DE", "ISP-1", "residential")},
	}
	report := BuildPoolReport(variants, obs)
	if report.DistinctIPs != 2 {
		t.Fatalf("distinct ips = %d, want 2 (deduped)", report.DistinctIPs)
	}
	if report.Composition.Residential != 2 {
		t.Fatalf("residential = %d, want 2", report.Composition.Residential)
	}
	if len(report.Cells) != 1 {
		t.Fatalf("cells = %d, want 1", len(report.Cells))
	}
	// Both variants requested country=de, dominant observed DE -> honored.
	if report.HonorRate == nil || *report.HonorRate != 1 {
		t.Fatalf("honor rate = %v, want 1", report.HonorRate)
	}
}

func TestBuildPoolReportHonorMismatch(t *testing.T) {
	s1 := uuid.New()
	variants := []VariantSession{
		{SessionID: s1, CellKey: `{"country":"jp"}`, Params: json.RawMessage(`{"country":"jp"}`), Snapshot: session.Snapshot{SamplesTaken: 2}},
	}
	obs := []IPObservation{
		{SessionID: s1, IP: netip.MustParseAddr("3.3.3.3"), HitCount: 5, Reputation: rep("US", "ISP-2", "datacenter")},
	}
	report := BuildPoolReport(variants, obs)
	if report.HonorRate == nil || *report.HonorRate != 0 {
		t.Fatalf("honor rate = %v, want 0 (requested jp, observed US)", report.HonorRate)
	}
}

func TestBuildPoolReportHonorUnknownWithoutSamples(t *testing.T) {
	s1 := uuid.New()
	variants := []VariantSession{
		{SessionID: s1, CellKey: `{"country":"de"}`, Params: json.RawMessage(`{"country":"de"}`), Snapshot: session.Snapshot{SamplesTaken: 0}},
	}
	report := BuildPoolReport(variants, nil)
	if report.HonorRate != nil {
		t.Fatalf("honor rate = %v, want nil (no samples)", report.HonorRate)
	}
	if report.EstimatedPoolSize != 0 {
		t.Fatalf("pool size = %d, want 0", report.EstimatedPoolSize)
	}
}
