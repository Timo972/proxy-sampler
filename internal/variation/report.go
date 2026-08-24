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
	CellKey string
	Params  json.RawMessage
	// TargetCountry is the manual target the cell's honor rate was measured
	// against, when one was set on the run. Params may still name a country
	// axis value the override ignored, so reports must not present the honor
	// rate as measuring the params country when this is set.
	TargetCountry string
	VariantCount  int
	DistinctIPs   int
	HonorRate     *float64
	Composition   Composition
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
			accum = &ipAccum{row: newIPRow(obs), variants: map[uuid.UUID]struct{}{}, flagged: reputationFlagged(obs.Reputation)}
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
		if accum.flagged {
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

// reputationFlagged mirrors the session report's flagged predicate so per-IP
// flagged counts are consistent between session and run reports. It uses the
// full reputation (ProxyCheck and StopForumSpam included), not the reduced
// IPRow, which drops those signals.
func reputationFlagged(r *session.Reputation) bool {
	if r == nil {
		return false
	}
	return boolValue(r.ProxyCheckProxy) ||
		(r.RiskScore != nil && *r.RiskScore >= 70) ||
		strings.EqualFold(r.GreyNoiseClass, "malicious") ||
		boolValue(r.SFSAppears) ||
		boolValue(r.DNSBLListed)
}

func boolValue(value *bool) bool { return value != nil && *value }

func buildHonorAndCells(variants []VariantSession, sessionObs map[uuid.UUID][]IPObservation) (*float64, []CellReport) {
	type cellAccum struct {
		key           string
		params        json.RawMessage
		targetCountry string
		variantCount  int
		honored       int
		sampled       int
		ips           map[netip.Addr]struct{}
		composition   Composition
	}
	order := []string{}
	cells := map[string]*cellAccum{}
	globalHonored, globalSampled := 0, 0

	for _, v := range variants {
		accum, ok := cells[v.CellKey]
		if !ok {
			// The cell key is the canonical JSON of the fixed (non-random) axis
			// params, so it is the correct params view for the cell. Using the
			// first variant's full params would leak that variant's random
			// values (e.g. a session id) and disagree with cell_key.
			accum = &cellAccum{key: v.CellKey, params: json.RawMessage(v.CellKey), ips: map[netip.Addr]struct{}{}}
			cells[v.CellKey] = accum
			order = append(order, v.CellKey)
		}
		accum.variantCount++
		// All of a run's children share one manual target, so the first
		// non-empty value is the cell's effective override.
		if accum.targetCountry == "" {
			accum.targetCountry = v.TargetCountry
		}
		obs := sessionObs[v.SessionID]
		for _, o := range obs {
			if _, seen := accum.ips[o.IP]; !seen {
				accum.ips[o.IP] = struct{}{}
				applyComposition(&accum.composition, normalizedCategory(reputationCategory(o.Reputation)))
			}
		}
		// Only variants with a determinable honor outcome enter the
		// denominator. A variant with no target (random/port-only runs) or no
		// known observed value for its target is "unknown" and excluded, so the
		// rate is not inflated to a misleading 100% or deflated by counting an
		// un-reputed variant as a mismatch. Observation presence — not the
		// per-run SamplesTaken counter, which a re-enable resets — drives this,
		// so already-observed variants survive a re-enable.
		switch variantHonor(v.Params, v.TargetCountry, obs) {
		case honorMatch:
			accum.sampled++
			accum.honored++
			globalSampled++
			globalHonored++
		case honorMismatch:
			accum.sampled++
			globalSampled++
		}
	}

	result := make([]CellReport, 0, len(order))
	for _, key := range order {
		accum := cells[key]
		cell := CellReport{
			CellKey: accum.key, Params: accum.params, TargetCountry: accum.targetCountry,
			VariantCount: accum.variantCount, DistinctIPs: len(accum.ips), Composition: accum.composition,
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

// honorOutcome is the tri-state result of comparing a variant's requested
// targeting to its observed egress.
type honorOutcome int

const (
	honorUnknown  honorOutcome = iota // no target, or no known observed value to compare
	honorMatch                        // every comparable requested field matched
	honorMismatch                     // at least one comparable requested field mismatched
)

// variantHonor compares requested country/isp params against the dominant
// observed values across the variant's IPs. It returns honorUnknown when there
// is nothing to compare — no target, or no known observed value for the target
// — so such variants can be excluded from the honor-rate denominator rather
// than silently counted as honored (100%) or as a mismatch. A non-empty
// targetCountry (the manually declared target) overrides any country axis
// param.
func variantHonor(params json.RawMessage, targetCountry string, obs []IPObservation) honorOutcome {
	requested := map[string]string{}
	_ = json.Unmarshal(params, &requested)
	country := firstParam(requested, "country", "cc")
	if targetCountry != "" {
		country = targetCountry
	}
	isp := firstParam(requested, "isp")
	if country == "" && isp == "" {
		return honorUnknown
	}
	// A proven mismatch on any requested field wins. Otherwise every requested
	// field must have a comparable observed value; if any cannot be compared,
	// the outcome is unknown rather than a partial "match".
	complete := true
	if country != "" {
		observed := dominant(obs, func(r *session.Reputation) string { return r.Country })
		switch {
		case observed == "":
			complete = false
		case !strings.EqualFold(observed, country):
			return honorMismatch
		}
	}
	if isp != "" {
		observed := dominant(obs, func(r *session.Reputation) string { return r.ISP })
		switch {
		case observed == "":
			complete = false
		case !ispMatches(isp, observed):
			return honorMismatch
		}
	}
	if !complete {
		return honorUnknown
	}
	return honorMatch
}

// ispMatches compares a requested ISP axis token against an observed ISP name
// tolerantly: providers use short tokens ("telekom") while reputation gives the
// full registered name ("Deutsche Telekom AG"), so an exact match is too
// strict. A case-insensitive substring match in either direction is used.
func ispMatches(requested, observed string) bool {
	r := strings.ToLower(strings.TrimSpace(requested))
	o := strings.ToLower(strings.TrimSpace(observed))
	if r == "" || o == "" {
		return false
	}
	return strings.Contains(o, r) || strings.Contains(r, o)
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
		// Weight by the actual observation count. An observed IP always has at
		// least one hit; clamp to 1 defensively so a zero-hit row still votes
		// once rather than giving every distinct IP a fixed bonus (which would
		// bias dominance toward IP count instead of traffic).
		weights[value] += max(o.HitCount, 1)
	}
	// Require a unique maximum. A tie has no clear dominant value, so return ""
	// (an unknown outcome) instead of letting map iteration order pick a
	// nondeterministic winner that could flip a variant's honor result between
	// identical report builds.
	var maxWeight int64
	for _, weight := range weights {
		if weight > maxWeight {
			maxWeight = weight
		}
	}
	best, count := "", 0
	for value, weight := range weights {
		if weight == maxWeight {
			best, count = value, count+1
		}
	}
	if count != 1 {
		return ""
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
	flagged  bool
}

// sortedIPs returns the accumulators in deterministic IP order.
func sortedIPs(ips map[netip.Addr]*ipAccum) []*ipAccum {
	keys := make([]netip.Addr, 0, len(ips))
	for k := range ips {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Compare(keys[j]) < 0 })
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

func normalizedCategory(value string) string { return NormalizeCategory(value) }

// NormalizeCategory collapses a reputation category to one of the pool report's
// canonical buckets. Exported so streaming readers (e.g. the pool CSV export)
// emit the same category values as BuildPoolReport.
func NormalizeCategory(value string) string {
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
