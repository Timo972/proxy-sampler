package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/config"
)

func TestRunRejectsMissingConfigBeforeStartingDependencies(t *testing.T) {
	deps, recorder := successfulDependencies(t)
	err := run(context.Background(), func(string) string { return "" }, deps)
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("err = %v", err)
	}
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("operations = %v", got)
	}
}

func TestRunReportsStartupFailuresAndCleansAcquiredResources(t *testing.T) {
	failure := errors.New("injected startup failure")
	tests := []struct {
		name       string
		fail       func(*dependencies)
		wantPrefix []string
	}{
		{name: "postgres migration", fail: func(deps *dependencies) {
			original := deps.migratePostgres
			deps.migratePostgres = func(ctx context.Context, value string, logger *slog.Logger) error {
				_ = original(ctx, value, logger)
				return failure
			}
		}, wantPrefix: []string{"telemetry_init", "parse_clickhouse", "migrate_postgres", "telemetry_shutdown"}},
		{name: "clickhouse migration", fail: func(deps *dependencies) {
			original := deps.migrateClickHouse
			deps.migrateClickHouse = func(ctx context.Context, options *clickhouse.Options, logger *slog.Logger) error {
				_ = original(ctx, options, logger)
				return failure
			}
		}, wantPrefix: []string{"telemetry_init", "parse_clickhouse", "migrate_postgres", "migrate_clickhouse", "telemetry_shutdown"}},
		{name: "postgres ping", fail: func(deps *dependencies) {
			original := deps.openPostgres
			deps.openPostgres = func(ctx context.Context, dsn string) (postgresResource, error) {
				resource, err := original(ctx, dsn)
				originalPing := resource.ping
				resource.ping = func(ctx context.Context) error {
					_ = originalPing(ctx)
					return failure
				}
				return resource, err
			}
		}, wantPrefix: []string{"telemetry_init", "parse_clickhouse", "migrate_postgres", "migrate_clickhouse", "postgres_open", "postgres_ping", "postgres_close", "telemetry_shutdown"}},
		{name: "clickhouse ping", fail: func(deps *dependencies) {
			original := deps.openWriter
			deps.openWriter = func(ctx context.Context, options *clickhouse.Options, logger *slog.Logger) (writerResource, error) {
				_, _ = original(ctx, options, logger)
				return writerResource{}, failure
			}
		}, wantPrefix: []string{"telemetry_init", "parse_clickhouse", "migrate_postgres", "migrate_clickhouse", "postgres_open", "postgres_ping", "core_build", "writer_open", "postgres_close", "telemetry_shutdown"}},
		{name: "clickhouse reader", fail: func(deps *dependencies) {
			original := deps.openReader
			deps.openReader = func(ctx context.Context, options *clickhouse.Options) (readerResource, error) {
				_, _ = original(ctx, options)
				return readerResource{}, failure
			}
		}, wantPrefix: []string{"telemetry_init", "parse_clickhouse", "migrate_postgres", "migrate_clickhouse", "postgres_open", "postgres_ping", "core_build", "writer_open", "reader_open", "writer_close", "postgres_close", "telemetry_shutdown"}},
		{name: "resume", fail: func(deps *dependencies) {
			original := deps.buildApplication
			deps.buildApplication = func(ctx context.Context, cfg config.Config, core coreResource, writer writerResource, reader readerResource, logger *slog.Logger) (application, error) {
				app, err := original(ctx, cfg, core, writer, reader, logger)
				app.supervisor.(*fakeSupervisor).resumeErr = failure
				return app, err
			}
		}, wantPrefix: []string{"telemetry_init", "parse_clickhouse", "migrate_postgres", "migrate_clickhouse", "postgres_open", "postgres_ping", "core_build", "writer_open", "reader_open", "application_build", "resume", "workers_wait", "writer_close", "reader_close", "postgres_close", "telemetry_shutdown"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deps, recorder := successfulDependencies(t)
			test.fail(&deps)
			err := run(context.Background(), validGetenv(), deps)
			if !errors.Is(err, failure) {
				t.Fatalf("err = %v, want injected failure", err)
			}
			if got := recorder.snapshot(); !reflect.DeepEqual(got, test.wantPrefix) {
				t.Fatalf("operations = %v, want %v", got, test.wantPrefix)
			}
		})
	}
}

