package sampler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/session"
)

type sessionReader interface {
	DeleteSession(context.Context, uuid.UUID) error
}

// Supervisor owns at most one prepared worker goroutine for each running row.
type Supervisor struct {
	store         session.Store
	sink          EventSink
	reader        sessionReader
	workerFactory func(session.Session) *Worker

	mu         sync.Mutex
	workers    map[uuid.UUID]*runningWorker
	operations map[uuid.UUID]*sessionOperation
	closing    bool
	root       context.Context
	wg         sync.WaitGroup
	now        func() time.Time
}

type runningWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type sessionOperation struct{ done chan struct{} }

var errSupervisorClosed = errors.New("sampling supervisor is closed")

// NewSupervisor constructs a race-safe owner for durable sampling workers.
func NewSupervisor(root context.Context, store session.Store, sink EventSink, reader sessionReader, workerFactory func(session.Session) *Worker) *Supervisor {
	if root == nil {
		root = context.Background()
	}
	return &Supervisor{
		store: store, sink: sink, reader: reader, workerFactory: workerFactory,
		workers: make(map[uuid.UUID]*runningWorker), operations: make(map[uuid.UUID]*sessionOperation),
		root: root, now: time.Now,
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
		if err := s.withSessionOperation(ctx, value.ID, func() error { return s.start(value, ctx) }); err != nil {
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
		s.mu.Lock()
		_, exists := s.workers[id]
		s.mu.Unlock()
		if exists {
			return nil
		}
		value, err := s.store.SessionByID(ctx, id)
		if err != nil {
			return err
		}
		return s.start(value, ctx)
	})
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
	runCtx, cancel := context.WithCancel(s.root)
	running := &runningWorker{cancel: cancel, done: make(chan struct{})}
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
	if _, exists := s.workers[value.ID]; exists {
		s.mu.Unlock()
		cancel()
		return nil
	}
	s.workers[value.ID] = running
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer close(running.done)
		prepared.Run(runCtx)
		s.mu.Lock()
		if s.workers[value.ID] == running {
			delete(s.workers, value.ID)
		}
		s.mu.Unlock()
	}()
	return nil
}

// Stop cancels an owned worker, waits for it without holding the map mutex,
// and only then marks the durable row stopped.
func (s *Supervisor) Stop(ctx context.Context, id uuid.UUID) error {
	return s.withSessionOperation(ctx, id, func() error { return s.stop(ctx, id) })
}

func (s *Supervisor) stop(ctx context.Context, id uuid.UUID) error {
	s.mu.Lock()
	running := s.workers[id]
	s.mu.Unlock()
	if running != nil {
		running.cancel()
		select {
		case <-running.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		value, err := s.store.SessionByID(ctx, id)
		if err != nil {
			return err
		}
		if value.Status != session.StatusRunning {
			return session.ErrNotRunning
		}
	}
	return s.store.Stop(ctx, id, s.now())
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
			if err := s.stop(ctx, id); err != nil && !errors.Is(err, session.ErrNotRunning) {
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

// Wait blocks until every worker published by this supervisor has exited.
func (s *Supervisor) Wait() {
	s.mu.Lock()
	s.closing = true
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
