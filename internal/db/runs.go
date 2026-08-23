package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/timo972/proxy-sampler/internal/session"
	"github.com/timo972/proxy-sampler/internal/variation"
)

// pgUUID converts a uuid.UUID into the pgtype.UUID the generated run queries
// expect for the nullable run_id foreign key column.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func (s *Store) CreateRun(ctx context.Context, run variation.Run, children []variation.ChildSession) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin create run: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if err := q.InsertRun(ctx, InsertRunParams{
		ID: run.ID, Name: run.Name, TemplateCiphertext: run.TemplateCiphertext,
		TemplateNonce: run.TemplateNonce, TemplateDisplay: run.TemplateDisplay,
		Axes: run.Axes, CreatedAt: timestamp(run.CreatedAt),
	}); err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	for _, child := range children {
		base := insertSessionParams(child.Session)
		cellKey := child.CellKey
		if err := q.InsertRunSession(ctx, InsertRunSessionParams{
			ID: base.ID, Name: base.Name, ProxyCiphertext: base.ProxyCiphertext,
			ProxyNonce: base.ProxyNonce, ProxyDisplay: base.ProxyDisplay, Mode: base.Mode,
			CadenceSeconds: base.CadenceSeconds, ProbesPerSample: base.ProbesPerSample,
			ProbeTarget: base.ProbeTarget, DialTimeoutMs: base.DialTimeoutMs,
			MaxSamples: base.MaxSamples, MaxDurationSeconds: base.MaxDurationSeconds,
			Status: base.Status, SamplesTaken: base.SamplesTaken, ProbesOk: base.ProbesOk,
			ProbesTotal: base.ProbesTotal, DistinctIps: base.DistinctIps,
			LastSampleAt: base.LastSampleAt, LastPrimaryIp: base.LastPrimaryIp,
			LastRttMs: base.LastRttMs, LastError: base.LastError, CreatedAt: base.CreatedAt,
			StartedAt: base.StartedAt, StoppedAt: base.StoppedAt, SequenceOffset: base.SequenceOffset,
			TargetCountry: base.TargetCountry, RunID: pgUUID(run.ID), VariantParams: child.Params, CellKey: &cellKey,
		}); err != nil {
			return fmt.Errorf("insert run session: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit create run: %w", err)
	}
	return nil
}

func (s *Store) Runs(ctx context.Context) ([]variation.RunSummary, error) {
	rows, err := s.q.Runs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	result := make([]variation.RunSummary, 0, len(rows))
	for _, row := range rows {
		result = append(result, variation.RunSummary{
			Run: variation.Run{
				ID: row.ID, Name: row.Name, TemplateCiphertext: row.TemplateCiphertext,
				TemplateNonce: row.TemplateNonce, TemplateDisplay: row.TemplateDisplay,
				Axes: row.Axes, CreatedAt: row.CreatedAt.Time,
			},
			VariantCount: int(row.VariantCount), DistinctIPs: int(row.DistinctIps),
			Status: deriveRunStatus(row.VariantCount, row.RunningCount, row.FinishedCount),
		})
	}
	return result, nil
}

func (s *Store) RunByID(ctx context.Context, id uuid.UUID) (variation.RunSummary, error) {
	row, err := s.q.RunByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return variation.RunSummary{}, variation.ErrRunNotFound
	}
	if err != nil {
		return variation.RunSummary{}, fmt.Errorf("run by id: %w", err)
	}
	return variation.RunSummary{
		Run: variation.Run{
			ID: row.ID, Name: row.Name, TemplateCiphertext: row.TemplateCiphertext,
			TemplateNonce: row.TemplateNonce, TemplateDisplay: row.TemplateDisplay,
			Axes: row.Axes, CreatedAt: row.CreatedAt.Time,
		},
		VariantCount: int(row.VariantCount), DistinctIPs: int(row.DistinctIps),
		Status: deriveRunStatus(row.VariantCount, row.RunningCount, row.FinishedCount),
	}, nil
}

func (s *Store) RunSessions(ctx context.Context, id uuid.UUID) ([]variation.VariantSession, error) {
	rows, err := s.q.RunSessions(ctx, pgUUID(id))
	if err != nil {
		return nil, fmt.Errorf("run sessions: %w", err)
	}
	result := make([]variation.VariantSession, 0, len(rows))
	for _, row := range rows {
		result = append(result, variation.VariantSession{
			SessionID: row.ID, Name: row.Name, Params: row.VariantParams, CellKey: row.CellKey,
			TargetCountry: row.TargetCountry, Status: session.Status(row.Status),
			Snapshot: session.Snapshot{
				SamplesTaken: int(row.SamplesTaken), ProbesOK: row.ProbesOk,
				ProbesTotal: row.ProbesTotal, DistinctIPs: int(row.DistinctIps),
			},
		})
	}
	return result, nil
}

