package sampler

import (
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestAggregateFoldsFailures(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.7")
	results := []ProbeResult{
		{IP: ip, Country: "US", RTT: 100 * time.Millisecond},
		{Err: errors.New("timeout"), RTT: time.Second},
		{IP: ip, Country: "US", RTT: 200 * time.Millisecond},
	}
	sample := Aggregate(results, netip.Addr{}, map[netip.Addr]struct{}{})

	if sample.ProbesAttempted != 3 || sample.ProbesOK != 2 {
		t.Fatalf("probe counts = %d/%d", sample.ProbesOK, sample.ProbesAttempted)
	}
	if sample.RTTMin != 100*time.Millisecond || sample.RTTMed != 150*time.Millisecond || sample.RTTMax != 200*time.Millisecond {
		t.Fatalf("RTTs = %v/%v/%v", sample.RTTMin, sample.RTTMed, sample.RTTMax)
	}
	if sample.Error != "" {
		t.Fatalf("partial-success error = %q, want empty", sample.Error)
	}
	if !reflect.DeepEqual(sample.ProbeIPs, []netip.Addr{ip, {}, ip}) {
		t.Fatalf("ProbeIPs = %#v", sample.ProbeIPs)
	}
	if !reflect.DeepEqual(sample.ProbeRTTs, []time.Duration{100 * time.Millisecond, 0, 200 * time.Millisecond}) {
		t.Fatalf("ProbeRTTs = %#v", sample.ProbeRTTs)
	}
	if !reflect.DeepEqual(sample.ProbeOK, []bool{true, false, true}) {
		t.Fatalf("ProbeOK = %#v", sample.ProbeOK)
	}
}

func TestAggregateChoosesModeWithFirstObservedTieBreak(t *testing.T) {
	first := netip.MustParseAddr("2001:db8::2")
	second := netip.MustParseAddr("2001:db8::1")
	results := []ProbeResult{
		{IP: first, Country: "DE", RTT: 40 * time.Millisecond},
		{IP: second, Country: "US", RTT: 10 * time.Millisecond},
		{IP: second, Country: "US", RTT: 20 * time.Millisecond},
		{IP: first, Country: "DE", RTT: 30 * time.Millisecond},
	}

	sample := Aggregate(results, netip.Addr{}, nil)
	if sample.PrimaryIP != first {
		t.Fatalf("PrimaryIP = %v, want earliest tied IP %v", sample.PrimaryIP, first)
	}
	if sample.EgressCountry != "DE" {
		t.Fatalf("EgressCountry = %q, want primary IP's country", sample.EgressCountry)
	}
	if sample.DistinctIPs != 2 {
		t.Fatalf("DistinctIPs = %d, want 2", sample.DistinctIPs)
	}
	wantHits := map[netip.Addr]int{first: 2, second: 2}
	if !reflect.DeepEqual(sample.IPHits, wantHits) {
		t.Fatalf("IPHits = %#v, want %#v", sample.IPHits, wantHits)
	}
	if sample.RTTMin != 10*time.Millisecond || sample.RTTMed != 25*time.Millisecond || sample.RTTMax != 40*time.Millisecond {
		t.Fatalf("RTTs = %v/%v/%v", sample.RTTMin, sample.RTTMed, sample.RTTMax)
	}
	if !reflect.DeepEqual(sample.ProbeIPs, []netip.Addr{first, second, second, first}) {
		t.Fatalf("ProbeIPs lost input order: %#v", sample.ProbeIPs)
	}
}

func TestAggregateIPChangedRequiresTwoValidDifferentIPs(t *testing.T) {
	current := netip.MustParseAddr("203.0.113.7")
	other := netip.MustParseAddr("203.0.113.8")
	tests := []struct {
		name     string
		results  []ProbeResult
		previous netip.Addr
		want     bool
	}{
		{name: "different", results: []ProbeResult{{IP: current}}, previous: other, want: true},
		{name: "same", results: []ProbeResult{{IP: current}}, previous: current},
		{name: "invalid previous", results: []ProbeResult{{IP: current}}},
		{name: "invalid current", results: []ProbeResult{{Err: errors.New("timeout")}}, previous: other},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sample := Aggregate(tt.results, tt.previous, nil)
			if sample.IPChanged != tt.want {
				t.Fatalf("IPChanged = %v, want %v", sample.IPChanged, tt.want)
			}
		})
	}
}