func TestRunSkipsMigrationsWhenDisabled(t *testing.T) {
	deps, recorder := successfulDependencies(t)
	getenv := validGetenv()
	disabledGetenv := func(name string) string {
		if name == "DB_AUTO_MIGRATE" {
			return "false"
		}
		return getenv(name)
	}
	_ = run(context.Background(), disabledGetenv, deps)
	if recorder.count("migrate_postgres") != 0 || recorder.count("migrate_clickhouse") != 0 {
		t.Fatalf("operations = %v", recorder.snapshot())
	}
}

func TestRunResumesBeforeServeAndUsesRequiredTimeouts(t *testing.T) {
	deps, recorder := successfulDependencies(t)
	deps.serve = func(server *http.Server) error {
		recorder.add("serve")
		if server.ReadHeaderTimeout != 5*time.Second || server.ReadTimeout != 30*time.Second ||
			server.WriteTimeout != 60*time.Second || server.IdleTimeout != 120*time.Second {
			t.Errorf("server timeouts = %+v", server)
		}
		return errors.New("serve failed")
	}

	err := run(context.Background(), validGetenv(), deps)
	if err == nil || !strings.Contains(err.Error(), "serve failed") {
		t.Fatalf("err = %v", err)
	}
	operations := recorder.snapshot()
	if indexOf(operations, "resume") > indexOf(operations, "serve") {
		t.Fatalf("operations = %v", operations)
	}
}

func TestRunCancellationStopsAdmissionThenWorkersAndClosesInOrder(t *testing.T) {
	deps, recorder := successfulDependencies(t)
	serveStarted := make(chan struct{})
	serverStopped := make(chan struct{})
	deps.serve = func(*http.Server) error {
		recorder.add("serve")
		close(serveStarted)
		<-serverStopped
		return http.ErrServerClosed
	}
	deps.shutdownHTTP = func(ctx context.Context, _ *http.Server) error {
		recorder.add("http_shutdown")
		if _, ok := ctx.Deadline(); !ok {
			t.Error("HTTP shutdown context has no deadline")
		}
		close(serverStopped)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, validGetenv(), deps) }()
	<-serveStarted
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	operations := recorder.snapshot()
	wantTail := []string{"http_shutdown", "workers_wait", "writer_close", "reader_close", "postgres_close", "telemetry_shutdown"}
	if !reflect.DeepEqual(operations[len(operations)-len(wantTail):], wantTail) {
		t.Fatalf("shutdown operations = %v, want tail %v", operations, wantTail)
	}
	if recorder.count("durable_stop") != 0 {
		t.Fatalf("durable stop calls = %d", recorder.count("durable_stop"))
	}
}

func TestRunDoesNotExposeCredentialsFromStartupOrCleanupErrors(t *testing.T) {
	t.Run("startup", func(t *testing.T) {
		deps, _ := successfulDependencies(t)
		failure := errors.New("cannot parse clickhouse://admin:ch-secret@clickhouse/proxy_sampler")
		deps.parseClickHouseDSN = func(string) (*clickhouse.Options, error) { return nil, failure }
		err := run(context.Background(), validGetenv(), deps)
		if !errors.Is(err, failure) {
			t.Fatalf("err = %v", err)
		}
		assertNoSecrets(t, err.Error())
	})

	t.Run("cleanup", func(t *testing.T) {
		deps, _ := successfulDependencies(t)
		failure := errors.New("flush clickhouse://admin:ch-secret@clickhouse/proxy_sampler")
		original := deps.openWriter
		deps.openWriter = func(ctx context.Context, options *clickhouse.Options, logger *slog.Logger) (writerResource, error) {
			resource, err := original(ctx, options, logger)
			resource.close = func(context.Context) error { return failure }
			return resource, err
		}
		err := run(context.Background(), validGetenv(), deps)
		if !errors.Is(err, failure) {
			t.Fatalf("err = %v", err)
		}
		assertNoSecrets(t, err.Error())
	})
}

