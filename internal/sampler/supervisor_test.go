package sampler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/proxy"

	"github.com/timo972/proxy-sampler/internal/session"
)

func TestSupervisorLifecycle(t *testing.T) {
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	store := newFakeSessionStore()
	running, cipher := encryptedSession(t, 1)
	stopped, _ := encryptedSession(t, 1)
	stopped.Status = session.StatusStopped
	store.sessions[running.ID], store.sessions[stopped.ID] = running, stopped
	sink := &fakeEventSink{operations: &store.operations}
	reader := &fakeSessionReader{operations: &store.operations}
	started := make(chan struct{}, 4)
	factory := func(value session.Session) *Worker {
		worker := testWorker(store, cipher, blockingProber{started: started}, &fakeLookup{}, sink)
		return worker
	}
	supervisor := NewSupervisor(root, store, sink, reader, factory)

	if err := supervisor.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	<-started
	if err := supervisor.Start(context.Background(), running.ID); err != nil {
		t.Fatalf("idempotent Start: %v", err)
	}
	supervisor.mu.Lock()
	count := len(supervisor.workers)
	supervisor.mu.Unlock()
	if count != 1 {
		t.Fatalf("workers = %d, want 1", count)
	}
	if err := supervisor.Start(context.Background(), stopped.ID); !errors.Is(err, session.ErrNotRunning) {
		t.Fatalf("Start stopped = %v", err)
	}

	if err := supervisor.Stop(context.Background(), running.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if store.stopCount.Load() != 1 {
		t.Fatalf("stop count = %d", store.stopCount.Load())
	}
	if err := supervisor.Stop(context.Background(), running.ID); !errors.Is(err, session.ErrNotRunning) {
		t.Fatalf("second Stop = %v", err)
	}
}

func TestSupervisorDeleteOrdersStopFlushCHAndPostgres(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newFakeSessionStore()
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	sink := &fakeEventSink{operations: &store.operations}
	reader := &fakeSessionReader{operations: &store.operations}
	supervisor := NewSupervisor(root, store, sink, reader, func(session.Session) *Worker { return testWorker(store, cipher, blockingProber{}, &fakeLookup{}, sink) })
	if err := supervisor.Start(context.Background(), value.ID); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Delete(context.Background(), value.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	want := []string{"stop", "flush", "ch-delete", "pg-delete"}
	if len(store.operations) != len(want) {
		t.Fatalf("operations = %v", store.operations)
	}
	for i := range want {
		if store.operations[i] != want[i] {
			t.Fatalf("operations = %v, want %v", store.operations, want)
		}
	}
}

func TestSupervisorDeletePreservesPostgresWhenClickHouseDeleteFails(t *testing.T) {
	store := newFakeSessionStore()
	value, _ := encryptedSession(t, 1)
	value.Status = session.StatusStopped
	store.sessions[value.ID] = value
	sink := &fakeEventSink{operations: &store.operations}
	reader := &fakeSessionReader{operations: &store.operations, err: errors.New("clickhouse unavailable")}
	supervisor := NewSupervisor(context.Background(), store, sink, reader, nil)
	if err := supervisor.Delete(context.Background(), value.ID); err == nil {
		t.Fatal("Delete error = nil")
	}
	if _, ok := store.sessions[value.ID]; !ok {
		t.Fatal("Postgres control row deleted after ClickHouse failure")
	}
}

func TestSupervisorRootCancellationWaitsWithoutChangingStatus(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	store := newFakeSessionStore()
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	sink := &fakeEventSink{}
	supervisor := NewSupervisor(root, store, sink, &fakeSessionReader{}, func(session.Session) *Worker { return testWorker(store, cipher, blockingProber{}, &fakeLookup{}, sink) })
	if err := supervisor.Start(context.Background(), value.ID); err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan struct{})
	go func() { supervisor.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait did not return")
	}
	if store.stopCount.Load() != 0 || store.finishCount.Load() != 0 {
		t.Fatal("root cancellation changed status")
	}
}

func TestSupervisorDoesNotPublishWorkersAfterRootCancellation(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	cancel()
	store := newFakeSessionStore()
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	sink := &fakeEventSink{}
	supervisor := NewSupervisor(root, store, sink, &fakeSessionReader{}, func(session.Session) *Worker {
		return testWorker(store, cipher, fixedProber{}, &fakeLookup{}, sink)
	})
	if err := supervisor.Start(context.Background(), value.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start after root cancellation = %v, want context.Canceled", err)
	}
	supervisor.Wait()
	supervisor.mu.Lock()
	count := len(supervisor.workers)
	supervisor.mu.Unlock()
	if count != 0 {
		t.Fatalf("workers = %d, want 0", count)
	}
}

func TestSupervisorSerializesStartWithStop(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newFakeSessionStore()
	store.ipsStarted, store.ipsRelease = make(chan struct{}), make(chan struct{})
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	sink := &fakeEventSink{}
	supervisor := NewSupervisor(root, store, sink, &fakeSessionReader{}, func(session.Session) *Worker {
		return testWorker(store, cipher, blockingProber{}, &fakeLookup{}, sink)
	})
	startResult := make(chan error, 1)
	go func() { startResult <- supervisor.Start(context.Background(), value.ID) }()
	<-store.ipsStarted
	stopResult := make(chan error, 1)
	go func() { stopResult <- supervisor.Stop(context.Background(), value.ID) }()
	var stopErr error
	stopDone := false
	select {
	case stopErr = <-stopResult:
		stopDone = true
		// Before the fix, Stop completes against the row while Start is preparing it.
	case <-time.After(20 * time.Millisecond):
	}
	close(store.ipsRelease)
	if err := <-startResult; err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !stopDone {
		stopErr = <-stopResult
	}
	if stopErr != nil {
		t.Fatalf("Stop: %v", stopErr)
	}
	supervisor.mu.Lock()
	count := len(supervisor.workers)
	supervisor.mu.Unlock()
	if count != 0 {
		t.Fatalf("workers after Stop = %d, want 0", count)
	}
}

func TestSupervisorSerializesStartWithDelete(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newFakeSessionStore()
	store.ipsStarted, store.ipsRelease = make(chan struct{}), make(chan struct{})
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	sink := &fakeEventSink{operations: &store.operations}
	supervisor := NewSupervisor(root, store, sink, &fakeSessionReader{operations: &store.operations}, func(session.Session) *Worker {
		return testWorker(store, cipher, blockingProber{}, &fakeLookup{}, sink)
	})
	startResult := make(chan error, 1)
	go func() { startResult <- supervisor.Start(context.Background(), value.ID) }()
	<-store.ipsStarted
	deleteResult := make(chan error, 1)
	go func() { deleteResult <- supervisor.Delete(context.Background(), value.ID) }()
	var deleteErr error
	deleteDone := false
	select {
	case deleteErr = <-deleteResult:
		deleteDone = true
	case <-time.After(20 * time.Millisecond):
	}
	close(store.ipsRelease)
	if err := <-startResult; err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !deleteDone {
		deleteErr = <-deleteResult
	}
	if deleteErr != nil {
		t.Fatalf("Delete: %v", deleteErr)
	}
	supervisor.mu.Lock()
	count := len(supervisor.workers)
	supervisor.mu.Unlock()
	if count != 0 {
		t.Fatalf("workers after Delete = %d, want 0", count)
	}
}

func TestSupervisorWaitClosesPublicationGate(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newFakeSessionStore()
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	sink := &fakeEventSink{}
	supervisor := NewSupervisor(root, store, sink, &fakeSessionReader{}, func(session.Session) *Worker {
		return testWorker(store, cipher, fixedProber{}, &fakeLookup{}, sink)
	})
	supervisor.Wait()
	if err := supervisor.Start(context.Background(), value.ID); err == nil {
		t.Fatal("Start after Wait = nil, want publication rejection")
	}
}

type blockingProber struct{ started chan struct{} }

func (p blockingProber) Probe(ctx context.Context, _ proxy.ContextDialer, _ string, _ time.Duration) ProbeResult {
	if p.started != nil {
		select {
		case p.started <- struct{}{}:
		default:
		}
	}
	<-ctx.Done()
	return ProbeResult{Err: ctx.Err()}
}

type fakeSessionReader struct {
	mu         sync.Mutex
	operations *[]string
	err        error
}

func (r *fakeSessionReader) DeleteSession(context.Context, uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	if r.operations != nil {
		*r.operations = append(*r.operations, "ch-delete")
	}
	return nil
}
