package ch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
)

const samplePageSize = 50

var (
	// ErrInvalidRange means a report's start is later than its end.
	ErrInvalidRange = errors.New("invalid time range")
	// ErrInvalidBucket means a series bucket is non-positive.
	ErrInvalidBucket = errors.New("invalid series bucket")
)

const eventColumns = `session_id, sampled_at, sample_seq, probes_attempted, probes_ok,
	primary_ip, distinct_ips, ip_changed, new_ips,
	rtt_min_ms, rtt_med_ms, rtt_max_ms,
	egress_country, primary_category, primary_risk,
	probe_ips, probe_rtts_ms, probe_ok, error`

// Reader owns a ClickHouse connection used for sample and report queries.
type Reader struct {
	conn      driver.Conn
	closeOnce sync.Once
	closeErr  error
}

// NewReader opens, pings, and owns a ClickHouse connection.
func NewReader(ctx context.Context, opts *clickhouse.Options) (*Reader, error) {
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse reader: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping clickhouse reader: %w", err)
	}
	return &Reader{conn: conn}, nil
}

// Ping verifies that the reader's connection can reach ClickHouse.
func (r *Reader) Ping(ctx context.Context) error {
	if r.conn == nil {
		return errors.New("clickhouse reader has no connection")
	}
	return r.conn.Ping(ctx)
}

// Close releases the owned ClickHouse connection. It is idempotent.
func (r *Reader) Close() error {
	r.closeOnce.Do(func() {
		if r.conn != nil {
			r.closeErr = r.conn.Close()
		}
	})
	return r.closeErr
}

// Samples returns a newest-first 50-row page with optional inclusive filters.
func (r *Reader) Samples(ctx context.Context, sessionID uuid.UUID, from, to *time.Time, page int) (SamplePage, error) {
	if err := optionalRange(from, to); err != nil {
		return SamplePage{}, err
	}
	if page < 1 {
		page = 1
	}
	where, args := sampleWhere(sessionID, from, to)
	result := SamplePage{Page: page, PageSize: samplePageSize, Items: []Event{}}
	if err := r.conn.QueryRow(ctx, "SELECT count() FROM sample_events "+where, args...).Scan(&result.Total); err != nil {
		return SamplePage{}, fmt.Errorf("count samples: %w", err)
	}
	query := "SELECT " + eventColumns + " FROM sample_events " + where +
		" ORDER BY sampled_at DESC, sample_seq DESC LIMIT ? OFFSET ?"
	args = append(args, samplePageSize, (page-1)*samplePageSize)
	rows, err := r.conn.Query(ctx, query, args...)
	if err != nil {
		return SamplePage{}, fmt.Errorf("query samples: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return SamplePage{}, fmt.Errorf("scan sample: %w", err)
		}
		result.Items = append(result.Items, event)
	}
	if err := rows.Err(); err != nil {
		return SamplePage{}, fmt.Errorf("iterate samples: %w", err)
	}
	return result, nil
}

// StreamSamples visits all filtered samples in chronological order.
func (r *Reader) StreamSamples(ctx context.Context, sessionID uuid.UUID, from, to *time.Time, visit func(Event) error) error {
	if err := optionalRange(from, to); err != nil {
		return err
	}
	if visit == nil {
		return errors.New("sample visitor is nil")
	}
	where, args := sampleWhere(sessionID, from, to)
	query := "SELECT " + eventColumns + " FROM sample_events " + where + " ORDER BY sampled_at, sample_seq"
	rows, err := r.conn.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("query sample stream: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return fmt.Errorf("scan sample stream: %w", err)
		}
		if err := visit(event); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate sample stream: %w", err)
	}
	return nil
}

