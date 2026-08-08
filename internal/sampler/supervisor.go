package sampler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/session"
)

type sessionReader interface {
	DeleteSession(context.Context, uuid.UUID) error
	DeleteSessions(context.Context, []uuid.UUID) error
}

const (
	stopTransitionTimeout = 5 * time.Second
	stopTransitionRetry   = 100 * time.Millisecond
)

// Supervisor owns at most one prepared worker goroutine for each running row.
type Supervisor struct {
	store         session.Store
	sink          EventSink
	reader        sessionReader
	workerFactory func(session.Session) *Worker

	mu             sync.Mutex
	workers        map[uuid.UUID]*runningWorker
	operations     map[uuid.UUID]*sessionOperation
	closing        bool
	root           context.Context
	recoveryCtx    context.Context
	cancelRecovery context.CancelFunc
	wg             sync.WaitGroup
	now            func() time.Time
	stopTimeout    time.Duration
	retryAfter     func(time.Duration) <-chan time.Time
}

type runningWorker struct {
	cancel            context.CancelFunc
	done              chan struct{}
	prepared          *PreparedWorker
	recoveryScheduled bool
}

type sessionOperation struct{ done chan struct{} }

var errSupervisorClosed = errors.New("sampling supervisor is closed")

// NewSupervisor constructs a race-safe owner for durable sampling workers.
func NewSupervisor(root context.Context, store session.Store, sink EventSink, reader sessionReader, workerFactory func(session.Session) *Worker) *Supervisor {
	if root == nil {
		root = context.Background()
	}
	recoveryCtx, cancelRecovery := context.WithCancel(root)
	return &Supervisor{
		store: store, sink: sink, reader: reader, workerFactory: workerFactory,
		workers: make(map[uuid.UUID]*runningWorker), operations: make(map[uuid.UUID]*sessionOperation),
		root: root, recoveryCtx: recoveryCtx, cancelRecovery: cancelRecovery,
		now: time.Now, stopTimeout: stopTransitionTimeout, retryAfter: time.After,
	}
}

// Resume synchronously prepares and starts every persisted running session.
func (s *Supervisor) Resume(ctx context.Context) error {
	values, err := s.store.RunningSessions(ctx)
	if err != nil {
		return err
	}
	for _, value := range values {
		if value.Status != session.StatusRunning {
			continue
		}
		var stale bool
		err := s.withSessionOperation(ctx, value.ID, func() error {
			var err error
			stale, err = s.startCurrent(ctx, value.ID)
			return err
		})
		if stale && (errors.Is(err, session.ErrNotRunning) || errors.Is(err, session.ErrNotFound)) {
			continue
		} else if err != nil {
			return fmt.Errorf("resume session %s: %w", value.ID, err)
		}
	}
	return nil
}

// Start prepares and owns a running session. Repeated starts are idempotent.
func (s *Supervisor) Start(ctx context.Context, id uuid.UUID) error {
	if err := s.root.Err(); err != nil {
		return err
	}
	return s.withSessionOperation(ctx, id, func() error {
		_, err := s.startCurrent(ctx, id)
		return err
	})
}

// Reenable resets a stopped or finished session's per-run counters and starts
// a fresh worker. Prior samples and IP inventory are preserved.
func (s *Supervisor) Reenable(ctx context.Context, id uuid.UUID) error {
	if err := s.root.Err(); err != nil {
		return err
	}
	if err := s.store.Reenable(ctx, id, s.now().UTC()); err != nil {
		return err
	}
	return s.Start(ctx, id)
}

// startCurrent reports stale when the control row itself was deleted or is no
// longer running. Errors from preparation are never classified as stale.
func (s *Supervisor) startCurrent(ctx context.Context, id uuid.UUID) (stale bool, err error) {
	s.mu.Lock()
	_, exists := s.workers[id]
	s.mu.Unlock()
	if exists {
		return false, nil
	}
	value, err := s.store.SessionByID(ctx, id)
	if err != nil {
		return errors.Is(err, session.ErrNotFound) || errors.Is(err, session.ErrNotRunning), err
	}
	if value.Status != session.StatusRunning {
		return true, session.ErrNotRunning
	}
	return false, s.start(value, ctx)
}

func (s *Supervisor) start(value session.Session, ctx context.Context) error {
	if value.Status != session.StatusRunning {
		return session.ErrNotRunning
	}
	s.mu.Lock()
	_, exists := s.workers[value.ID]
	s.mu.Unlock()
	if exists {
		return nil
	}
	if s.workerFactory == nil {
		return errors.New("worker factory is required")
	}
	worker := s.workerFactory(value)
	if worker == nil {
		return errors.New("worker factory returned nil")
	}
	prepared, err := worker.Prepare(ctx, value)
	if err != nil {
		return err
	}
	return s.publishPrepared(value.ID, prepared)
}

