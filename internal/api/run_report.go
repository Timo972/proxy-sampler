package api

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/api/openapi"
	"github.com/timo972/proxy-sampler/internal/variation"
)

// capIPRows keeps only the most-active limit IP rows in the pool report so the
// response array stays bounded for large pools.
func capIPRows(pool *variation.PoolReport, limit int) {
	if len(pool.IPs) <= limit {
		return
	}
	sort.Slice(pool.IPs, func(i, j int) bool { return pool.IPs[i].HitCount > pool.IPs[j].HitCount })
	pool.IPs = pool.IPs[:limit]
}

// RunReport aggregates a run's children into a deduped pool report plus a
// ClickHouse series across every child session.
func (s *Server) RunReport(ctx context.Context, request openapi.RunReportRequestObject) (openapi.RunReportResponseObject, error) {
	if s.runStore == nil || s.reader == nil {
		return nil, dependencyUnavailable()
	}
	summary, err := s.runStore.RunByID(ctx, request.Id)
	if errors.Is(err, variation.ErrRunNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, dependencyUnavailable()
	}
	variants, err := s.runStore.RunSessions(ctx, request.Id)
	if err != nil {
		return nil, dependencyUnavailable()
	}
	observations, err := s.runStore.RunIPObservations(ctx, request.Id, maxReportObservations)
	if err != nil {
		return nil, dependencyUnavailable()
	}
	pool := variation.BuildPoolReport(variants, observations)
	// If the observation load hit the cap, the summary is a lower-bound sample
	// of a larger pool. Flag it so the UI can indicate the numbers are partial
	// (and don't contradict the uncapped distinct_ips shown on the list page).
	truncated := len(observations) >= maxReportObservations
	if truncated {
		pool.PoolSizeLowerBound = true
	}
	// Bound the IP detail array so a large pool cannot produce a huge JSON
	// response on every poll. The aggregate stats above already cover the
	// (capped) observation set; the details show the most-active IPs.
	capIPRows(&pool, maxReportIPRows)

	ids := make([]uuid.UUID, 0, len(variants))
	for _, v := range variants {
		ids = append(ids, v.SessionID)
	}
	from := summary.CreatedAt
	to := s.now().UTC()
	bucket := shortReportBucket
	if to.Sub(from) > shortReportRange {
		bucket = longReportBucket
	}
	series, err := s.reader.SeriesForSessions(ctx, ids, from, to, bucket)
	if err != nil {
		return nil, dependencyUnavailable()
	}
	return openapi.RunReport200JSONResponse(mapRunReport(pool, mapSeries(series), truncated)), nil
}

func mapRunReport(pool variation.PoolReport, series []openapi.SeriesPoint, truncated bool) openapi.RunReport {
	return openapi.RunReport{
		DistinctIps: pool.DistinctIPs, EstimatedPoolSize: pool.EstimatedPoolSize,
		PoolSizeLowerBound: pool.PoolSizeLowerBound, Truncated: truncated, HonorRate: pool.HonorRate,
		Composition: openapi.PoolComposition{
			Mobile: pool.Composition.Mobile, Residential: pool.Composition.Residential,
			Datacenter: pool.Composition.Datacenter, Unknown: pool.Composition.Unknown,
		},
		FlaggedIps: pool.FlaggedIPs, FlaggedPercent: pool.FlaggedPercent, DnsblHitIps: pool.DNSBLHitIPs,
		RiskHistogram: mapRiskBuckets(pool.RiskHistogram), Series: series,
		Cells: mapCells(pool.Cells), Ips: mapPoolIPs(pool.IPs),
	}
}

func mapRiskBuckets(buckets []variation.RiskBucket) []openapi.RiskBucket {
	result := make([]openapi.RiskBucket, 0, len(buckets))
	for _, bucket := range buckets {
		result = append(result, openapi.RiskBucket{
			Label: bucket.Label, Min: bucket.Min, Max: bucket.Max, Count: bucket.Count,
		})
	}
	return result
}

func mapCells(cells []variation.CellReport) []openapi.CellReport {
	result := make([]openapi.CellReport, 0, len(cells))
	for _, cell := range cells {
		params := map[string]string{}
		_ = json.Unmarshal(cell.Params, &params)
		result = append(result, openapi.CellReport{
			CellKey: cell.CellKey, Params: params, TargetCountry: optionalString(cell.TargetCountry),
			VariantCount: cell.VariantCount, DistinctIps: cell.DistinctIPs, HonorRate: cell.HonorRate,
			Composition: openapi.PoolComposition{
				Mobile: cell.Composition.Mobile, Residential: cell.Composition.Residential,
				Datacenter: cell.Composition.Datacenter, Unknown: cell.Composition.Unknown,
			},
		})
	}
	return result
}

func mapPoolIPs(rows []variation.IPRow) []openapi.IPRow {
	result := make([]openapi.IPRow, 0, len(rows))
	for _, row := range rows {
		result = append(result, openapi.IPRow{
			Ip: row.IP, Category: row.Category, Country: row.Country, Isp: row.ISP, Asn: row.ASN,
			RiskScore: row.RiskScore, GreynoiseClass: row.GreyNoiseClass, DnsblListed: row.DNSBLListed,
			DnsblHits: row.DNSBLHits, HitCount: row.HitCount, FirstSeen: row.FirstSeen, LastSeen: row.LastSeen,
		})
	}
	return result
}
