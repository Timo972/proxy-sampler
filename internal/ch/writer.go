package ch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

const (
	defaultBatchSize     = 100
	defaultFlushInterval = 2 * time.Second
	defaultQueueCapacity = 4096
	flushTimeout         = 15 * time.Second
	dropLogInterval      = 10 * time.Second
)

const insertQuery = `INSERT INTO sample_events (
		session_id, sampled_at, sample_seq, probes_attempted, probes_ok,
		primary_ip, distinct_ips, ip_changed, new_ips,
		rtt_min_ms, rtt_med_ms, rtt_max_ms,
		egress_country, primary_category, primary_risk,
		probe_ips, probe_rtts_ms, probe_ok, error
	)`

var errWriterClosed = errors.New("clickhouse writer closed")

type command struct {
	event   *Event
	barrier chan error
}

type writerConfig struct {
	BatchSize     int
	FlushInterval time.Duration
	QueueCapacity int
}

type batchSink interface {
	Append(v ...any) error
	Send() error
	Abort() error
}

type sinkOpener interface {
	open(context.Context) (batchSink, error)
}

type chOpener struct{ conn driver.Conn }

func (o chOpener) open(ctx context.Context) (batchSink, error) {
	return o.conn.PrepareBatch(ctx, insertQuery)
}

// Writer is a bounded, asynchronous sample-event writer.
type Writer struct {
	open   sinkOpener
	closer io.Closer
	logger *slog.Logger

	commands chan command
	done     chan struct{}

	batchSize     int
	flushInterval time.Duration

	stateMu   sync.RWMutex
	accepting bool
	closeOnce sync.Once
	closeErr  error
	closing   atomic.Bool

	dropped     atomic.Int64
	flushErrors atomic.Int64
	lastDropLog atomic.Int64
	dropLogBusy atomic.Bool
}

// NewWriter opens, pings, and owns a ClickHouse connection.
func NewWriter(ctx context.Context, opts *clickhouse.Options, logger *slog.Logger) (*Writer, error) {
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse writer: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping clickhouse writer: %w", err)
	}
	return newWriter(chOpener{conn: conn}, conn, writerConfig{}, logger), nil
}

func newWriter(open sinkOpener, closer io.Closer, cfg writerConfig, logger *slog.Logger) *Writer {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = defaultFlushInterval
	}
	if cfg.QueueCapacity <= 0 {
		cfg.QueueCapacity = defaultQueueCapacity
	}
	w := &Writer{
		open:          open,
		closer:        closer,
		logger:        logger,
		commands:      make(chan command, cfg.QueueCapacity),
		done:          make(chan struct{}),
		batchSize:     cfg.BatchSize,
		flushInterval: cfg.FlushInterval,
		accepting:     true,
	}
	go w.run()
	return w
}

// Enqueue accepts an event without blocking. It returns false if the event is
// invalid, the writer is closed, or the command queue is full.
func (w *Writer) Enqueue(event Event) bool {
	if !validEvent(event) {
		return false
	}
	if w.closing.Load() {
		return false
	}
	w.stateMu.RLock()
	defer w.stateMu.RUnlock()
	if !w.accepting || w.closing.Load() {
		return false
	}
	select {
	case w.commands <- command{event: &event}:
		return true
	default:
		w.dropped.Add(1)
		w.scheduleDropLog()
		return false
	}
}

// Dropped reports the number of valid events rejected because the queue was full.
func (w *Writer) Dropped() int64 { return w.dropped.Load() }

// FlushErrors reports the number of failed batch flushes.
func (w *Writer) FlushErrors() int64 { return w.flushErrors.Load() }