// Series returns chronological aggregate buckets over an explicit range.
func (r *Reader) Series(ctx context.Context, sessionID uuid.UUID, from, to time.Time, bucket time.Duration) ([]SeriesPoint, error) {
	if from.After(to) {
		return nil, ErrInvalidRange
	}
	if bucket <= 0 {
		return nil, ErrInvalidBucket
	}
	bucketSeconds := int64(bucket / time.Second)
	if bucketSeconds == 0 {
		bucketSeconds = 1
	}
	const query = `SELECT
		toStartOfInterval(sampled_at, toIntervalSecond(?)) AS bucket,
		if(sum(probes_attempted) = 0, 0, sum(probes_ok) / sum(probes_attempted)) AS success_rate,
		if(countIf(probes_ok > 0) = 0, 0, quantileExactIf(0.5)(rtt_med_ms, probes_ok > 0)) AS latency_p50_ms,
		if(countIf(probes_ok > 0) = 0, 0, quantileExactIf(0.95)(rtt_med_ms, probes_ok > 0)) AS latency_p95_ms,
		avg(distinct_ips) AS distinct_per_sample,
		countIf(ip_changed > 0) AS ip_changes,
		countIf(primary_category = 'mobile') AS mobile,
		countIf(primary_category = 'residential') AS residential,
		countIf(primary_category = 'datacenter') AS datacenter,
		countIf(primary_category = 'unknown') AS unknown
	FROM sample_events
	WHERE session_id = ? AND sampled_at >= ? AND sampled_at <= ?
	GROUP BY bucket
	ORDER BY bucket`
	rows, err := r.conn.Query(ctx, query, bucketSeconds, sessionID, from, to)
	if err != nil {
		return nil, fmt.Errorf("query series: %w", err)
	}
	defer rows.Close()
	points := []SeriesPoint{}
	for rows.Next() {
		var point SeriesPoint
		var latencyP50MS, latencyP95MS uint32
		if err := rows.Scan(
			&point.At, &point.SuccessRate, &latencyP50MS, &latencyP95MS,
			&point.DistinctPerSample, &point.IPChanges,
			&point.Mobile, &point.Residential, &point.Datacenter, &point.Unknown,
		); err != nil {
			return nil, fmt.Errorf("scan series: %w", err)
		}
		point.LatencyP50MS = float64(latencyP50MS)
		point.LatencyP95MS = float64(latencyP95MS)
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate series: %w", err)
	}
	return points, nil
}

// Stickiness derives successful primary-IP holds and rotations in Go.
func (r *Reader) Stickiness(ctx context.Context, sessionID uuid.UUID, from, to time.Time) (Stickiness, error) {
	if from.After(to) {
		return Stickiness{}, ErrInvalidRange
	}
	const query = `SELECT sampled_at, primary_ip, ip_changed
		FROM sample_events
		WHERE session_id = ? AND sampled_at >= ? AND sampled_at <= ? AND probes_ok > 0
		ORDER BY sampled_at, sample_seq`
	rows, err := r.conn.Query(ctx, query, sessionID, from, to)
	if err != nil {
		return Stickiness{}, fmt.Errorf("query stickiness: %w", err)
	}
	defer rows.Close()

	observations := []stickinessObservation{}
	for rows.Next() {
		var item stickinessObservation
		if err := rows.Scan(&item.at, &item.ip, &item.changed); err != nil {
			return Stickiness{}, fmt.Errorf("scan stickiness: %w", err)
		}
		observations = append(observations, item)
	}
	if err := rows.Err(); err != nil {
		return Stickiness{}, fmt.Errorf("iterate stickiness: %w", err)
	}
	return deriveStickiness(observations), nil
}

// PoolGrowth returns the chronological cumulative sum of newly observed IPs.
func (r *Reader) PoolGrowth(ctx context.Context, sessionID uuid.UUID, from, to time.Time) ([]GrowthPoint, error) {
	if from.After(to) {
		return nil, ErrInvalidRange
	}
	const query = `SELECT sampled_at, new_ips
		FROM sample_events
		WHERE session_id = ? AND sampled_at >= ? AND sampled_at <= ?
		ORDER BY sampled_at, sample_seq`
	rows, err := r.conn.Query(ctx, query, sessionID, from, to)
	if err != nil {
		return nil, fmt.Errorf("query pool growth: %w", err)
	}
	defer rows.Close()
	points := []GrowthPoint{}
	var total uint64
	for rows.Next() {
		var at time.Time
		var newIPs uint8
		if err := rows.Scan(&at, &newIPs); err != nil {
			return nil, fmt.Errorf("scan pool growth: %w", err)
		}
		total += uint64(newIPs)
		points = append(points, GrowthPoint{At: at, DistinctIPs: total})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pool growth: %w", err)
	}
	return points, nil
}

