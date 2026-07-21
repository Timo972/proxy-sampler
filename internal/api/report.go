package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/timo972/proxy-sampler/internal/api/openapi"
	"github.com/timo972/proxy-sampler/internal/ch"
	"github.com/timo972/proxy-sampler/internal/session"
)

const (
	shortReportBucket = 5 * time.Minute
	longReportBucket  = time.Hour
	shortReportRange  = 48 * time.Hour
)

// SessionReport composes ClickHouse aggregates with the Postgres IP inventory.
func (s *Server) SessionReport(ctx context.Context, request openapi.SessionReportRequestObject) (openapi.SessionReportResponseObject, error) {
	if s.store == nil || s.reader == nil {
		return nil, dependencyUnavailable()
	}
	value, err := s.store.SessionByID(ctx, request.Id)
	if errors.Is(err, session.ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, dependencyUnavailable()
	}

	from := value.CreatedAt
	if value.StartedAt != nil {
		from = *value.StartedAt
	}
	if request.Params.From != nil {
		from = *request.Params.From
	}
	to := s.now().UTC()
	if request.Params.To != nil {
		to = *request.Params.To
	}
	if from.After(to) {
		return nil, invalidRequest()
	}
	bucket := shortReportBucket
	if to.Sub(from) > shortReportRange {
		bucket = longReportBucket
	}

	series, err := s.reader.Series(ctx, value.ID, from, to, bucket)
	if err != nil {
		return nil, dependencyUnavailable()
	}
	stickiness, err := s.reader.Stickiness(ctx, value.ID, from, to)
	if err != nil {
		return nil, dependencyUnavailable()
	}
	growth, err := s.reader.PoolGrowth(ctx, value.ID, from, to)
	if err != nil {
		return nil, dependencyUnavailable()
	}
	ips, err := s.store.SessionIPs(ctx, value.ID)
	if err != nil {
		return nil, dependencyUnavailable()
	}

	report := openapi.SessionReport{
		Series:          mapSeries(series),
		Stickiness:      mapStickiness(stickiness),
		PoolGrowth:      mapGrowth(growth),
		RiskHistogram:   newRiskHistogram(),
		Ips:             make([]openapi.IPRow, 0, len(ips)),
		PoolComposition: openapi.PoolComposition{},
	}
	for _, record := range ips {
		addIPRecord(&report, record)
	}
	report.ReputationSummary.TotalIps = len(ips)
	if len(ips) > 0 {
		report.ReputationSummary.FlaggedPercent = float64(report.ReputationSummary.FlaggedIps) * 100 / float64(len(ips))
	}
	return openapi.SessionReport200JSONResponse(report), nil
}

func mapSeries(points []ch.SeriesPoint) []openapi.SeriesPoint {
	result := make([]openapi.SeriesPoint, 0, len(points))
	for _, point := range points {
		result = append(result, openapi.SeriesPoint{
			At: point.At, SuccessRate: point.SuccessRate, LatencyP50Ms: point.LatencyP50MS,
			LatencyP95Ms: point.LatencyP95MS, DistinctPerSample: point.DistinctPerSample,
			IpChanges: int64(point.IPChanges), Mobile: int64(point.Mobile),
			Residential: int64(point.Residential), Datacenter: int64(point.Datacenter), Unknown: int64(point.Unknown),
		})
	}
	return result
}

func mapStickiness(value ch.Stickiness) openapi.Stickiness {
	result := openapi.Stickiness{
		Holds: make([]openapi.Hold, 0, len(value.Holds)), Rotations: make([]openapi.Rotation, 0, len(value.Rotations)),
		AverageHoldSeconds: value.AverageHoldSeconds, MedianHoldSeconds: value.MedianHoldSeconds,
	}
	for _, hold := range value.Holds {
		result.Holds = append(result.Holds, openapi.Hold{
			Ip: hold.IP, StartedAt: hold.StartedAt, EndedAt: hold.EndedAt,
			Samples: int64(hold.Samples), DurationSeconds: hold.DurationSeconds,
		})
	}
	for _, rotation := range value.Rotations {
		result.Rotations = append(result.Rotations, openapi.Rotation{
			At: rotation.At, FromIp: rotation.FromIP, ToIp: rotation.ToIP,
			SincePreviousSeconds: rotation.SincePreviousSeconds,
		})
	}
	return result
}

func mapGrowth(points []ch.GrowthPoint) []openapi.PoolGrowthPoint {
	result := make([]openapi.PoolGrowthPoint, 0, len(points))
	for _, point := range points {
		result = append(result, openapi.PoolGrowthPoint{At: point.At, DistinctIps: int64(point.DistinctIPs)})
	}
	return result
}

func newRiskHistogram() []openapi.RiskBucket {
	result := make([]openapi.RiskBucket, 10)
	for i := range result {
		result[i] = openapi.RiskBucket{Label: riskLabel(i), Min: i * 10, Max: i*10 + 9}
	}
	result[9].Max = 100
	return result
}

func riskLabel(index int) string {
	if index == 9 {
		return "90-100"
	}
	min := index * 10
	return strconv.Itoa(min) + "-" + strconv.Itoa(min+9)
}

func addIPRecord(report *openapi.SessionReport, record session.IPRecord) {
	row := openapi.IPRow{
		Ip: record.IP.String(), Category: "unknown", DnsblHits: []string{},
		FirstSeen: record.FirstSeen, LastSeen: record.LastSeen, HitCount: record.HitCount,
	}
	flagged := false
	if reputation := record.Reputation; reputation != nil {
		row.Category = normalizedCategory(reputation.Category)
		row.Country, row.Isp, row.Asn = reputation.Country, reputation.ISP, reputation.ASN
		row.RiskScore = cloneInt(reputation.RiskScore)
		row.GreynoiseClass = reputation.GreyNoiseClass
		row.DnsblListed = boolValue(reputation.DNSBLListed)
		row.DnsblHits = append(row.DnsblHits, reputation.DNSBLHits...)
		flagged = boolValue(reputation.ProxyCheckProxy) ||
			(reputation.RiskScore != nil && *reputation.RiskScore >= 70) ||
			strings.EqualFold(reputation.GreyNoiseClass, "malicious") ||
			boolValue(reputation.SFSAppears) || row.DnsblListed
		if reputation.RiskScore != nil && *reputation.RiskScore >= 0 && *reputation.RiskScore <= 100 {
			bucket := *reputation.RiskScore / 10
			if bucket == 10 {
				bucket = 9
			}
			report.RiskHistogram[bucket].Count++
		}
	}
	switch row.Category {
	case "mobile":
		report.PoolComposition.Mobile++
	case "residential":
		report.PoolComposition.Residential++
	case "datacenter":
		report.PoolComposition.Datacenter++
	default:
		report.PoolComposition.Unknown++
	}
	if flagged {
		report.ReputationSummary.FlaggedIps++
	}
	if row.DnsblListed {
		report.ReputationSummary.DnsblHitIps++
	}
	report.Ips = append(report.Ips, row)
}

func normalizedCategory(value string) string {
	switch value {
	case "mobile", "residential", "datacenter":
		return value
	default:
		return "unknown"
	}
}

func boolValue(value *bool) bool { return value != nil && *value }
