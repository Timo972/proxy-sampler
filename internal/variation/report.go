package variation

import (
	"encoding/json"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/session"
)

// Composition counts distinct IPs by network category.
type Composition struct {
	Mobile      int
	Residential int
	Datacenter  int
	Unknown     int
}

// RiskBucket is one 10-wide risk-score histogram bin.
type RiskBucket struct {
	Label string
	Min   int
	Max   int
	Count int
}

// IPRow is one deduped pool IP with its reputation for display/export.
type IPRow struct {
	IP             string
	Category       string
	Country        string
	ISP            string
	ASN            string
	RiskScore      *int
	GreyNoiseClass string
	DNSBLListed    bool
	DNSBLHits      []string
	HitCount       int64
	FirstSeen      time.Time
	LastSeen       time.Time
}

// CellReport summarizes one parameter cell (params minus random axes).
type CellReport struct {
	CellKey      string
	Params       json.RawMessage
	VariantCount int
	DistinctIPs  int
	HonorRate    *float64
	Composition  Composition
}

// PoolReport is the aggregated, IP-deduped view over a whole run.
type PoolReport struct {
	DistinctIPs        int
	EstimatedPoolSize  int
	PoolSizeLowerBound bool
	Composition        Composition
	RiskHistogram      []RiskBucket
	FlaggedIPs         int
	FlaggedPercent     float64
	DNSBLHitIPs        int
	HonorRate          *float64
	Cells              []CellReport
	IPs                []IPRow
}

// BuildPoolReport aggregates variant sessions and their IP observations into a
// pool-level report, deduping every statistic by distinct exit IP.
func BuildPoolReport(variants []VariantSession, observations []IPObservation) PoolReport {
	report := PoolReport{RiskHistogram: newRiskHistogram(), IPs: []IPRow{}, Cells: []CellReport{}}

	// Distinct IP -> merged row + set of variant sessions that saw it.
	ips := map[netip.Addr]*ipAccum{}
	// Per-session observed rollups for honor-rate computation.
	sessionObs := map[uuid.UUID][]IPObservation{}
	for _, obs := range observations {
		sessionObs[obs.SessionID] = append(sessionObs[obs.SessionID], obs)
		accum, ok := ips[obs.IP]
		if !ok {
			accum = &ipAccum{row: newIPRow(obs), variants: map[uuid.UUID]struct{}{}}
			ips[obs.IP] = accum
		} else {
			accum.row.HitCount += obs.HitCount
			if !obs.FirstSeen.IsZero() && (accum.row.FirstSeen.IsZero() || obs.FirstSeen.Before(accum.row.FirstSeen)) {
				accum.row.FirstSeen = obs.FirstSeen
			}
			if obs.LastSeen.After(accum.row.LastSeen) {
				accum.row.LastSeen = obs.LastSeen
			}
		}
		accum.variants[obs.SessionID] = struct{}{}
	}

	frequencies := make([]int, 0, len(ips))
	for _, accum := range sortedIPs(ips) {
		frequencies = append(frequencies, len(accum.variants))
		applyComposition(&report.Composition, accum.row.Category)
		if flagged := isFlagged(accum.row); flagged {
			report.FlaggedIPs++
		}
		if accum.row.DNSBLListed {
			report.DNSBLHitIPs++
		}
		if accum.row.RiskScore != nil {
			addRisk(report.RiskHistogram, *accum.row.RiskScore)
		}
		report.IPs = append(report.IPs, accum.row)
	}
	report.DistinctIPs = len(ips)
	if report.DistinctIPs > 0 {
		report.FlaggedPercent = float64(report.FlaggedIPs) * 100 / float64(report.DistinctIPs)
	}
	estimate := Chao1(frequencies)
	report.EstimatedPoolSize = estimate.Estimate
	report.PoolSizeLowerBound = estimate.LowerBound

	report.HonorRate, report.Cells = buildHonorAndCells(variants, sessionObs)
	return report
}

func newIPRow(obs IPObservation) IPRow {
	row := IPRow{
		IP: obs.IP.String(), Category: "unknown", DNSBLHits: []string{},
		HitCount: obs.HitCount, FirstSeen: obs.FirstSeen, LastSeen: obs.LastSeen,
	}
	if r := obs.Reputation; r != nil {
		row.Category = normalizedCategory(r.Category)
		row.Country, row.ISP, row.ASN = r.Country, r.ISP, r.ASN
		row.GreyNoiseClass = r.GreyNoiseClass
		if r.RiskScore != nil {
			score := *r.RiskScore
			row.RiskScore = &score
		}
		if r.DNSBLListed != nil {
			row.DNSBLListed = *r.DNSBLListed
		}
		row.DNSBLHits = append(row.DNSBLHits, r.DNSBLHits...)
	}
	return row
}

func isFlagged(row IPRow) bool {
	return (row.RiskScore != nil && *row.RiskScore >= 70) ||
		strings.EqualFold(row.GreyNoiseClass, "malicious") || row.DNSBLListed
}

