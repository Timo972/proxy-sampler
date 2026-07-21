package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/timo972/proxy-sampler/internal/session"
)

type Store struct {
	pool *pgxpool.Pool
	q    *Queries
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: New(pool)}
}

// Ping verifies the store's Postgres connection for readiness checks.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) Create(ctx context.Context, value session.Session) (session.Session, error) {
	if err := s.q.InsertSession(ctx, insertSessionParams(value)); err != nil {
		return session.Session{}, fmt.Errorf("insert session: %w", err)
	}
	return s.SessionByID(ctx, value.ID)
}

func (s *Store) Sessions(ctx context.Context) ([]session.Session, error) {
	rows, err := s.q.Sessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	result := make([]session.Session, 0, len(rows))
	for _, row := range rows {
		mapped, err := mapSessionsRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, mapped)
	}
	return result, nil
}

func (s *Store) SessionByID(ctx context.Context, id uuid.UUID) (session.Session, error) {
	row, err := s.q.SessionByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return session.Session{}, session.ErrNotFound
	}
	if err != nil {
		return session.Session{}, fmt.Errorf("session by id: %w", err)
	}
	return mapSessionByIDRow(row)
}

func (s *Store) RunningSessions(ctx context.Context) ([]session.Session, error) {
	rows, err := s.q.RunningSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list running sessions: %w", err)
	}
	result := make([]session.Session, 0, len(rows))
	for _, row := range rows {
		mapped, err := mapRunningSessionsRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, mapped)
	}
	return result, nil
}

func (s *Store) Stop(ctx context.Context, id uuid.UUID, at time.Time) error {
	rows, err := s.q.StopSession(ctx, StopSessionParams{ID: id, StoppedAt: timestamp(at)})
	if err != nil {
		return fmt.Errorf("stop session: %w", err)
	}
	if rows == 0 {
		return session.ErrNotRunning
	}
	return nil
}

