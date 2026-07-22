package sampler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/proxy"
	"golang.org/x/sync/semaphore"

	"github.com/timo972/proxy-sampler/internal/ch"
	appcrypto "github.com/timo972/proxy-sampler/internal/crypto"
	"github.com/timo972/proxy-sampler/internal/proxydial"
	"github.com/timo972/proxy-sampler/internal/session"
)

const (
	maxConcurrentProbes     = 8
	terminalTransitionRetry = time.Second
	unknownPrimaryCategory  = "unknown"
)

// ReputationLookup enriches one successful egress IP. Implementations may
// return a useful partial reputation together with an error.
type ReputationLookup interface {
	Lookup(context.Context, netip.Addr) (session.Reputation, error)
}

// EventSink accepts persisted sample events and provides a deletion barrier.
type EventSink interface {
	Enqueue(ch.Event) bool
	Flush(context.Context) error
}

// DialerFactory builds the credential-bearing dialer during preparation.
type DialerFactory func(string, time.Duration) (proxy.ContextDialer, error)

type workerClock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                             { return time.Now() }
func (realClock) After(delay time.Duration) <-chan time.Time { return time.After(delay) }

// Worker owns the process dependencies needed to prepare one durable session.
type Worker struct {
	store         session.Store
	cipher        *appcrypto.Cipher
	prober        Prober
	lookup        ReputationLookup
	sink          EventSink
	logger        *slog.Logger
	dialerFactory DialerFactory
	clock         workerClock
	dropped       atomic.Int64
}

// NewWorker constructs a worker template. Prepare performs all session-specific
// I/O synchronously before Run may be published as a goroutine.
func NewWorker(store session.Store, cipher *appcrypto.Cipher, prober Prober, lookup ReputationLookup, sink EventSink, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		store: store, cipher: cipher, prober: prober, lookup: lookup, sink: sink,
		logger: logger, dialerFactory: proxydial.FromURL, clock: realClock{},
	}
}

// DroppedEvents reports samples that were durably saved but rejected by the
// bounded ClickHouse queue.
func (w *Worker) DroppedEvents() int64 { return w.dropped.Load() }

// PreparedWorker contains only fully initialized, session-specific state.
type PreparedWorker struct {
	worker          *Worker
	session         session.Session
	dialer          proxy.ContextDialer
	seen            map[netip.Addr]struct{}
	previousPrimary netip.Addr
	sampleSeq       uint32
}

// Prepare decrypts credentials, constructs the dialer, and restores rolling
// state before the supervisor publishes a goroutine.
func (w *Worker) Prepare(ctx context.Context, value session.Session) (*PreparedWorker, error) {
	if w == nil || w.store == nil || w.cipher == nil || w.prober == nil || w.lookup == nil || w.sink == nil {
		return nil, errors.New("worker dependencies are incomplete")
	}
	proxyURL, err := w.cipher.Decrypt(value.ProxyCiphertext, value.ProxyNonce)
	if err != nil {
		return nil, fmt.Errorf("prepare session %s: %w", value.ID, err)
	}
	dialer, err := w.dialerFactory(proxyURL, value.DialTimeout)
	if err != nil {
		return nil, fmt.Errorf("prepare session %s dialer: %w", value.ID, err)
	}
	records, err := w.store.SessionIPs(ctx, value.ID)
	if err != nil {
		return nil, fmt.Errorf("prepare session %s seen IPs: %w", value.ID, err)
	}
	seen := make(map[netip.Addr]struct{}, len(records))
	for _, record := range records {
		if record.IP.IsValid() {
			seen[record.IP.Unmap()] = struct{}{}
		}
	}
	sequence := value.Snapshot.SamplesTaken
	if sequence < 0 {
		sequence = 0
	}
	if sequence > math.MaxUint32 {
		sequence = math.MaxUint32
	}
	return &PreparedWorker{
		worker: w, session: value, dialer: dialer, seen: seen,
		previousPrimary: value.Snapshot.LastPrimaryIP, sampleSeq: uint32(sequence),
	}, nil
}