func buildHonorAndCells(variants []VariantSession, sessionObs map[uuid.UUID][]IPObservation) (*float64, []CellReport) {
	type cellAccum struct {
		key          string
		params       json.RawMessage
		variantCount int
		honored      int
		sampled      int
		ips          map[netip.Addr]struct{}
		composition  Composition
	}
	order := []string{}
	cells := map[string]*cellAccum{}
	globalHonored, globalSampled := 0, 0

	for _, v := range variants {
		accum, ok := cells[v.CellKey]
		if !ok {
			accum = &cellAccum{key: v.CellKey, params: v.Params, ips: map[netip.Addr]struct{}{}}
			cells[v.CellKey] = accum
			order = append(order, v.CellKey)
		}
		accum.variantCount++
		obs := sessionObs[v.SessionID]
		for _, o := range obs {
			if _, seen := accum.ips[o.IP]; !seen {
				accum.ips[o.IP] = struct{}{}
				applyComposition(&accum.composition, normalizedCategory(reputationCategory(o.Reputation)))
			}
		}
		if v.Snapshot.SamplesTaken == 0 || len(obs) == 0 {
			continue
		}
		accum.sampled++
		globalSampled++
		if honored := variantHonored(v.Params, obs); honored {
			accum.honored++
			globalHonored++
		}
	}

	result := make([]CellReport, 0, len(order))
	for _, key := range order {
		accum := cells[key]
		cell := CellReport{
			CellKey: accum.key, Params: accum.params, VariantCount: accum.variantCount,
			DistinctIPs: len(accum.ips), Composition: accum.composition,
		}
		if accum.sampled > 0 {
			rate := float64(accum.honored) / float64(accum.sampled)
			cell.HonorRate = &rate
		}
		result = append(result, cell)
	}
	var honorRate *float64
	if globalSampled > 0 {
		rate := float64(globalHonored) / float64(globalSampled)
		honorRate = &rate
	}
	return honorRate, result
}

// variantHonored compares requested country/isp params against the dominant
// observed values (weighted by hit count) across the variant's IPs.
func variantHonored(params json.RawMessage, obs []IPObservation) bool {
	requested := map[string]string{}
	_ = json.Unmarshal(params, &requested)
	country := firstParam(requested, "country", "cc")
	isp := firstParam(requested, "isp")
	if country == "" && isp == "" {
		return true // nothing targeted -> nothing to violate
	}
	if country != "" && !strings.EqualFold(dominant(obs, func(r *session.Reputation) string { return r.Country }), country) {
		return false
	}
	if isp != "" && !strings.EqualFold(dominant(obs, func(r *session.Reputation) string { return r.ISP }), isp) {
		return false
	}
	return true
}

func dominant(obs []IPObservation, pick func(*session.Reputation) string) string {
	weights := map[string]int64{}
	for _, o := range obs {
		if o.Reputation == nil {
			continue
		}
		value := pick(o.Reputation)
		if value == "" {
			continue
		}
		weights[value] += o.HitCount + 1
	}
	best, bestWeight := "", int64(0)
	for value, weight := range weights {
		if weight > bestWeight {
			best, bestWeight = value, weight
		}
	}
	return best
}

func firstParam(params map[string]string, keys ...string) string {
	for _, key := range keys {
		if v, ok := params[key]; ok && v != "" {
			return v
		}
	}
	return ""
}

func reputationCategory(r *session.Reputation) string {
	if r == nil {
		return "unknown"
	}
	return r.Category
}

// ipAccum merges every observation of one distinct exit IP across variants.
type ipAccum struct {
	row      IPRow
	variants map[uuid.UUID]struct{}
}

// sortedIPs returns the accumulators in deterministic IP order.
func sortedIPs(ips map[netip.Addr]*ipAccum) []*ipAccum {
	keys := make([]netip.Addr, 0, len(ips))
	for k := range ips {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	out := make([]*ipAccum, 0, len(keys))
	for _, k := range keys {
		out = append(out, ips[k])
	}
	return out
}

func applyComposition(c *Composition, category string) {
	switch category {
	case "mobile":
		c.Mobile++
	case "residential":
		c.Residential++
	case "datacenter":
		c.Datacenter++
	default:
		c.Unknown++
	}
}

func normalizedCategory(value string) string {
	switch value {
	case "mobile", "residential", "datacenter":
		return value
	default:
		return "unknown"
	}
}

func newRiskHistogram() []RiskBucket {
	buckets := make([]RiskBucket, 10)
	for i := range buckets {
		min := i * 10
		max := min + 9
		if i == 9 {
			max = 100
		}
		buckets[i] = RiskBucket{Label: labelFor(min, max), Min: min, Max: max}
	}
	return buckets
}

func labelFor(min, max int) string {
	return strconv.Itoa(min) + "-" + strconv.Itoa(max)
}

func addRisk(buckets []RiskBucket, score int) {
	if score < 0 || score > 100 {
		return
	}
	bucket := score / 10
	if bucket == 10 {
		bucket = 9
	}
	buckets[bucket].Count++
}