func (s *Store) Finish(ctx context.Context, id uuid.UUID, at time.Time) error {
	rows, err := s.q.FinishSession(ctx, FinishSessionParams{ID: id, StoppedAt: timestamp(at)})
	if err != nil {
		return fmt.Errorf("finish session: %w", err)
	}
	if rows == 0 {
		return session.ErrNotRunning
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, id uuid.UUID) error {
	if err := s.q.DeleteSession(ctx, id); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (s *Store) SaveTick(ctx context.Context, id uuid.UUID, snap session.Snapshot, hits []session.IPHit) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tick: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)
	if err := q.UpdateSessionSnapshot(ctx, snapshotParams(id, snap)); err != nil {
		return fmt.Errorf("save snapshot: %w", err)
	}
	for _, hit := range hits {
		if err := q.UpsertSessionIP(ctx, hitParams(id, hit)); err != nil {
			return fmt.Errorf("save session ip %s: %w", hit.IP, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tick: %w", err)
	}
	return nil
}

func (s *Store) SessionIPs(ctx context.Context, id uuid.UUID) ([]session.IPRecord, error) {
	rows, err := s.q.SessionIPs(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list session ips: %w", err)
	}
	result := make([]session.IPRecord, 0, len(rows))
	for _, row := range rows {
		ip, err := parseAddr(row.Ip, "session ip")
		if err != nil {
			return nil, err
		}
		record := session.IPRecord{
			IP: ip, FirstSeen: row.FirstSeen.Time, LastSeen: row.LastSeen.Time, HitCount: row.HitCount,
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
			record.Reputation = &reputation
		}
		result = append(result, record)
	}
	return result, nil
}

func (s *Store) ReputationByIP(ctx context.Context, ip netip.Addr) (session.Reputation, bool, error) {
	row, err := s.q.ReputationByIP(ctx, ip.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return session.Reputation{}, false, nil
	}
	if err != nil {
		return session.Reputation{}, false, fmt.Errorf("reputation by ip: %w", err)
	}
	reputation, err := reputationFromValues(reputationValues{
		IP: row.Ip, Country: row.Country, Region: row.Region, City: row.City,
		ISP: row.Isp, ASN: row.Asn, IsMobile: row.IsMobile, IPAPIProxy: row.IpapiProxy,
		IPAPIHosting: row.IpapiHosting, ProxyCheckType: row.PcType,
		ProxyCheckProxy: row.PcProxy, RiskScore: row.RiskScore,
		GreyNoiseClass: row.GreynoiseClass, SFSAppears: row.SfsAppears,
		SFSFrequency: row.SfsFrequency, DNSBLListed: row.DnsblListed,
		DNSBLHits: row.DnsblHits, Category: row.Category, Raw: row.Raw,
		FirstSeen: row.FirstSeen, RefreshedAt: row.RefreshedAt,
	})
	if err != nil {
		return session.Reputation{}, false, err
	}
	return reputation, true, nil
}

func (s *Store) SaveReputation(ctx context.Context, reputation session.Reputation) error {
	dnsblHits, err := json.Marshal(reputation.DNSBLHits)
	if err != nil {
		return fmt.Errorf("encode DNSBL hits: %w", err)
	}
	raw := reputation.Raw
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := s.q.UpsertReputation(ctx, UpsertReputationParams{
		Ip: reputation.IP.String(), Country: reputation.Country, Region: reputation.Region,
		City: reputation.City, Isp: reputation.ISP, Asn: reputation.ASN,
		IsMobile: reputation.IsMobile, IpapiProxy: reputation.IPAPIProxy,
		IpapiHosting: reputation.IPAPIHosting, PcType: reputation.ProxyCheckType,
		PcProxy: reputation.ProxyCheckProxy, RiskScore: int32Ptr(reputation.RiskScore),
		GreynoiseClass: reputation.GreyNoiseClass, SfsAppears: reputation.SFSAppears,
		SfsFrequency: int32Ptr(reputation.SFSFrequency), DnsblListed: reputation.DNSBLListed,
		DnsblHits: string(dnsblHits), Category: reputation.Category, Raw: raw,
		FirstSeen: timestamp(reputation.FirstSeen), RefreshedAt: timestamp(reputation.RefreshedAt),
	}); err != nil {
		return fmt.Errorf("upsert reputation: %w", err)
	}
	return nil
}

type sessionRow struct {
	ID                 uuid.UUID
	Name               string
	ProxyCiphertext    []byte
	ProxyNonce         []byte
	ProxyDisplay       string
	Mode               string
	CadenceSeconds     int32
	ProbesPerSample    int32
	ProbeTarget        string
	DialTimeoutMs      int32
	MaxSamples         *int32
	MaxDurationSeconds *int32
	Status             string
	SamplesTaken       int32
	ProbesOK           int64
	ProbesTotal        int64
	DistinctIPs        int32
	LastSampleAt       pgtype.Timestamptz
	LastPrimaryIP      string
	LastCategory       string
	LastRTTMs          *int32
	LastError          *string
	CreatedAt          pgtype.Timestamptz
	StartedAt          pgtype.Timestamptz
	StoppedAt          pgtype.Timestamptz
}

func mapSession(row sessionRow) (session.Session, error) {
	lastPrimaryIP, err := parseOptionalAddr(row.LastPrimaryIP)
	if err != nil {
		return session.Session{}, fmt.Errorf("map last primary ip: %w", err)
	}
	return session.Session{
		ID: row.ID, Name: row.Name, ProxyCiphertext: row.ProxyCiphertext,
		ProxyNonce: row.ProxyNonce, ProxyDisplay: row.ProxyDisplay, Mode: session.Mode(row.Mode),
		Cadence:         time.Duration(row.CadenceSeconds) * time.Second,
		ProbesPerSample: int(row.ProbesPerSample), ProbeTarget: row.ProbeTarget,
		DialTimeout: time.Duration(row.DialTimeoutMs) * time.Millisecond,
		MaxSamples:  intPtr(row.MaxSamples), MaxDuration: secondsDurationPtr(row.MaxDurationSeconds),
		Status: session.Status(row.Status), CreatedAt: row.CreatedAt.Time,
		StartedAt: timePtr(row.StartedAt), StoppedAt: timePtr(row.StoppedAt),
		Snapshot: session.Snapshot{
			SamplesTaken: int(row.SamplesTaken), ProbesOK: row.ProbesOK, ProbesTotal: row.ProbesTotal,
			DistinctIPs: int(row.DistinctIPs), LastSampleAt: timePtr(row.LastSampleAt),
			LastPrimaryIP: lastPrimaryIP, LastCategory: row.LastCategory,
			LastRTT: millisDurationPtr(row.LastRTTMs), LastError: stringValue(row.LastError),
		},
	}, nil
}

func mapSessionByIDRow(row SessionByIDRow) (session.Session, error) {
	return mapSession(sessionRow{
		ID: row.ID, Name: row.Name, ProxyCiphertext: row.ProxyCiphertext, ProxyNonce: row.ProxyNonce,
		ProxyDisplay: row.ProxyDisplay, Mode: row.Mode, CadenceSeconds: row.CadenceSeconds,
		ProbesPerSample: row.ProbesPerSample, ProbeTarget: row.ProbeTarget, DialTimeoutMs: row.DialTimeoutMs,
		MaxSamples: row.MaxSamples, MaxDurationSeconds: row.MaxDurationSeconds, Status: row.Status,
		SamplesTaken: row.SamplesTaken, ProbesOK: row.ProbesOk, ProbesTotal: row.ProbesTotal,
		DistinctIPs: row.DistinctIps, LastSampleAt: row.LastSampleAt, LastPrimaryIP: row.LastPrimaryIp,
		LastCategory: row.LastCategory, LastRTTMs: row.LastRttMs, LastError: row.LastError,
		CreatedAt: row.CreatedAt, StartedAt: row.StartedAt, StoppedAt: row.StoppedAt,
	})
}

func mapSessionsRow(row SessionsRow) (session.Session, error) {
	return mapSession(sessionRow{
		ID: row.ID, Name: row.Name, ProxyCiphertext: row.ProxyCiphertext, ProxyNonce: row.ProxyNonce,
		ProxyDisplay: row.ProxyDisplay, Mode: row.Mode, CadenceSeconds: row.CadenceSeconds,
		ProbesPerSample: row.ProbesPerSample, ProbeTarget: row.ProbeTarget, DialTimeoutMs: row.DialTimeoutMs,
		MaxSamples: row.MaxSamples, MaxDurationSeconds: row.MaxDurationSeconds, Status: row.Status,
		SamplesTaken: row.SamplesTaken, ProbesOK: row.ProbesOk, ProbesTotal: row.ProbesTotal,
		DistinctIPs: row.DistinctIps, LastSampleAt: row.LastSampleAt, LastPrimaryIP: row.LastPrimaryIp,
		LastCategory: row.LastCategory, LastRTTMs: row.LastRttMs, LastError: row.LastError,
		CreatedAt: row.CreatedAt, StartedAt: row.StartedAt, StoppedAt: row.StoppedAt,
	})
}

func mapRunningSessionsRow(row RunningSessionsRow) (session.Session, error) {
	return mapSession(sessionRow{
		ID: row.ID, Name: row.Name, ProxyCiphertext: row.ProxyCiphertext, ProxyNonce: row.ProxyNonce,
		ProxyDisplay: row.ProxyDisplay, Mode: row.Mode, CadenceSeconds: row.CadenceSeconds,
		ProbesPerSample: row.ProbesPerSample, ProbeTarget: row.ProbeTarget, DialTimeoutMs: row.DialTimeoutMs,
		MaxSamples: row.MaxSamples, MaxDurationSeconds: row.MaxDurationSeconds, Status: row.Status,
		SamplesTaken: row.SamplesTaken, ProbesOK: row.ProbesOk, ProbesTotal: row.ProbesTotal,
		DistinctIPs: row.DistinctIps, LastSampleAt: row.LastSampleAt, LastPrimaryIP: row.LastPrimaryIp,
		LastCategory: row.LastCategory, LastRTTMs: row.LastRttMs, LastError: row.LastError,
		CreatedAt: row.CreatedAt, StartedAt: row.StartedAt, StoppedAt: row.StoppedAt,
	})
}

func insertSessionParams(value session.Session) InsertSessionParams {
	return InsertSessionParams{
		ID: value.ID, Name: value.Name, ProxyCiphertext: value.ProxyCiphertext,
		ProxyNonce: value.ProxyNonce, ProxyDisplay: value.ProxyDisplay, Mode: string(value.Mode),
		CadenceSeconds: int32(value.Cadence / time.Second), ProbesPerSample: int32(value.ProbesPerSample),
		ProbeTarget: value.ProbeTarget, DialTimeoutMs: int32(value.DialTimeout / time.Millisecond),
		MaxSamples: int32Ptr(value.MaxSamples), MaxDurationSeconds: durationSecondsPtr(value.MaxDuration),
		Status: string(value.Status), SamplesTaken: int32(value.Snapshot.SamplesTaken),
		ProbesOk: value.Snapshot.ProbesOK, ProbesTotal: value.Snapshot.ProbesTotal,
		DistinctIps: int32(value.Snapshot.DistinctIPs), LastSampleAt: timestampPtr(value.Snapshot.LastSampleAt),
		LastPrimaryIp: addrString(value.Snapshot.LastPrimaryIP), LastRttMs: durationMillisPtr(value.Snapshot.LastRTT),
		LastError: value.Snapshot.LastError, CreatedAt: timestamp(value.CreatedAt),
		StartedAt: timestampPtr(value.StartedAt), StoppedAt: timestampPtr(value.StoppedAt),
	}
}

func snapshotParams(id uuid.UUID, value session.Snapshot) UpdateSessionSnapshotParams {
	return UpdateSessionSnapshotParams{
		ID: id, SamplesTaken: int32(value.SamplesTaken), ProbesOk: value.ProbesOK,
		ProbesTotal: value.ProbesTotal, DistinctIps: int32(value.DistinctIPs),
		LastSampleAt: timestampPtr(value.LastSampleAt), LastPrimaryIp: addrString(value.LastPrimaryIP),
		LastRttMs: durationMillisPtr(value.LastRTT), LastError: value.LastError,
	}
}

func hitParams(id uuid.UUID, hit session.IPHit) UpsertSessionIPParams {
	return UpsertSessionIPParams{SessionID: id, Ip: hit.IP.String(), SeenAt: timestamp(hit.SeenAt), Hits: hit.Hits}
}

type reputationValues struct {
	IP              string
	Country         *string
	Region          *string
	City            *string
	ISP             *string
	ASN             *string
	IsMobile        *bool
	IPAPIProxy      *bool
	IPAPIHosting    *bool
	ProxyCheckType  *string
	ProxyCheckProxy *bool
	RiskScore       *int32
	GreyNoiseClass  *string
	SFSAppears      *bool
	SFSFrequency    *int32
	DNSBLListed     *bool
	DNSBLHits       *string
	Category        *string
	Raw             []byte
	FirstSeen       pgtype.Timestamptz
	RefreshedAt     pgtype.Timestamptz
}

func reputationFromValues(value reputationValues) (session.Reputation, error) {
	ip, err := parseAddr(value.IP, "reputation ip")
	if err != nil {
		return session.Reputation{}, err
	}
	var dnsblHits []string
	if value.DNSBLHits != nil && *value.DNSBLHits != "" {
		if err := json.Unmarshal([]byte(*value.DNSBLHits), &dnsblHits); err != nil {
			return session.Reputation{}, fmt.Errorf("decode DNSBL hits: %w", err)
		}
	}
	return session.Reputation{
		IP: ip, Country: stringValue(value.Country), Region: stringValue(value.Region),
		City: stringValue(value.City), ISP: stringValue(value.ISP), ASN: stringValue(value.ASN),
		IsMobile: value.IsMobile, IPAPIProxy: value.IPAPIProxy, IPAPIHosting: value.IPAPIHosting,
		ProxyCheckType: stringValue(value.ProxyCheckType), ProxyCheckProxy: value.ProxyCheckProxy,
		RiskScore: intPtr(value.RiskScore), GreyNoiseClass: stringValue(value.GreyNoiseClass),
		SFSAppears: value.SFSAppears, SFSFrequency: intPtr(value.SFSFrequency),
		DNSBLListed: value.DNSBLListed, DNSBLHits: dnsblHits, Category: stringValue(value.Category),
		Raw: json.RawMessage(value.Raw), FirstSeen: value.FirstSeen.Time, RefreshedAt: value.RefreshedAt.Time,
	}, nil
}

func parseOptionalAddr(value string) (netip.Addr, error) {
	if value == "" {
		return netip.Addr{}, nil
	}
	return parseAddr(value, "address")
}

func parseAddr(value, field string) (netip.Addr, error) {
	address, err := netip.ParseAddr(value)
	if err == nil {
		return address, nil
	}
	prefix, prefixErr := netip.ParsePrefix(value)
	if prefixErr == nil {
		return prefix.Addr(), nil
	}
	return netip.Addr{}, fmt.Errorf("map %s: %w", field, err)
}

func addrString(value netip.Addr) string {
	if !value.IsValid() {
		return ""
	}
	return value.String()
}

func timestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

func timestampPtr(value *time.Time) pgtype.Timestamptz {
	if value == nil {
		return pgtype.Timestamptz{}
	}
	return timestamp(*value)
}

func timePtr(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time
	return &result
}

func int32Ptr(value *int) *int32 {
	if value == nil {
		return nil
	}
	result := int32(*value)
	return &result
}

func intPtr(value *int32) *int {
	if value == nil {
		return nil
	}
	result := int(*value)
	return &result
}

func durationSecondsPtr(value *time.Duration) *int32 {
	if value == nil {
		return nil
	}
	result := int32(*value / time.Second)
	return &result
}

func durationMillisPtr(value *time.Duration) *int32 {
	if value == nil {
		return nil
	}
	result := int32(*value / time.Millisecond)
	return &result
}

func secondsDurationPtr(value *int32) *time.Duration {
	if value == nil {
		return nil
	}
	result := time.Duration(*value) * time.Second
	return &result
}

func millisDurationPtr(value *int32) *time.Duration {
	if value == nil {
		return nil
	}
	result := time.Duration(*value) * time.Millisecond
	return &result
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

var _ session.Store = (*Store)(nil)