func (s *Store) RunIPObservations(ctx context.Context, id uuid.UUID, limit int) ([]variation.IPObservation, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.q.RunIPObservations(ctx, RunIPObservationsParams{RunID: pgUUID(id), RowLimit: int32(limit)})
	if err != nil {
		return nil, fmt.Errorf("run ip observations: %w", err)
	}
	result := make([]variation.IPObservation, 0, len(rows))
	for _, row := range rows {
		ip, err := parseAddr(row.Ip, "run ip")
		if err != nil {
			return nil, err
		}
		obs := variation.IPObservation{
			SessionID: row.SessionID, IP: ip, HitCount: row.HitCount,
			FirstSeen: row.FirstSeen.Time, LastSeen: row.LastSeen.Time,
		}
		if row.ReputationIp != "" {
			reputation, err := reputationFromValues(reputationValues{
				IP: row.ReputationIp, Country: row.Country, Region: row.Region, City: row.City,
				ISP: row.Isp, ASN: row.Asn, IsMobile: row.IsMobile, IPAPIProxy: row.IpapiProxy,
				IPAPIHosting: row.IpapiHosting, ProxyCheckType: row.PcType,
				ProxyCheckProxy: row.PcProxy, RiskScore: row.RiskScore,
				GreyNoiseClass: row.GreynoiseClass, SFSAppears: row.SfsAppears,
				SFSFrequency: row.SfsFrequency, DNSBLListed: row.DnsblListed,
				DNSBLHits: row.DnsblHits, Category: row.Category, Raw: row.Raw,
				FirstSeen: row.ReputationFirstSeen, RefreshedAt: row.RefreshedAt,
			})
			if err != nil {
				return nil, err
			}
			obs.Reputation = &reputation
		}
		result = append(result, obs)
	}
	return result, nil
}

// streamPoolIPsSQL deduplicates a run's exit IPs server-side: it aggregates
// session_ips across the run's children (summing hits, taking the earliest
// first_seen and latest last_seen) and joins each distinct IP's reputation
// once, streaming one row per IP in ascending order.
const streamPoolIPsSQL = `SELECT
  agg.ip::text AS ip, agg.hit_count, agg.first_seen, agg.last_seen,
  CAST(COALESCE(r.ip::text, '') AS text) AS reputation_ip,
  r.country, r.isp, r.asn, r.risk_score, r.greynoise_class, r.dnsbl_listed, r.dnsbl_hits, r.category
FROM (
  SELECT si.ip AS ip, SUM(si.hit_count)::bigint AS hit_count,
         MIN(si.first_seen) AS first_seen, MAX(si.last_seen) AS last_seen
  FROM session_ips AS si
  JOIN sampling_sessions AS s ON s.id = si.session_id
  WHERE s.run_id = $1
  GROUP BY si.ip
) AS agg
LEFT JOIN ip_reputation_cache AS r ON r.ip = agg.ip
ORDER BY agg.ip`

// StreamPoolIPs visits each distinct exit IP of a run once, deduped and
// aggregated in the database, so a large export never materializes the whole
// pool in memory.
func (s *Store) StreamPoolIPs(ctx context.Context, runID uuid.UUID, visit func(variation.IPRow) error) error {
	rows, err := s.pool.Query(ctx, streamPoolIPsSQL, runID)
	if err != nil {
		return fmt.Errorf("stream pool ips: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ip                                     string
			hitCount                               int64
			firstSeen, lastSeen                    pgtype.Timestamptz
			reputationIP                           string
			country, isp, asn, greynoise, category *string
			riskScore                              *int32
			dnsblListed                            *bool
			dnsblHits                              *string
		)
		if err := rows.Scan(&ip, &hitCount, &firstSeen, &lastSeen, &reputationIP,
			&country, &isp, &asn, &riskScore, &greynoise, &dnsblListed, &dnsblHits, &category); err != nil {
			return fmt.Errorf("scan pool ip: %w", err)
		}
		addr, err := parseAddr(ip, "pool ip")
		if err != nil {
			return err
		}
		row := variation.IPRow{
			IP: addr.String(), Category: "unknown", DNSBLHits: []string{},
			HitCount: hitCount, FirstSeen: firstSeen.Time, LastSeen: lastSeen.Time,
		}
		if reputationIP != "" {
			row.Category = variation.NormalizeCategory(stringValue(category))
			row.Country, row.ISP, row.ASN = stringValue(country), stringValue(isp), stringValue(asn)
			row.GreyNoiseClass = stringValue(greynoise)
			row.RiskScore = intPtr(riskScore)
			if dnsblListed != nil {
				row.DNSBLListed = *dnsblListed
			}
			if dnsblHits != nil && *dnsblHits != "" {
				var hits []string
				if err := json.Unmarshal([]byte(*dnsblHits), &hits); err != nil {
					return fmt.Errorf("decode DNSBL hits: %w", err)
				}
				row.DNSBLHits = append(row.DNSBLHits, hits...)
			}
		}
		if err := visit(row); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) DeleteRun(ctx context.Context, id uuid.UUID) error {
	if err := s.q.DeleteRun(ctx, id); err != nil {
		return fmt.Errorf("delete run: %w", err)
	}
	return nil
}

// deriveRunStatus mirrors the spec: running if any child runs, else finished if
// every child finished, else stopped. Empty runs report stopped.
func deriveRunStatus(variantCount, runningCount, finishedCount int64) variation.RunStatus {
	if runningCount > 0 {
		return variation.RunRunning
	}
	if variantCount > 0 && finishedCount == variantCount {
		return variation.RunFinished
	}
	return variation.RunStopped
}

var _ variation.Store = (*Store)(nil)