// Run samples immediately and then once per configured cadence until canceled
// or a persisted session cap is reached.
func (p *PreparedWorker) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if p.capReached() {
			p.finish(ctx)
			return
		}
		err := p.tick(ctx)
		if err != nil && ctx.Err() == nil {
			p.worker.logger.Warn("sampling tick was not persisted", "session_id", p.session.ID, "err", err)
		}
		if ctx.Err() != nil {
			return
		}
		if err == nil && p.capReached() {
			p.finish(ctx)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-p.worker.clock.After(p.session.Cadence):
		}
	}
}

func (p *PreparedWorker) capReached() bool {
	if p.maxSamplesReached() {
		return true
	}
	return p.session.MaxDuration != nil && p.session.StartedAt != nil &&
		p.worker.clock.Now().Sub(*p.session.StartedAt) >= *p.session.MaxDuration
}

func (p *PreparedWorker) maxSamplesReached() bool {
	return p.session.MaxSamples != nil && int(p.sampleSeq) >= *p.session.MaxSamples
}

func (p *PreparedWorker) finish(ctx context.Context) {
	for {
		err := p.worker.store.Finish(ctx, p.session.ID, p.worker.clock.Now())
		if err == nil || ctx.Err() != nil {
			return
		}
		p.worker.logger.Warn("mark capped sampling session finished", "session_id", p.session.ID, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-p.worker.clock.After(terminalTransitionRetry):
		}
	}
}

func (p *PreparedWorker) tick(ctx context.Context) error {
	sampledAt := p.worker.clock.Now()
	results := p.probe(ctx)
	sample := Aggregate(results, p.previousPrimary, p.seen)
	reputations := p.enrich(ctx, sample)

	nextSeen := make(map[netip.Addr]struct{}, len(p.seen)+len(sample.IPHits))
	for ip := range p.seen {
		nextSeen[ip] = struct{}{}
	}
	hits := make([]session.IPHit, 0, len(sample.IPHits))
	added := make(map[netip.Addr]struct{}, len(sample.IPHits))
	for _, ip := range sample.ProbeIPs {
		if !ip.IsValid() {
			continue
		}
		ip = ip.Unmap()
		if _, exists := added[ip]; exists {
			continue
		}
		added[ip] = struct{}{}
		nextSeen[ip] = struct{}{}
		hits = append(hits, session.IPHit{IP: ip, SeenAt: sampledAt, Hits: int64(sample.IPHits[ip])})
	}

	nextSequence := p.sampleSeq + 1
	nextPrimary := p.previousPrimary
	if sample.PrimaryIP.IsValid() {
		nextPrimary = sample.PrimaryIP
	}
	nextSnapshot := session.Snapshot{
		SamplesTaken: int(nextSequence),
		ProbesOK:     p.session.Snapshot.ProbesOK + int64(sample.ProbesOK),
		ProbesTotal:  p.session.Snapshot.ProbesTotal + int64(sample.ProbesAttempted),
		DistinctIPs:  len(nextSeen), LastSampleAt: timePointer(sampledAt),
		LastPrimaryIP: nextPrimary, LastCategory: p.session.Snapshot.LastCategory,
		LastError: sample.Error,
	}
	if sample.ProbesOK > 0 {
		nextSnapshot.LastRTT = durationPointer(sample.RTTMed)
	}
	if reputation, ok := reputations[sample.PrimaryIP]; ok {
		nextSnapshot.LastCategory = reputation.Category
	}
	event := sampleEvent(p.session.ID, sampledAt, nextSequence, sample, reputations[sample.PrimaryIP])

	if err := p.worker.store.SaveTick(ctx, p.session.ID, nextSnapshot, hits); err != nil {
		return err
	}
	p.seen = nextSeen
	p.previousPrimary = nextPrimary
	p.sampleSeq = nextSequence
	p.session.Snapshot = nextSnapshot
	if !p.worker.sink.Enqueue(event) {
		dropped := p.worker.dropped.Add(1)
		p.worker.logger.Warn("clickhouse sample queue full; dropping persisted event", "session_id", p.session.ID, "sample_seq", nextSequence, "dropped_total", dropped)
	}
	return nil
}

