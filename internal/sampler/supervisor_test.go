package sampler

import (
	"context"
	"errors"
	"strings"
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

func TestSupervisorStopCompletesAfterRequestCanceledWhileWaitingForWorker(t *testing.T) {
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	store := newFakeSessionStore()
	store.rejectCanceledStop = true
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	prober := &cancelGateProber{started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	sink := &fakeEventSink{}
	supervisor := NewSupervisor(root, store, sink, &fakeSessionReader{}, func(session.Session) *Worker {
		return testWorker(store, cipher, prober, &fakeLookup{}, sink)
	})
	if err := supervisor.Start(context.Background(), value.ID); err != nil {
		t.Fatal(err)
	}
	<-prober.started

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- supervisor.Stop(requestCtx, value.ID) }()
	<-prober.canceled
	cancelRequest()
	close(prober.release)
	if err := <-result; err != nil {
		t.Fatalf("Stop after accepted request cancellation = %v, want durable completion", err)
	}
	stored, err := store.SessionByID(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != session.StatusStopped || store.stopCount.Load() != 1 {
		t.Fatalf("durable status/count = %q/%d, want stopped/1", stored.Status, store.stopCount.Load())
	}
}

func TestSupervisorStopUsesIndependentContextAtStoreBoundary(t *testing.T) {
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	store := newFakeSessionStore()
	store.rejectCanceledStop = true
	store.stopStarted, store.stopRelease = make(chan struct{}), make(chan struct{})
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	started := make(chan struct{}, 1)
	sink := &fakeEventSink{}
	supervisor := NewSupervisor(root, store, sink, &fakeSessionReader{}, func(session.Session) *Worker {
		return testWorker(store, cipher, blockingProber{started: started}, &fakeLookup{}, sink)
	})
	if err := supervisor.Start(context.Background(), value.ID); err != nil {
		t.Fatal(err)
	}
	<-started

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- supervisor.Stop(requestCtx, value.ID) }()
	<-store.stopStarted
	cancelRequest()
	close(store.stopRelease)
	if err := <-result; err != nil {
		t.Fatalf("Stop with canceled request at store boundary = %v, want durable completion", err)
	}
	stored, err := store.SessionByID(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != session.StatusStopped {
		t.Fatalf("durable status = %q, want stopped", stored.Status)
	}
}

func TestSupervisorStopRetriesTransientStoreFailure(t *testing.T) {
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	store := newFakeSessionStore()
	store.stopErrs = []error{errors.New("postgres temporarily unavailable"), nil}
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	sink := &fakeEventSink{}
	supervisor := NewSupervisor(root, store, sink, &fakeSessionReader{}, func(session.Session) *Worker {
		return testWorker(store, cipher, blockingProber{}, &fakeLookup{}, sink)
	})
	if err := supervisor.Start(context.Background(), value.ID); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Stop(context.Background(), value.ID); err != nil {
		t.Fatalf("Stop after transient store failure = %v, want retry success", err)
	}
	if got := store.stopAttempts.Load(); got != 2 || store.stopCount.Load() != 1 {
		t.Fatalf("Stop attempts/successes = %d/%d, want 2/1", got, store.stopCount.Load())
	}
	supervisor.mu.Lock()
	workers := len(supervisor.workers)
	supervisor.mu.Unlock()
	if workers != 0 {
		t.Fatalf("workers after durable Stop = %d, want 0", workers)
	}
}

func TestSupervisorStopPersistentStoreFailureRestoresWorkerOwnership(t *testing.T) {
	root, cancelRoot := context.WithCancel(context.Background())
	store := newFakeSessionStore()
	store.stopErr = errors.New("postgres unavailable")
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	started := make(chan struct{}, 2)
	sink := &fakeEventSink{}
	supervisor := NewSupervisor(root, store, sink, &fakeSessionReader{}, func(session.Session) *Worker {
		return testWorker(store, cipher, blockingProber{started: started}, &fakeLookup{}, sink)
	})
	supervisor.stopTimeout = 20 * time.Millisecond
	if err := supervisor.Start(context.Background(), value.ID); err != nil {
		t.Fatal(err)
	}
	<-started
	store.sessionErr = errors.New("postgres reads unavailable")

	err := supervisor.Stop(context.Background(), value.ID)
	if err == nil || !strings.Contains(err.Error(), "postgres unavailable") {
		t.Fatalf("Stop persistent store failure = %v, want bounded persistence error", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("persistent Stop failure left the durable running row without a replacement worker")
	}
	store.mu.Lock()
	stored := store.sessions[value.ID]
	store.mu.Unlock()
	supervisor.mu.Lock()
	workers := len(supervisor.workers)
	supervisor.mu.Unlock()
	if stored.Status != session.StatusRunning || workers != 1 {
		t.Fatalf("durable status/workers = %q/%d, want running/1", stored.Status, workers)
	}

	cancelRoot()
	supervisor.Wait()
	if store.stopCount.Load() != 0 || store.finishCount.Load() != 0 {
		t.Fatal("root cancellation changed durable status after reownership")
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

func TestSupervisorDeleteStopFailureRestoresWorkerOwnershipBeforeReturning(t *testing.T) {
	root, cancelRoot := context.WithCancel(context.Background())
	store := newFakeSessionStore()
	store.stopErr = errors.New("postgres unavailable")
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	started := make(chan struct{}, 2)
	sink := &fakeEventSink{operations: &store.operations}
	supervisor := NewSupervisor(root, store, sink, &fakeSessionReader{operations: &store.operations}, func(session.Session) *Worker {
		return testWorker(store, cipher, blockingProber{started: started}, &fakeLookup{}, sink)
	})
	supervisor.stopTimeout = 20 * time.Millisecond
	if err := supervisor.Start(context.Background(), value.ID); err != nil {
		t.Fatal(err)
	}
	<-started

	deleteCtx, cancelDelete := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- supervisor.Delete(deleteCtx, value.ID) }()
	var err error
	select {
	case err = <-result:
	case <-time.After(100 * time.Millisecond):
		cancelDelete()
		<-result
		t.Fatal("Delete stop phase ignored the supervisor's bounded transition timeout")
	}
	cancelDelete()
	if err == nil || !strings.Contains(err.Error(), "postgres unavailable") {
		t.Fatalf("Delete with persistent Stop failure = %v, want persistence error", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("failed Delete left the durable running row without a replacement worker")
	}
	if len(store.operations) != 0 {
		t.Fatalf("Delete continued after failed Stop: %v", store.operations)
	}
	stored, getErr := store.SessionByID(context.Background(), value.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.Status != session.StatusRunning {
		t.Fatalf("durable status = %q, want running", stored.Status)
	}

	cancelRoot()
	supervisor.Wait()
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

func TestSupervisorResumeSkipsSessionStoppedAfterRunningSnapshot(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newFakeSessionStore()
	store.runningStarted, store.runningRelease = make(chan struct{}), make(chan struct{})
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	sink := &fakeEventSink{}
	supervisor := NewSupervisor(root, store, sink, &fakeSessionReader{}, func(session.Session) *Worker {
		return testWorker(store, cipher, blockingProber{}, &fakeLookup{}, sink)
	})
	resumeResult := make(chan error, 1)
	go func() { resumeResult <- supervisor.Resume(context.Background()) }()
	<-store.runningStarted
	if err := supervisor.Stop(context.Background(), value.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	close(store.runningRelease)
	if err := <-resumeResult; err != nil {
		t.Fatalf("Resume: %v", err)
	}
	supervisor.mu.Lock()
	count := len(supervisor.workers)
	supervisor.mu.Unlock()
	if count != 0 {
		t.Fatalf("workers after concurrent Stop = %d, want 0", count)
	}
}

func TestSupervisorResumeSkipsSessionDeletedAfterRunningSnapshot(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newFakeSessionStore()
	store.runningStarted, store.runningRelease = make(chan struct{}), make(chan struct{})
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	sink := &fakeEventSink{}
	supervisor := NewSupervisor(root, store, sink, &fakeSessionReader{}, func(session.Session) *Worker {
		return testWorker(store, cipher, blockingProber{}, &fakeLookup{}, sink)
	})
	resumeResult := make(chan error, 1)
	go func() { resumeResult <- supervisor.Resume(context.Background()) }()
	<-store.runningStarted
	if err := supervisor.Delete(context.Background(), value.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	close(store.runningRelease)
	if err := <-resumeResult; err != nil {
		t.Fatalf("Resume: %v", err)
	}
	supervisor.mu.Lock()
	count := len(supervisor.workers)
	supervisor.mu.Unlock()
	if count != 0 {
		t.Fatalf("workers after concurrent Delete = %d, want 0", count)
	}
}

func TestSupervisorResumePropagatesPrepareErrNotFound(t *testing.T) {
	store := newFakeSessionStore()
	value, cipher := encryptedSession(t, 1)
	store.sessions[value.ID] = value
	store.ipsErr = session.ErrNotFound
	sink := &fakeEventSink{}
	supervisor := NewSupervisor(context.Background(), store, sink, &fakeSessionReader{}, func(session.Session) *Worker {
		return testWorker(store, cipher, fixedProber{}, &fakeLookup{}, sink)
	})
	if err := supervisor.Resume(context.Background()); err == nil {
		t.Fatal("Resume prepare error = nil, want propagated SessionIPs failure")
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

type cancelGateProber struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (p *cancelGateProber) Probe(ctx context.Context, _ proxy.ContextDialer, _ string, _ time.Duration) ProbeResult {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	close(p.canceled)
	<-p.release
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