func (s *Supervisor) publishPrepared(id uuid.UUID, prepared *PreparedWorker) error {
	runCtx, cancel := context.WithCancel(s.root)
	running := &runningWorker{cancel: cancel, done: make(chan struct{}), prepared: prepared}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		cancel()
		return errSupervisorClosed
	}
	if err := s.root.Err(); err != nil {
		s.mu.Unlock()
		cancel()
		return err
	}
	if _, exists := s.workers[id]; exists {
		s.mu.Unlock()
		cancel()
		return nil
	}
	s.workers[id] = running
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer close(running.done)
		prepared.Run(runCtx)
		s.mu.Lock()
		if s.workers[id] == running {
			delete(s.workers, id)
		}
		s.mu.Unlock()
	}()
	return nil
}

// Stop cancels an owned worker, waits for it without holding the map mutex,
// and only then marks the durable row stopped.
func (s *Supervisor) Stop(ctx context.Context, id uuid.UUID) error {
	return s.withSessionOperation(ctx, id, func() error {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.stopTimeout)
		defer cancel()
		return s.stopWithOwnership(stopCtx, id)
	})
}

func (s *Supervisor) stopWithOwnership(ctx context.Context, id uuid.UUID) error {
	prepared, delayed, err := s.stop(ctx, id)
	if err == nil || errors.Is(err, session.ErrNotRunning) || errors.Is(err, session.ErrNotFound) {
		return err
	}
	if delayed != nil {
		if recoveryErr := s.scheduleRecovery(id, delayed); recoveryErr != nil {
			return errors.Join(err, fmt.Errorf("schedule worker recovery: %w", recoveryErr))
		}
		return err
	}
	restoreCtx, cancelRestore := context.WithTimeout(context.Background(), s.stopTimeout)
	defer cancelRestore()
	var stale bool
	var restoreErr error
	if prepared != nil {
		restoreErr = s.publishPrepared(id, prepared)
	} else {
		stale, restoreErr = s.startCurrent(restoreCtx, id)
	}
	if stale && (errors.Is(restoreErr, session.ErrNotRunning) || errors.Is(restoreErr, session.ErrNotFound)) {
		return err
	}
	if restoreErr != nil {
		return errors.Join(err, fmt.Errorf("restore worker ownership after failed stop: %w", restoreErr))
	}
	return err
}

