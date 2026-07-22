// Package telemetry installs optional OpenTelemetry SDK providers for the
// proxy-sampler process. Exporter transport is configured exclusively through
// the standard OTEL_* environment variables.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

const (
	serviceName     = "proxy-sampler"
	shutdownTimeout = 5 * time.Second
	metricInterval  = 10 * time.Second
	telemetryScope  = "github.com/timo972/proxy-sampler"
)

var (
	newMetricExporter = func(ctx context.Context) (sdkmetric.Exporter, error) {
		return otlpmetrichttp.New(ctx)
	}
	newTraceExporter = func(ctx context.Context) (sdktrace.SpanExporter, error) {
		return otlptracehttp.New(ctx)
	}
	newLogExporter = func(ctx context.Context) (sdklog.Exporter, error) {
		return otlploghttp.New(ctx)
	}
)

// Init installs OTLP/HTTP trace, metric, and log providers. With no common
// OTLP endpoint it leaves all global providers untouched and returns a no-op.
func Init(ctx context.Context, getenv func(string) string, logger *slog.Logger) (func(context.Context) error, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	res, err := buildResource(ctx)
	if err != nil {
		return nil, fmt.Errorf("build telemetry resource: %w", err)
	}
	metricExporter, err := newMetricExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("create metric exporter: %w", err)
	}
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(metricInterval))),
	)

	traceExporter, err := newTraceExporter(ctx)
	if err != nil {
		_ = shutdownProvider(func(shutdownCtx context.Context) error { return meterProvider.Shutdown(shutdownCtx) })
		return nil, fmt.Errorf("create trace exporter: %w", err)
	}
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExporter),
	)

	logExporter, err := newLogExporter(ctx)
	if err != nil {
		_ = shutdownProvider(func(shutdownCtx context.Context) error { return tracerProvider.Shutdown(shutdownCtx) })
		_ = shutdownProvider(func(shutdownCtx context.Context) error { return meterProvider.Shutdown(shutdownCtx) })
		return nil, fmt.Errorf("create log exporter: %w", err)
	}
	loggerProvider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
	)

	otel.SetMeterProvider(meterProvider)
	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	global.SetLoggerProvider(loggerProvider)
	logger.Info("telemetry initialized", "service", serviceName)

	var once sync.Once
	var shutdownErr error
	return func(parent context.Context) error {
		once.Do(func() {
			shutdownCtx, cancel := boundedShutdownContext(parent)
			defer cancel()
			shutdownErr = errors.Join(
				wrapShutdown("trace", tracerProvider.Shutdown(shutdownCtx)),
				wrapShutdown("metric", meterProvider.Shutdown(shutdownCtx)),
				wrapShutdown("log", loggerProvider.Shutdown(shutdownCtx)),
			)
		})
		return shutdownErr
	}, nil
}

func buildResource(ctx context.Context) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithHost(),
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
}

func shutdownProvider(shutdown func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return shutdown(ctx)
}

func boundedShutdownContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, shutdownTimeout)
}

func wrapShutdown(name string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s telemetry shutdown: %w", name, err)
}

// SlogHandler bridges structured logs into the installed OTel log provider.
func SlogHandler() slog.Handler {
	return otelslog.NewHandler(telemetryScope)
}

// MultiHandler fans one record out to all configured handlers.
func MultiHandler(handlers ...slog.Handler) slog.Handler {
	return &multiHandler{handlers: append([]slog.Handler(nil), handlers...)}
}

type multiHandler struct{ handlers []slog.Handler }

func (h *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler != nil && handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *multiHandler) Handle(ctx context.Context, record slog.Record) error {
	var errs []error
	for _, handler := range h.handlers {
		if handler == nil || !handler.Enabled(ctx, record.Level) {
			continue
		}
		if err := handler.Handle(ctx, record.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (h *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		if handler != nil {
			handlers = append(handlers, handler.WithAttrs(attrs))
		}
	}
	return &multiHandler{handlers: handlers}
}

func (h *multiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		if handler != nil {
			handlers = append(handlers, handler.WithGroup(name))
		}
	}
	return &multiHandler{handlers: handlers}
}