// Flush waits until every command accepted before its barrier has been handled.
// Callers must first stop and wait for all event producers.
func (w *Writer) Flush(ctx context.Context) error {
	barrier := make(chan error, 1)
	if w.closing.Load() {
		return errWriterClosed
	}
	w.stateMu.RLock()
	if !w.accepting || w.closing.Load() {
		w.stateMu.RUnlock()
		return errWriterClosed
	}
	select {
	case w.commands <- command{barrier: barrier}:
		w.stateMu.RUnlock()
	case <-ctx.Done():
		w.stateMu.RUnlock()
		return ctx.Err()
	}
	select {
	case err := <-barrier:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops accepting events, drains the queue, flushes it, and closes the
// owned connection. Every call is bounded by its own context.
func (w *Writer) Close(ctx context.Context) error {
	w.closeOnce.Do(func() {
		w.closing.Store(true)
		go w.initiateClose()
	})
	select {
	case <-w.done:
		return w.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Writer) initiateClose() {
	w.stateMu.Lock()
	w.accepting = false
	close(w.commands)
	w.stateMu.Unlock()
}

func (w *Writer) run() {
	defer close(w.done)
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	batch := make([]Event, 0, w.batchSize)
	var pendingErr error
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := w.flush(batch)
		batch = batch[:0]
		if err != nil {
			pendingErr = errors.Join(pendingErr, err)
		}
		return err
	}

	for {
		select {
		case cmd, ok := <-w.commands:
			if !ok {
				_ = flush()
				w.closeErr = pendingErr
				if w.closer != nil {
					w.closeErr = errors.Join(w.closeErr, w.closer.Close())
				}
				return
			}
			if cmd.event != nil {
				batch = append(batch, *cmd.event)
				if len(batch) >= w.batchSize {
					_ = flush()
				}
			}
			if cmd.barrier != nil {
				_ = flush()
				cmd.barrier <- pendingErr
				pendingErr = nil
			}
		case <-ticker.C:
			_ = flush()
		}
	}
}

func (w *Writer) flush(events []Event) error {
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	batch, err := w.open.open(ctx)
	if err != nil {
		w.recordFlushError("open batch", len(events), err)
		return err
	}
	for i := range events {
		if err := appendArgs(batch, events[i]); err != nil {
			_ = batch.Abort()
			w.recordFlushError("append batch", len(events), err)
			return err
		}
	}
	if err := batch.Send(); err != nil {
		w.recordFlushError("send batch", len(events), err)
		return err
	}
	return nil
}

func (w *Writer) recordFlushError(operation string, count int, err error) {
	w.flushErrors.Add(1)
	w.logger.Warn("clickhouse sample batch failed", "operation", operation, "count", count, "err", err)
}

func (w *Writer) scheduleDropLog() {
	if w.dropLogBusy.Load() {
		return
	}
	now := time.Now().UnixNano()
	last := w.lastDropLog.Load()
	if now-last < int64(dropLogInterval) || !w.lastDropLog.CompareAndSwap(last, now) {
		return
	}
	if !w.dropLogBusy.CompareAndSwap(false, true) {
		return
	}
	go func(dropped int64) {
		defer w.dropLogBusy.Store(false)
		w.logger.Warn("clickhouse sample queue full; dropping event", "dropped_total", dropped)
	}(w.dropped.Load())
}

func validEvent(event Event) bool {
	want := int(event.ProbesAttempted)
	return len(event.ProbeIPs) == want && len(event.ProbeRTTsMS) == want && len(event.ProbeOK) == want
}

func appendArgs(batch batchSink, event Event) error {
	primaryIP := normalizedIP(event.PrimaryIP)
	probeIPs := make([]net.IP, len(event.ProbeIPs))
	for i := range event.ProbeIPs {
		if event.ProbeOK[i] == 0 {
			probeIPs[i] = append(net.IP(nil), net.IPv6zero...)
		} else {
			probeIPs[i] = normalizedIP(event.ProbeIPs[i])
		}
	}
	return batch.Append(
		event.SessionID, event.SampledAt, event.SampleSeq, event.ProbesAttempted, event.ProbesOK,
		primaryIP, event.DistinctIPs, event.IPChanged, event.NewIPs,
		event.RTTMinMS, event.RTTMedMS, event.RTTMaxMS,
		event.EgressCountry, event.PrimaryCategory, event.PrimaryRisk,
		probeIPs, event.ProbeRTTsMS, event.ProbeOK, event.Error,
	)
}

func normalizedIP(ip net.IP) net.IP {
	if len(ip) == 0 {
		return append(net.IP(nil), net.IPv6zero...)
	}
	return append(net.IP(nil), ip...)
}
