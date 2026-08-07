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

func TestBuildPoolReportFlaggedIncludesProxyCheckAndSFS(t *testing.T) {
	s1 := uuid.New()
	yes := true
	variants := []VariantSession{
		{SessionID: s1, CellKey: `{}`, Params: json.RawMessage(`{}`), Snapshot: session.Snapshot{SamplesTaken: 1}},
	}
	obs := []IPObservation{
		{SessionID: s1, IP: netip.MustParseAddr("4.4.4.1"), HitCount: 1, Reputation: &session.Reputation{ProxyCheckProxy: &yes, Category: "residential"}},
		{SessionID: s1, IP: netip.MustParseAddr("4.4.4.2"), HitCount: 1, Reputation: &session.Reputation{SFSAppears: &yes, Category: "residential"}},
		{SessionID: s1, IP: netip.MustParseAddr("4.4.4.3"), HitCount: 1, Reputation: &session.Reputation{Category: "residential"}}, // clean
	}
	report := BuildPoolReport(variants, obs)
	if report.FlaggedIPs != 2 {
		t.Fatalf("flagged ips = %d, want 2 (ProxyCheck + SFS), matching session report", report.FlaggedIPs)
	}
}

func TestBuildPoolReportHonorWeightsByHitsNotIPCount(t *testing.T) {
	s1 := uuid.New()
	variants := []VariantSession{
		{SessionID: s1, CellKey: `{"country":"de"}`, Params: json.RawMessage(`{"country":"de"}`), Snapshot: session.Snapshot{SamplesTaken: 5}},
	}
	// DE: 1 IP with 4 hits (weight 4). US: 3 IPs with 1 hit each (weight 3).
	// Actual traffic favors DE, so requested de is honored. Under the old
	// hit_count+1-per-IP weighting US would win (5 vs 6) and mark it a mismatch.
	obs := []IPObservation{
		{SessionID: s1, IP: netip.MustParseAddr("6.6.6.1"), HitCount: 4, Reputation: rep("DE", "", "residential")},
		{SessionID: s1, IP: netip.MustParseAddr("6.6.6.2"), HitCount: 1, Reputation: rep("US", "", "residential")},
		{SessionID: s1, IP: netip.MustParseAddr("6.6.6.3"), HitCount: 1, Reputation: rep("US", "", "residential")},
		{SessionID: s1, IP: netip.MustParseAddr("6.6.6.4"), HitCount: 1, Reputation: rep("US", "", "residential")},
	}
	report := BuildPoolReport(variants, obs)
	if report.HonorRate == nil || *report.HonorRate != 1 {
		t.Fatalf("honor rate = %v, want 1 (DE has more actual hits than US)", report.HonorRate)
	}
}

func TestBuildPoolReportCellParamsExcludeRandomValues(t *testing.T) {
	s1 := uuid.New()
	variants := []VariantSession{
		{SessionID: s1, CellKey: `{"country":"de"}`, Params: json.RawMessage(`{"country":"de","session":"abc123"}`), Snapshot: session.Snapshot{SamplesTaken: 1}},
	}
	obs := []IPObservation{{SessionID: s1, IP: netip.MustParseAddr("7.7.7.7"), HitCount: 1, Reputation: rep("DE", "", "residential")}}
	report := BuildPoolReport(variants, obs)
	if len(report.Cells) != 1 {
		t.Fatalf("cells = %d, want 1", len(report.Cells))
	}
	var params map[string]string
	if err := json.Unmarshal(report.Cells[0].Params, &params); err != nil {
		t.Fatal(err)
	}
	if _, leaked := params["session"]; leaked {
		t.Fatalf("cell params leaked the random axis value: %v", params)
	}
	if params["country"] != "de" || len(params) != 1 {
		t.Fatalf("cell params = %v, want {country: de}", params)
	}
}

func TestBuildPoolReportHonorUnknownWhenNoTarget(t *testing.T) {
	s1 := uuid.New()
	// A random/port-only variant targets no country or ISP, so its honor
	// outcome is unknown and it must not inflate the rate to 100%.
	variants := []VariantSession{
		{SessionID: s1, CellKey: `{}`, Params: json.RawMessage(`{"session":"abc"}`), Snapshot: session.Snapshot{SamplesTaken: 3}},
	}
	obs := []IPObservation{{SessionID: s1, IP: netip.MustParseAddr("9.9.9.1"), HitCount: 3, Reputation: rep("DE", "ISP-1", "residential")}}
	report := BuildPoolReport(variants, obs)
	if report.HonorRate != nil {
		t.Fatalf("honor rate = %v, want nil (no country/isp target)", report.HonorRate)
	}
}