func successfulDependencies(t *testing.T) (dependencies, *operationRecorder) {
	t.Helper()
	recorder := &operationRecorder{}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	deps := dependencies{logger: logger}
	deps.initTelemetry = func(context.Context, func(string) string, *slog.Logger) (func(context.Context) error, error) {
		recorder.add("telemetry_init")
		return func(context.Context) error { recorder.add("telemetry_shutdown"); return nil }, nil
	}
	deps.parseClickHouseDSN = func(string) (*clickhouse.Options, error) {
		recorder.add("parse_clickhouse")
		return &clickhouse.Options{}, nil
	}
	deps.migratePostgres = func(context.Context, string, *slog.Logger) error { recorder.add("migrate_postgres"); return nil }
	deps.migrateClickHouse = func(context.Context, *clickhouse.Options, *slog.Logger) error {
		recorder.add("migrate_clickhouse")
		return nil
	}
	deps.openPostgres = func(context.Context, string) (postgresResource, error) {
		recorder.add("postgres_open")
		return postgresResource{
			ping:  func(context.Context) error { recorder.add("postgres_ping"); return nil },
			close: func() { recorder.add("postgres_close") },
		}, nil
	}
	deps.buildCore = func(config.Config, postgresResource, *slog.Logger) (coreResource, error) {
		recorder.add("core_build")
		return coreResource{}, nil
	}
	deps.openWriter = func(context.Context, *clickhouse.Options, *slog.Logger) (writerResource, error) {
		recorder.add("writer_open")
		return writerResource{close: func(context.Context) error { recorder.add("writer_close"); return nil }}, nil
	}
	deps.openReader = func(context.Context, *clickhouse.Options) (readerResource, error) {
		recorder.add("reader_open")
		return readerResource{close: func() error { recorder.add("reader_close"); return nil }}, nil
	}
	deps.buildApplication = func(root context.Context, _ config.Config, _ coreResource, _ writerResource, _ readerResource, _ *slog.Logger) (application, error) {
		recorder.add("application_build")
		return application{supervisor: &fakeSupervisor{root: root, recorder: recorder}, handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}, nil
	}
	deps.serve = func(*http.Server) error { recorder.add("serve"); return errors.New("serve failed") }
	deps.shutdownHTTP = func(context.Context, *http.Server) error { recorder.add("http_shutdown"); return nil }
	return deps, recorder
}

func validGetenv() func(string) string {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x2a}, 32))
	values := map[string]string{
		"DATABASE_URL":   "postgres://user:password@db/proxy_sampler",
		"CLICKHOUSE_DSN": "clickhouse://user:password@clickhouse/proxy_sampler",
		"ENCRYPTION_KEY": key,
	}
	return func(name string) string { return values[name] }
}

type fakeSupervisor struct {
	root      context.Context
	recorder  *operationRecorder
	resumeErr error
}

func (s *fakeSupervisor) Resume(context.Context) error           { s.recorder.add("resume"); return s.resumeErr }
func (s *fakeSupervisor) Start(context.Context, uuid.UUID) error { return nil }
func (s *fakeSupervisor) Stop(context.Context, uuid.UUID) error {
	s.recorder.add("durable_stop")
	return nil
}
func (s *fakeSupervisor) Reenable(context.Context, uuid.UUID) error {
	s.recorder.add("reenable")
	return nil
}
func (s *fakeSupervisor) Delete(context.Context, uuid.UUID) error { return nil }
func (s *fakeSupervisor) Wait() {
	if s.root.Err() == nil {
		panic("workers were not canceled before Wait")
	}
	s.recorder.add("workers_wait")
}

type operationRecorder struct {
	mu         sync.Mutex
	operations []string
}

func (r *operationRecorder) add(operation string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.operations = append(r.operations, operation)
}

func (r *operationRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.operations...)
}

func (r *operationRecorder) count(operation string) int {
	count := 0
	for _, candidate := range r.snapshot() {
		if candidate == operation {
			count++
		}
	}
	return count
}

func indexOf(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}

func assertNoSecrets(t *testing.T, value string) {
	t.Helper()
	for _, secret := range []string{"password", "ch-secret", "postgres://user:password", "clickhouse://admin:ch-secret"} {
		if strings.Contains(value, secret) {
			t.Fatalf("error leaked %q: %s", secret, value)
		}
	}
}
