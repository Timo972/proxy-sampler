package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/semaphore"

	"github.com/timo972/proxy-sampler/internal/api"
	"github.com/timo972/proxy-sampler/internal/ch"
	"github.com/timo972/proxy-sampler/internal/chmigrate"
	"github.com/timo972/proxy-sampler/internal/config"
	cryptox "github.com/timo972/proxy-sampler/internal/crypto"
	"github.com/timo972/proxy-sampler/internal/db"
	"github.com/timo972/proxy-sampler/internal/enrich"
	"github.com/timo972/proxy-sampler/internal/migrate"
	"github.com/timo972/proxy-sampler/internal/sampler"
	"github.com/timo972/proxy-sampler/internal/session"
	apptelemetry "github.com/timo972/proxy-sampler/internal/telemetry"
	webassets "github.com/timo972/proxy-sampler/web"
)

const (
	httpShutdownTimeout = 10 * time.Second
	writerCloseTimeout  = 15 * time.Second
)

type postgresResource struct {
	pool  *pgxpool.Pool
	ping  func(context.Context) error
	close func()
}

type coreResource struct {
	store  *db.Store
	cipher *cryptox.Cipher
	lookup sampler.ReputationLookup
}

type writerResource struct {
	sink  sampler.EventSink
	close func(context.Context) error
}

type clickhouseReader interface {
	api.Reader
	DeleteSession(context.Context, uuid.UUID) error
	DeleteSessions(context.Context, []uuid.UUID) error
	Close() error
}

type readerResource struct {
	reader clickhouseReader
	close  func() error
}

type supervisorLifecycle interface {
	api.Control
	Resume(context.Context) error
	Wait()
}

type application struct {
	supervisor supervisorLifecycle
	handler    http.Handler
}

type dependencies struct {
	logger             *slog.Logger
	initTelemetry      func(context.Context, func(string) string, *slog.Logger) (func(context.Context) error, error)
	parseClickHouseDSN func(string) (*clickhouse.Options, error)
	migratePostgres    func(context.Context, string, *slog.Logger) error
	migrateClickHouse  func(context.Context, *clickhouse.Options, *slog.Logger) error
	openPostgres       func(context.Context, string) (postgresResource, error)
	buildCore          func(config.Config, postgresResource, *slog.Logger) (coreResource, error)
	openWriter         func(context.Context, *clickhouse.Options, *slog.Logger) (writerResource, error)
	openReader         func(context.Context, *clickhouse.Options) (readerResource, error)
	buildApplication   func(context.Context, config.Config, coreResource, writerResource, readerResource, *slog.Logger) (application, error)
	serve              func(*http.Server) error
	shutdownHTTP       func(context.Context, *http.Server) error
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Getenv, productionDependencies(logger)); err != nil {
		logger.Error("application stopped with an error", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string, deps dependencies) (returnErr error) {
	cfg, err := config.Load(getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger := deps.logger
	if logger == nil {
		logger = slog.Default()
	}

	telemetryShutdown, err := deps.initTelemetry(ctx, getenv, logger)
	if err != nil {
		return startupFailure("initialize telemetry", err)
	}
	if telemetryShutdown == nil {
		telemetryShutdown = func(context.Context) error { return nil }
	}
	if getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		logger = slog.New(apptelemetry.MultiHandler(logger.Handler(), apptelemetry.SlogHandler()))
	}
	previousLogger := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(previousLogger)

	workerRoot, cancelWorkers := context.WithCancel(context.Background())
	var postgres postgresResource
	var writer writerResource
	var reader readerResource
	var app application
	appBuilt := false
	defer func() {
		cancelWorkers()
		var cleanupErrors []error
		if appBuilt && app.supervisor != nil {
			app.supervisor.Wait()
		}
		if writer.close != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), writerCloseTimeout)
			cleanupErrors = append(cleanupErrors, startupFailure("close ClickHouse writer", writer.close(closeCtx)))
			cancel()
		}
		if reader.close != nil {
			cleanupErrors = append(cleanupErrors, startupFailure("close ClickHouse reader", reader.close()))
		}
		if postgres.close != nil {
			postgres.close()
		}
		cleanupErrors = append(cleanupErrors, startupFailure("shutdown telemetry", telemetryShutdown(context.Background())))
		returnErr = errors.Join(returnErr, errors.Join(cleanupErrors...))
	}()

	clickhouseOptions, err := deps.parseClickHouseDSN(cfg.ClickHouseDSN)
	if err != nil {
		return startupFailure("parse CLICKHOUSE_DSN", err)
	}
	if cfg.DBAutoMigrate {
		if err := deps.migratePostgres(ctx, cfg.DatabaseURL, logger); err != nil {
			return startupFailure("run Postgres migrations", err)
		}
		if err := deps.migrateClickHouse(ctx, clickhouseOptions, logger); err != nil {
			return startupFailure("run ClickHouse migrations", err)
		}
	}

	postgres, err = deps.openPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return startupFailure("open Postgres", err)
	}
	if postgres.ping == nil {
		return errors.New("ping Postgres failed")
	}
	if err := postgres.ping(ctx); err != nil {
		return startupFailure("ping Postgres", err)
	}
	core, err := deps.buildCore(cfg, postgres, logger)
	if err != nil {
		return startupFailure("build application core", err)
	}
	writer, err = deps.openWriter(ctx, clickhouseOptions, logger)
	if err != nil {
		return startupFailure("open and ping ClickHouse writer", err)
	}
	reader, err = deps.openReader(ctx, clickhouseOptions)
	if err != nil {
		return startupFailure("open and ping ClickHouse reader", err)
	}
	app, err = deps.buildApplication(workerRoot, cfg, core, writer, reader, logger)
	if err != nil {
		return startupFailure("build HTTP application", err)
	}
	appBuilt = true
	if app.supervisor == nil || app.handler == nil {
		return errors.New("build HTTP application failed")
	}
	if err := app.supervisor.Resume(ctx); err != nil {
		return startupFailure("resume running sessions", err)
	}

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           app.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- deps.serve(server) }()

	select {
	case serveErr := <-serveErrors:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
		defer cancel()
		shutdownErr := deps.shutdownHTTP(shutdownCtx, server)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(wrapHTTPError("serve HTTP", serveErr), wrapHTTPError("shutdown HTTP", shutdownErr))
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
		defer cancel()
		shutdownErr := deps.shutdownHTTP(shutdownCtx, server)
		var serveErr error
		select {
		case serveErr = <-serveErrors:
			if errors.Is(serveErr, http.ErrServerClosed) {
				serveErr = nil
			}
		case <-shutdownCtx.Done():
			serveErr = shutdownCtx.Err()
		}
		return errors.Join(wrapHTTPError("shutdown HTTP", shutdownErr), wrapHTTPError("wait for HTTP server", serveErr))
	}
}