// DeleteSession synchronously deletes every sample belonging to a session.
func (r *Reader) DeleteSession(ctx context.Context, sessionID uuid.UUID) error {
	if err := r.conn.Exec(ctx, `ALTER TABLE sample_events DELETE WHERE session_id = ? SETTINGS mutations_sync = 1`, sessionID); err != nil {
		return fmt.Errorf("delete clickhouse session samples: %w", err)
	}
	return nil
}

func optionalRange(from, to *time.Time) error {
	if from != nil && to != nil && from.After(*to) {
		return ErrInvalidRange
	}
	return nil
}

func sampleWhere(sessionID uuid.UUID, from, to *time.Time) (string, []any) {
	clauses := []string{"session_id = ?"}
	args := []any{sessionID}
	if from != nil {
		clauses = append(clauses, "sampled_at >= ?")
		args = append(args, *from)
	}
	if to != nil {
		clauses = append(clauses, "sampled_at <= ?")
		args = append(args, *to)
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

type rowScanner interface{ Scan(...any) error }

func scanEvent(row rowScanner) (Event, error) {
	var event Event
	err := row.Scan(
		&event.SessionID, &event.SampledAt, &event.SampleSeq, &event.ProbesAttempted, &event.ProbesOK,
		&event.PrimaryIP, &event.DistinctIPs, &event.IPChanged, &event.NewIPs,
		&event.RTTMinMS, &event.RTTMedMS, &event.RTTMaxMS,
		&event.EgressCountry, &event.PrimaryCategory, &event.PrimaryRisk,
		&event.ProbeIPs, &event.ProbeRTTsMS, &event.ProbeOK, &event.Error,
	)
	return event, err
}

type stickinessObservation struct {
	at      time.Time
	ip      net.IP
	changed uint8
}

func deriveStickiness(observations []stickinessObservation) Stickiness {
	result := Stickiness{Holds: []Hold{}, Rotations: []Rotation{}}
	if len(observations) == 0 {
		return result
	}
	firstAt := observations[0].at
	current := Hold{IP: observations[0].ip.String(), StartedAt: firstAt, EndedAt: firstAt, Samples: 1}
	var previousRotation time.Time
	for _, observation := range observations[1:] {
		ip := observation.ip.String()
		if ip == current.IP {
			current.EndedAt = observation.at
			current.Samples++
			continue
		}
		current.DurationSeconds = int64(current.EndedAt.Sub(current.StartedAt) / time.Second)
		result.Holds = append(result.Holds, current)
		cadenceStart := previousRotation
		if cadenceStart.IsZero() {
			cadenceStart = firstAt
		}
		result.Rotations = append(result.Rotations, Rotation{
			At:                   observation.at,
			FromIP:               current.IP,
			ToIP:                 ip,
			SincePreviousSeconds: int64(observation.at.Sub(cadenceStart) / time.Second),
		})
		previousRotation = observation.at
		current = Hold{IP: ip, StartedAt: observation.at, EndedAt: observation.at, Samples: 1}
	}
	current.DurationSeconds = int64(current.EndedAt.Sub(current.StartedAt) / time.Second)
	result.Holds = append(result.Holds, current)

	durations := make([]float64, len(result.Holds))
	for i, hold := range result.Holds {
		durations[i] = float64(hold.DurationSeconds)
		result.AverageHoldSeconds += durations[i]
	}
	result.AverageHoldSeconds /= float64(len(durations))
	sort.Float64s(durations)
	middle := len(durations) / 2
	if len(durations)%2 == 1 {
		result.MedianHoldSeconds = durations[middle]
	} else {
		result.MedianHoldSeconds = (durations[middle-1] + durations[middle]) / 2
	}
	return result
}
