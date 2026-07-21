package ch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestInsertQueryUsesMigrationColumnOrder(t *testing.T) {
	want := `INSERT INTO sample_events (
		session_id, sampled_at, sample_seq, probes_attempted, probes_ok,
		primary_ip, distinct_ips, ip_changed, new_ips,
		rtt_min_ms, rtt_med_ms, rtt_max_ms,
		egress_country, primary_category, primary_risk,
		probe_ips, probe_rtts_ms, probe_ok, error
	)`
	if insertQuery != want {
		t.Fatalf("insert query:\n%s\nwant:\n%s", insertQuery, want)
	}
}

func TestWriterFlushesAtBatchSizeWithExplicitArgumentOrder(t *testing.T) {
	opener := &fakeOpener{}
	w := newWriter(opener, nil, writerConfig{BatchSize: 2, FlushInterval: time.Hour, QueueCapacity: 8}, discardLogger())
	t.Cleanup(func() { closeWriter(t, w) })

	first := testEvent(1)
	second := testEvent(2)
	if !w.Enqueue(first) || !w.Enqueue(second) {
		t.Fatal("enqueue rejected")
	}
	opener.waitForSends(t, 1)

	got := opener.batchesSnapshot()
	if len(got) != 1 || len(got[0].rows) != 2 {
		t.Fatalf("batches = %#v", got)
	}
	want := []any{
		first.SessionID, first.SampledAt, first.SampleSeq, first.ProbesAttempted, first.ProbesOK,
		first.PrimaryIP, first.DistinctIPs, first.IPChanged, first.NewIPs,
		first.RTTMinMS, first.RTTMedMS, first.RTTMaxMS,
		first.EgressCountry, first.PrimaryCategory, first.PrimaryRisk,
		first.ProbeIPs, first.ProbeRTTsMS, first.ProbeOK, first.Error,
	}
	assertArgs(t, got[0].rows[0], want)
}

func TestWriterNormalizesMissingIPsAndRejectsMismatchedProbeArrays(t *testing.T) {
	opener := &fakeOpener{}
	w := newWriter(opener, nil, writerConfig{BatchSize: 1, FlushInterval: time.Hour, QueueCapacity: 8}, discardLogger())
	t.Cleanup(func() { closeWriter(t, w) })

	ev := testEvent(1)
	ev.PrimaryIP = nil
	ev.ProbeIPs[0] = nil
	if !w.Enqueue(ev) {
		t.Fatal("valid event rejected")
	}
	opener.waitForSends(t, 1)
	row := opener.batchesSnapshot()[0].rows[0]
	if got := row[5].(net.IP); !got.Equal(net.IPv6zero) {
		t.Fatalf("primary ip = %v", got)
	}
	if got := row[15].([]net.IP)[0]; !got.Equal(net.IPv6zero) {
		t.Fatalf("probe ip = %v", got)
	}

	invalid := testEvent(2)
	invalid.ProbeOK = invalid.ProbeOK[:1]
	if w.Enqueue(invalid) {
		t.Fatal("event with mismatched probe arrays accepted")
	}
}

func TestWriterFlushesOnInterval(t *testing.T) {
	opener := &fakeOpener{}
	w := newWriter(opener, nil, writerConfig{BatchSize: 100, FlushInterval: 10 * time.Millisecond, QueueCapacity: 8}, discardLogger())
	t.Cleanup(func() { closeWriter(t, w) })

	if !w.Enqueue(testEvent(1)) {
		t.Fatal("enqueue rejected")
	}
	opener.waitForSends(t, 1)
}

func TestWriterQueueFullDropsWithoutBlocking(t *testing.T) {
	gate := make(chan struct{})
	opener := &fakeOpener{openGate: gate}
	w := newWriter(opener, nil, writerConfig{BatchSize: 1, FlushInterval: time.Hour, QueueCapacity: 1}, discardLogger())

	if !w.Enqueue(testEvent(1)) {
		t.Fatal("first enqueue rejected")
	}
	opener.waitForOpens(t, 1)
	if !w.Enqueue(testEvent(2)) {
		t.Fatal("second enqueue rejected")
	}
	started := time.Now()
	if w.Enqueue(testEvent(3)) {
		t.Fatal("third enqueue accepted despite full queue")
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("enqueue blocked for %s", elapsed)
	}
	if got := w.Dropped(); got != 1 {
		t.Fatalf("Dropped = %d, want 1", got)
	}
	close(gate)
	closeWriter(t, w)
}