func (p *PreparedWorker) probe(ctx context.Context) []ProbeResult {
	count := p.session.ProbesPerSample
	if count < 0 {
		count = 0
	}
	if count > maxUInt8Value {
		count = maxUInt8Value
	}
	limit := count
	if limit > maxConcurrentProbes {
		limit = maxConcurrentProbes
	}
	return runOrderedProbes(ctx, count, limit, func(ctx context.Context, _ int) ProbeResult {
		return p.worker.prober.Probe(ctx, p.dialer, p.session.ProbeTarget, p.session.DialTimeout)
	})
}

func runOrderedProbes(ctx context.Context, count, limit int, probe func(context.Context, int) ProbeResult) []ProbeResult {
	results := make([]ProbeResult, count)
	if count == 0 || limit <= 0 {
		return results
	}
	sem := semaphore.NewWeighted(int64(limit))
	var wg sync.WaitGroup
	for index := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if err := sem.Acquire(ctx, 1); err != nil {
				results[index] = ProbeResult{Err: err}
				return
			}
			defer sem.Release(1)
			results[index] = probe(ctx, index)
		}(index)
	}
	wg.Wait()
	return results
}

func (p *PreparedWorker) enrich(ctx context.Context, sample Sample) map[netip.Addr]session.Reputation {
	result := make(map[netip.Addr]session.Reputation, len(sample.IPHits))
	for _, ip := range sample.ProbeIPs {
		if !ip.IsValid() {
			continue
		}
		ip = ip.Unmap()
		if _, exists := result[ip]; exists {
			continue
		}
		reputation, err := p.worker.lookup.Lookup(ctx, ip)
		if reputation.Category == "" {
			reputation.Category = unknownPrimaryCategory
		}
		result[ip] = reputation
		if err != nil && ctx.Err() == nil {
			p.worker.logger.Warn("IP enrichment incomplete", "session_id", p.session.ID, "ip", ip, "err", err)
		}
	}
	return result
}

func sampleEvent(id uuid.UUID, at time.Time, sequence uint32, sample Sample, reputation session.Reputation) ch.Event {
	if reputation.Category == "" {
		reputation.Category = unknownPrimaryCategory
	}
	event := ch.Event{
		SessionID: id, SampledAt: at, SampleSeq: sequence,
		ProbesAttempted: uint8(sample.ProbesAttempted), ProbesOK: uint8(sample.ProbesOK),
		PrimaryIP: addrIP(sample.PrimaryIP), DistinctIPs: uint8(sample.DistinctIPs),
		IPChanged: boolByte(sample.IPChanged), NewIPs: uint8(sample.NewIPs),
		RTTMinMS: durationMillis(sample.RTTMin), RTTMedMS: durationMillis(sample.RTTMed), RTTMaxMS: durationMillis(sample.RTTMax),
		EgressCountry: sample.EgressCountry, PrimaryCategory: reputation.Category,
		PrimaryRisk: riskByte(reputation.RiskScore), Error: sample.Error,
		ProbeIPs: make([]net.IP, len(sample.ProbeIPs)), ProbeRTTsMS: make([]uint32, len(sample.ProbeRTTs)), ProbeOK: make([]uint8, len(sample.ProbeOK)),
	}
	for index := range sample.ProbeIPs {
		event.ProbeIPs[index] = addrIP(sample.ProbeIPs[index])
		event.ProbeRTTsMS[index] = durationMillis(sample.ProbeRTTs[index])
		event.ProbeOK[index] = boolByte(sample.ProbeOK[index])
	}
	return event
}

func addrIP(address netip.Addr) net.IP {
	if !address.IsValid() {
		return nil
	}
	return net.IP(append([]byte(nil), address.Unmap().AsSlice()...))
}
func durationMillis(value time.Duration) uint32 {
	if value <= 0 {
		return 0
	}
	millis := value.Milliseconds()
	if millis > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(millis)
}
func riskByte(value *int) uint8 {
	if value == nil || *value <= 0 {
		return 0
	}
	if *value > math.MaxUint8 {
		return math.MaxUint8
	}
	return uint8(*value)
}
func boolByte(value bool) uint8 {
	if value {
		return 1
	}
	return 0
}
func timePointer(value time.Time) *time.Time             { return &value }
func durationPointer(value time.Duration) *time.Duration { return &value }