func productionDependencies(logger *slog.Logger) dependencies {
	return dependencies{
		logger:             logger,
		initTelemetry:      apptelemetry.Init,
		parseClickHouseDSN: clickhouse.ParseDSN,
		migratePostgres:    migrate.Up,
		migrateClickHouse:  chmigrate.Up,
		openPostgres: func(ctx context.Context, databaseURL string) (postgresResource, error) {
			pool, err := pgxpool.New(ctx, databaseURL)
			if err != nil {
				return postgresResource{}, err
			}
			return postgresResource{pool: pool, ping: pool.Ping, close: pool.Close}, nil
		},
		buildCore: func(cfg config.Config, postgres postgresResource, logger *slog.Logger) (coreResource, error) {
			if postgres.pool == nil {
				return coreResource{}, errors.New("Postgres pool is unavailable")
			}
			store := db.NewStore(postgres.pool)
			cipher, err := cryptox.New(cfg.EncryptionKey)
			if err != nil {
				return coreResource{}, err
			}
			lookup, err := enrich.NewService(
				store,
				enrich.DefaultProviders(nil, nil),
				cfg.ReputationTTL,
				semaphore.NewWeighted(int64(cfg.EnrichConcurrency)),
			)
			if err != nil {
				return coreResource{}, err
			}
			return coreResource{store: store, cipher: cipher, lookup: lookup}, nil
		},
		openWriter: func(ctx context.Context, options *clickhouse.Options, logger *slog.Logger) (writerResource, error) {
			writer, err := ch.NewWriter(ctx, options, logger)
			if err != nil {
				return writerResource{}, err
			}
			return writerResource{sink: writer, close: writer.Close}, nil
		},
		openReader: func(ctx context.Context, options *clickhouse.Options) (readerResource, error) {
			reader, err := ch.NewReader(ctx, options)
			if err != nil {
				return readerResource{}, err
			}
			return readerResource{reader: reader, close: reader.Close}, nil
		},
		buildApplication: func(root context.Context, cfg config.Config, core coreResource, writer writerResource, reader readerResource, logger *slog.Logger) (application, error) {
			if core.store == nil || core.cipher == nil || core.lookup == nil || writer.sink == nil || reader.reader == nil {
				return application{}, errors.New("application dependencies are incomplete")
			}
			supervisor := sampler.NewSupervisor(root, core.store, writer.sink, reader.reader, func(session.Session) *sampler.Worker {
				return sampler.NewWorker(core.store, core.cipher, sampler.HTTPProber{}, core.lookup, writer.sink, logger)
			})
			server := api.NewServer(core.store, supervisor, core.cipher, api.Defaults{
				ProbeTarget: cfg.ProbeTargetDefault,
				DialTimeout: 10 * time.Second,
			}, core.store, cfg.MaxVariantsPerRun, reader.reader)
			handler, err := api.NewRouter(server.Handler(), webassets.FS, logger)
			if err != nil {
				return application{}, err
			}
			return application{supervisor: supervisor, handler: handler}, nil
		},
		serve:        func(server *http.Server) error { return server.ListenAndServe() },
		shutdownHTTP: func(ctx context.Context, server *http.Server) error { return server.Shutdown(ctx) },
	}
}

type startupStageError struct {
	stage string
	err   error
}

func (e *startupStageError) Error() string { return e.stage + " failed" }
func (e *startupStageError) Unwrap() error { return e.err }

func startupFailure(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &startupStageError{stage: stage, err: err}
}

func wrapHTTPError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