func TestWriterCountsAppendAndSendErrors(t *testing.T) {
	t.Run("append", func(t *testing.T) {
		opener := &fakeOpener{appendErr: errors.New("append")}
		w := newWriter(opener, nil, writerConfig{BatchSize: 1, FlushInterval: time.Hour, QueueCapacity: 1}, discardLogger())
		if !w.Enqueue(testEvent(1)) {
			t.Fatal("enqueue rejected")
		}
		opener.waitForAborts(t, 1)
		if got := w.FlushErrors(); got != 1 {
			t.Fatalf("FlushErrors = %d, want 1", got)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := w.Close(ctx); err == nil || err.Error() != "append" {
			t.Fatalf("Close error = %v, want append error", err)
		}
	})

	t.Run("send", func(t *testing.T) {
		opener := &fakeOpener{sendErr: errors.New("send")}
		w := newWriter(opener, nil, writerConfig{BatchSize: 1, FlushInterval: time.Hour, QueueCapacity: 1}, discardLogger())
		if !w.Enqueue(testEvent(1)) {
			t.Fatal("enqueue rejected")
		}
		opener.waitForSends(t, 1)
		if got := w.FlushErrors(); got != 1 {
			t.Fatalf("FlushErrors = %d, want 1", got)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := w.Close(ctx); err == nil || err.Error() != "send" {
			t.Fatalf("Close error = %v, want send error", err)
		}
	})
}

func TestWriterFlushIsBarrierForAcceptedEvents(t *testing.T) {
	opener := &fakeOpener{}
	w := newWriter(opener, nil, writerConfig{BatchSize: 100, FlushInterval: time.Hour, QueueCapacity: 8}, discardLogger())
	t.Cleanup(func() { closeWriter(t, w) })

	for i := 1; i <= 3; i++ {
		if !w.Enqueue(testEvent(uint32(i))) {
			t.Fatalf("enqueue %d rejected", i)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	got := opener.batchesSnapshot()
	if len(got) != 1 || len(got[0].rows) != 3 {
		t.Fatalf("barrier returned before accepted events were sent: %#v", got)
	}
}

func TestWriterCloseDrainsAndIsIdempotent(t *testing.T) {
	opener := &fakeOpener{}
	closer := &fakeCloser{}
	w := newWriter(opener, closer, writerConfig{BatchSize: 100, FlushInterval: time.Hour, QueueCapacity: 8}, discardLogger())
	for i := 1; i <= 3; i++ {
		if !w.Enqueue(testEvent(uint32(i))) {
			t.Fatalf("enqueue %d rejected", i)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	got := opener.batchesSnapshot()
	if len(got) != 1 || len(got[0].rows) != 3 {
		t.Fatalf("close did not drain: %#v", got)
	}
	if closer.calls != 1 {
		t.Fatalf("closer calls = %d, want 1", closer.calls)
	}
	if w.Enqueue(testEvent(4)) {
		t.Fatal("enqueue accepted after close")
	}
}

func TestWriterCloseHonorsContextWhileFlushIsBlocked(t *testing.T) {
	gate := make(chan struct{})
	opener := &fakeOpener{openGate: gate}
	w := newWriter(opener, nil, writerConfig{BatchSize: 1, FlushInterval: time.Hour, QueueCapacity: 1}, discardLogger())
	if !w.Enqueue(testEvent(1)) {
		t.Fatal("enqueue rejected")
	}
	opener.waitForOpens(t, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := w.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want deadline exceeded", err)
	}
	close(gate)
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := w.Close(ctx2); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func testEvent(seq uint32) Event {
	return Event{
		SessionID:       uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		SampledAt:       time.Date(2026, 7, 21, 12, 0, int(seq), 0, time.UTC),
		SampleSeq:       seq,
		ProbesAttempted: 2,
		ProbesOK:        2,
		PrimaryIP:       net.ParseIP("2001:db8::1"),
		DistinctIPs:     1,
		IPChanged:       1,
		NewIPs:          1,
		RTTMinMS:        10,
		RTTMedMS:        20,
		RTTMaxMS:        30,
		EgressCountry:   "DE",
		PrimaryCategory: "residential",
		PrimaryRisk:     7,
		ProbeIPs:        []net.IP{net.ParseIP("2001:db8::1"), net.ParseIP("2001:db8::1")},
		ProbeRTTsMS:     []uint32{10, 30},
		ProbeOK:         []uint8{1, 1},
		Error:           "",
	}
}

func closeWriter(t *testing.T, w *Writer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeOpener struct {
	mu        sync.Mutex
	batches   []*fakeBatch
	openGate  <-chan struct{}
	appendErr error
	sendErr   error
	opens     chan struct{}
	sends     chan struct{}
	aborts    chan struct{}
}

func (o *fakeOpener) open(ctx context.Context) (batchSink, error) {
	o.mu.Lock()
	if o.opens == nil {
		o.opens = make(chan struct{}, 32)
		o.sends = make(chan struct{}, 32)
		o.aborts = make(chan struct{}, 32)
	}
	opens := o.opens
	o.mu.Unlock()
	opens <- struct{}{}
	if o.openGate != nil {
		select {
		case <-o.openGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	b := &fakeBatch{appendErr: o.appendErr, sendErr: o.sendErr, sends: o.sends, aborts: o.aborts}
	o.mu.Lock()
	o.batches = append(o.batches, b)
	o.mu.Unlock()
	return b, nil
}

func (o *fakeOpener) waitForOpens(t *testing.T, n int)  { o.wait(t, &o.opens, n) }
func (o *fakeOpener) waitForSends(t *testing.T, n int)  { o.wait(t, &o.sends, n) }
func (o *fakeOpener) waitForAborts(t *testing.T, n int) { o.wait(t, &o.aborts, n) }

func (o *fakeOpener) wait(t *testing.T, target *chan struct{}, n int) {
	t.Helper()
	deadline := time.After(time.Second)
	for i := 0; i < n; i++ {
		for {
			o.mu.Lock()
			ch := *target
			o.mu.Unlock()
			if ch == nil {
				time.Sleep(time.Millisecond)
				continue
			}
			select {
			case <-ch:
				goto next
			case <-deadline:
				t.Fatal("timed out waiting for fake sink")
			}
		}
	next:
	}
}

func (o *fakeOpener) batchesSnapshot() []*fakeBatch {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]*fakeBatch(nil), o.batches...)
}

type fakeBatch struct {
	mu        sync.Mutex
	rows      [][]any
	appendErr error
	sendErr   error
	sends     chan<- struct{}
	aborts    chan<- struct{}
}

func (b *fakeBatch) Append(v ...any) error {
	if b.appendErr != nil {
		return b.appendErr
	}
	b.mu.Lock()
	b.rows = append(b.rows, append([]any(nil), v...))
	b.mu.Unlock()
	return nil
}
func (b *fakeBatch) Send() error  { b.sends <- struct{}{}; return b.sendErr }
func (b *fakeBatch) Abort() error { b.aborts <- struct{}{}; return nil }

type fakeCloser struct{ calls int }

func (c *fakeCloser) Close() error { c.calls++; return nil }

func assertArgs(t *testing.T, got, want []any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argument count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if !valuesEqual(got[i], want[i]) {
			t.Errorf("arg[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func valuesEqual(a, b any) bool {
	switch av := a.(type) {
	case net.IP:
		bv, ok := b.(net.IP)
		return ok && av.Equal(bv)
	case []net.IP:
		bv, ok := b.([]net.IP)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !av[i].Equal(bv[i]) {
				return false
			}
		}
		return true
	case []uint32:
		bv, ok := b.([]uint32)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if av[i] != bv[i] {
				return false
			}
		}
		return true
	case []uint8:
		bv, ok := b.([]uint8)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if av[i] != bv[i] {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}