func TestAggregateCountsDistinctNewSessionIPsWithoutMutatingSeen(t *testing.T) {
	seenIP := netip.MustParseAddr("203.0.113.1")
	newA := netip.MustParseAddr("203.0.113.2")
	newB := netip.MustParseAddr("203.0.113.3")
	seen := map[netip.Addr]struct{}{seenIP: {}}
	results := []ProbeResult{{IP: seenIP}, {IP: newA}, {IP: newA}, {IP: newB}}

	sample := Aggregate(results, netip.Addr{}, seen)
	if sample.NewIPs != 2 {
		t.Fatalf("NewIPs = %d, want 2", sample.NewIPs)
	}
	if len(seen) != 1 {
		t.Fatalf("Aggregate mutated seen set: %#v", seen)
	}
}

func TestAggregateTreatsOnlyErrorFreeValidIPsAsSuccess(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.7")
	results := []ProbeResult{
		{},
		{IP: ip, RTT: time.Second, Err: errors.New("bad response")},
		{IP: ip, Country: "US", RTT: 25 * time.Millisecond},
	}

	sample := Aggregate(results, netip.Addr{}, nil)
	if sample.ProbesOK != 1 || sample.PrimaryIP != ip {
		t.Fatalf("sample = %#v", sample)
	}
	if !reflect.DeepEqual(sample.ProbeOK, []bool{false, false, true}) {
		t.Fatalf("ProbeOK = %#v", sample.ProbeOK)
	}
	if !reflect.DeepEqual(sample.ProbeRTTs, []time.Duration{0, 0, 25 * time.Millisecond}) {
		t.Fatalf("ProbeRTTs = %#v", sample.ProbeRTTs)
	}
}

func TestAggregateAllFailedJoinsUniqueErrorsInProbeOrder(t *testing.T) {
	results := []ProbeResult{
		{Err: errors.New("timeout")},
		{Err: errors.New("connection refused")},
		{Err: errors.New("timeout")},
		{},
	}

	sample := Aggregate(results, netip.Addr{}, nil)
	if sample.ProbesOK != 0 || sample.PrimaryIP.IsValid() {
		t.Fatalf("sample = %#v", sample)
	}
	if sample.Error != "timeout; connection refused; probe returned invalid IP" {
		t.Fatalf("Error = %q", sample.Error)
	}
}

func TestAggregateCapsAllFailedErrorAtTwoKiB(t *testing.T) {
	results := make([]ProbeResult, 400)
	for i := range results {
		results[i].Err = errors.New(strings.Repeat("é", 20) + time.Duration(i).String())
	}

	sample := Aggregate(results, netip.Addr{}, nil)
	if len(sample.Error) > 2<<10 {
		t.Fatalf("error size = %d, want <= 2048", len(sample.Error))
	}
	if !utf8.ValidString(sample.Error) {
		t.Fatalf("capped error is invalid UTF-8")
	}
}

func TestAggregateSaturatesUInt8CompatibleCounts(t *testing.T) {
	const resultCount = 300
	results := make([]ProbeResult, resultCount)
	for i := range results {
		var bytes [16]byte
		bytes[0], bytes[1] = 0x20, 0x01
		bytes[14], bytes[15] = byte(i>>8), byte(i)
		results[i] = ProbeResult{IP: netip.AddrFrom16(bytes), RTT: time.Duration(i) * time.Millisecond}
	}

	sample := Aggregate(results, netip.Addr{}, nil)
	if sample.ProbesAttempted != 255 || sample.ProbesOK != 255 || sample.DistinctIPs != 255 || sample.NewIPs != 255 {
		t.Fatalf("saturated counts = attempted:%d ok:%d distinct:%d new:%d", sample.ProbesAttempted, sample.ProbesOK, sample.DistinctIPs, sample.NewIPs)
	}
	if len(sample.ProbeIPs) != 255 || len(sample.ProbeRTTs) != 255 || len(sample.ProbeOK) != 255 {
		t.Fatalf("probe array lengths = %d/%d/%d", len(sample.ProbeIPs), len(sample.ProbeRTTs), len(sample.ProbeOK))
	}
	if sample.ProbeIPs[254] != results[254].IP {
		t.Fatalf("last retained IP = %v, want first 255 inputs kept in order", sample.ProbeIPs[254])
	}
}