func (s *Supervisor) stop(ctx context.Context, id uuid.UUID) (*PreparedWorker, *runningWorker, error) {
	s.mu.Lock()
	running := s.workers[id]
	s.mu.Unlock()
	var prepared *PreparedWorker
	if running != nil {
		prepared = running.prepared
		running.cancel()
		select {
		case <-running.done:
		case <-ctx.Done():
			select {
			case <-running.done:
			default:
				return prepared, running, ctx.Err()
			}
		}
	} else {
		value, err := s.store.SessionByID(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		if value.Status != session.StatusRunning {
			return nil, nil, session.ErrNotRunning
		}
	}
	for {
		err := s.store.Stop(ctx, id, s.now())
		if err == nil || errors.Is(err, session.ErrNotRunning) {
			return prepared, nil, err
		}
		select {
		case <-ctx.Done():
			return prepared, nil, errors.Join(err, ctx.Err())
		case <-s.retryAfter(stopTransitionRetry):
		}
	}
}

func (s *Supervisor) scheduleRecovery(id uuid.UUID, running *runningWorker) error {
	s.mu.Lock()
	if running.recoveryScheduled {
		s.mu.Unlock()
		return nil
	}
	if s.closing {
		s.mu.Unlock()
		return errSupervisorClosed
	}
	if err := s.recoveryCtx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	running.recoveryScheduled = true
	s.wg.Add(1)
	s.mu.Unlock()

	go s.recoverAfterExit(id, running)
	return nil
}

func (s *Supervisor) recoverAfterExit(id uuid.UUID, running *runningWorker) {
	defer s.wg.Done()
	select {
	case <-running.done:
	case <-s.recoveryCtx.Done():
		return
	}

	for {
		attemptCtx, cancelAttempt := context.WithTimeout(s.recoveryCtx, s.stopTimeout)
		var stale bool
		err := s.withSessionOperation(attemptCtx, id, func() error {
			var err error
			stale, err = s.startCurrent(attemptCtx, id)
			return err
		})
		cancelAttempt()
		if err == nil || (stale && (errors.Is(err, session.ErrNotRunning) || errors.Is(err, session.ErrNotFound))) {
			return
		}
		if errors.Is(err, errSupervisorClosed) || s.recoveryCtx.Err() != nil {
			return
		}
		select {
		case <-s.recoveryCtx.Done():
			return
		case <-s.retryAfter(stopTransitionRetry):
		}
	}
}

// Delete stops production, crosses the queue barrier, synchronously removes
// ClickHouse samples, and deletes the Postgres control row last.
func (s *Supervisor) Delete(ctx context.Context, id uuid.UUID) error {
	return s.withSessionOperation(ctx, id, func() error {
		value, err := s.store.SessionByID(ctx, id)
		if err != nil {
			return err
		}
		if value.Status == session.StatusRunning {
			stopCtx, cancelStop := context.WithTimeout(ctx, s.stopTimeout)
			err := s.stopWithOwnership(stopCtx, id)
			cancelStop()
			if err != nil && !errors.Is(err, session.ErrNotRunning) {
				return err
			}
		}
		if err := s.sink.Flush(ctx); err != nil {
			return fmt.Errorf("flush samples before delete: %w", err)
		}
		if err := s.reader.DeleteSession(ctx, id); err != nil {
			return err
		}
		return s.store.Delete(ctx, id)
	})
}

// DeleteSessions deletes many sessions with the per-session flush and
// ClickHouse mutation performed once for the whole set rather than once per
// session, keeping deletion of a large run within the request's write
// deadline. Every child's session-operation lock is held for the whole
// duration — through the stop, the single flush, the single ClickHouse
// mutation, and the row deletes — so a concurrent Reenable cannot start a new
// worker after the barrier and leave it orphaned by the row deletion. It is
// idempotent: already-stopped or already-deleted sessions are skipped, so a
// retry after a partial failure completes cleanly.
func (s *Supervisor) DeleteSessions(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	// Acquire every lock in a stable (sorted) order so overlapping deletions
	// cannot deadlock. Single-session operations only ever hold one lock, so
	// they always make progress and release for this batch. De-duplicate first:
	// a repeated id would nest withSessionOperation on itself and deadlock
	// against its own in-flight operation.
	ordered := append([]uuid.UUID(nil), ids...)
	sort.Slice(ordered, func(i, j int) bool { return bytes.Compare(ordered[i][:], ordered[j][:]) < 0 })
	deduped := ordered[:0]
	for i, id := range ordered {
		if i == 0 || id != ordered[i-1] {
			deduped = append(deduped, id)
		}
	}
	ordered = deduped

	return s.withOperations(ctx, ordered, func() error {
		for _, id := range ordered {
			value, err := s.store.SessionByID(ctx, id)
			if errors.Is(err, session.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if value.Status != session.StatusRunning {
				continue
			}
			stopCtx, cancelStop := context.WithTimeout(ctx, s.stopTimeout)
			err = s.stopWithOwnership(stopCtx, id)
			cancelStop()
			if err != nil && !errors.Is(err, session.ErrNotRunning) {
				return err
			}
		}
		if err := s.sink.Flush(ctx); err != nil {
			return fmt.Errorf("flush samples before delete: %w", err)
		}
		if err := s.reader.DeleteSessions(ctx, ordered); err != nil {
			return err
		}
		for _, id := range ordered {
			if err := s.store.Delete(ctx, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// withOperations holds every session's operation lock at once for the duration
// of fn by nesting withSessionOperation. ids must be de-duplicated and in a
// stable order (see DeleteSessions) to stay deadlock-free.
func (s *Supervisor) withOperations(ctx context.Context, ids []uuid.UUID, fn func() error) error {
	if len(ids) == 0 {
		return fn()
	}
	return s.withSessionOperation(ctx, ids[0], func() error {
		return s.withOperations(ctx, ids[1:], fn)
	})
}

// Wait blocks until every published worker and deferred recovery has exited.
func (s *Supervisor) Wait() {
	s.mu.Lock()
	s.closing = true
	s.cancelRecovery()
	s.mu.Unlock()
	s.wg.Wait()
}

// withSessionOperation serializes lifecycle transitions for one session. A
// waiter never holds the worker-map mutex while another operation runs.
func (s *Supervisor) withSessionOperation(ctx context.Context, id uuid.UUID, operation func() error) error {
	for {
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			return errSupervisorClosed
		}
		if active := s.operations[id]; active != nil {
			done := active.done
			s.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		active := &sessionOperation{done: make(chan struct{})}
		s.operations[id] = active
		s.mu.Unlock()

		err := operation()
		s.mu.Lock()
		if s.operations[id] == active {
			delete(s.operations, id)
			close(active.done)
		}
		s.mu.Unlock()
		return err
	}
}