func TestBuildPoolReportHonorUnknownWhenObservedValueMissing(t *testing.T) {
	s1 := uuid.New()
	// Requests an ISP, but the observed reputation carries no ISP, so there is
	// nothing to compare: unknown, excluded from the denominator.
	variants := []VariantSession{
		{SessionID: s1, CellKey: `{"isp":"ACME"}`, Params: json.RawMessage(`{"isp":"ACME"}`), Snapshot: session.Snapshot{SamplesTaken: 3}},
	}
	obs := []IPObservation{{SessionID: s1, IP: netip.MustParseAddr("9.9.9.2"), HitCount: 3, Reputation: rep("DE", "", "residential")}}
	report := BuildPoolReport(variants, obs)
	if report.HonorRate != nil {
		t.Fatalf("honor rate = %v, want nil (requested isp not observable)", report.HonorRate)
	}
}

func TestBuildPoolReportHonorUnknownOnDominantTie(t *testing.T) {
	s1 := uuid.New()
	variants := []VariantSession{
		{SessionID: s1, CellKey: `{"country":"de"}`, Params: json.RawMessage(`{"country":"de"}`), Snapshot: session.Snapshot{SamplesTaken: 3}},
	}
	// Two countries with equal hit weight: no unique dominant, so the outcome
	// is unknown and deterministic (not a coin-flip on map iteration order).
	obs := []IPObservation{
		{SessionID: s1, IP: netip.MustParseAddr("9.9.9.3"), HitCount: 2, Reputation: rep("DE", "", "residential")},
		{SessionID: s1, IP: netip.MustParseAddr("9.9.9.4"), HitCount: 2, Reputation: rep("US", "", "residential")},
	}
	report := BuildPoolReport(variants, obs)
	if report.HonorRate != nil {
		t.Fatalf("honor rate = %v, want nil (dominant country tie is unknown)", report.HonorRate)
	}
}

func TestBuildPoolReportHonorExcludesUnknownFromDenominator(t *testing.T) {
	s1, s2 := uuid.New(), uuid.New()
	variants := []VariantSession{
		{SessionID: s1, CellKey: `{"country":"de"}`, Params: json.RawMessage(`{"country":"de"}`), Snapshot: session.Snapshot{SamplesTaken: 3}},
		{SessionID: s2, CellKey: `{}`, Params: json.RawMessage(`{"session":"x"}`), Snapshot: session.Snapshot{SamplesTaken: 3}}, // untargeted -> unknown
	}
	obs := []IPObservation{
		{SessionID: s1, IP: netip.MustParseAddr("9.9.9.5"), HitCount: 3, Reputation: rep("DE", "", "residential")},
		{SessionID: s2, IP: netip.MustParseAddr("9.9.9.6"), HitCount: 3, Reputation: rep("US", "", "residential")},
	}
	report := BuildPoolReport(variants, obs)
	// Only the targeted, matched variant counts: 1/1 = 1; the untargeted
	// variant is excluded rather than diluting the rate.
	if report.HonorRate == nil || *report.HonorRate != 1 {
		t.Fatalf("honor rate = %v, want 1 (untargeted variant excluded)", report.HonorRate)
	}
}

func TestBuildPoolReportHonorCountsObservedVariantAfterReenable(t *testing.T) {
	s1 := uuid.New()
	// A re-enable resets SamplesTaken to 0 but preserves session_ips, so an
	// already-observed variant must still count toward the honor denominator.
	variants := []VariantSession{
		{SessionID: s1, CellKey: `{"country":"de"}`, Params: json.RawMessage(`{"country":"de"}`), Snapshot: session.Snapshot{SamplesTaken: 0}},
	}
	obs := []IPObservation{{SessionID: s1, IP: netip.MustParseAddr("8.8.8.8"), HitCount: 2, Reputation: rep("DE", "", "residential")}}
	report := BuildPoolReport(variants, obs)
	if report.HonorRate == nil || *report.HonorRate != 1 {
		t.Fatalf("honor rate = %v, want 1 (observed variant counts despite SamplesTaken=0)", report.HonorRate)
	}
}
