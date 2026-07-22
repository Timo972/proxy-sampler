package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log/global"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestInitWithoutEndpointLeavesGlobalProvidersUntouched(t *testing.T) {
	originalTracer := otel.GetTracerProvider()
	originalMeter := otel.GetMeterProvider()
	tracer := trace.NewNoopTracerProvider()
	meter := metricnoop.NewMeterProvider()
	otel.SetTracerProvider(tracer)
	otel.SetMeterProvider(meter)
	t.Cleanup(func() {
		otel.SetTracerProvider(originalTracer)
		otel.SetMeterProvider(originalMeter)
	})

	shutdown, err := Init(context.Background(), func(string) string { return "" }, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if shutdown == nil {
		t.Fatal("shutdown is nil")
	}
	if otel.GetTracerProvider() != tracer || otel.GetMeterProvider() != meter {
		t.Fatal("disabled telemetry replaced a global provider")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown: %v", err)
	}
}

func TestInitWithEndpointInstallsAllProvidersAndBoundsShutdown(t *testing.T) {
	metricExporter := &fakeMetricExporter{}
	traceExporter := &fakeTraceExporter{}
	logExporter := &fakeLogExporter{}
	restore := replaceExporterFactories(t,
		func(context.Context) (sdkmetric.Exporter, error) { return metricExporter, nil },
		func(context.Context) (sdktrace.SpanExporter, error) { return traceExporter, nil },
		func(context.Context) (sdklog.Exporter, error) { return logExporter, nil },
	)
	defer restore()

	originalTracer := otel.GetTracerProvider()
	originalMeter := otel.GetMeterProvider()
	originalLogger := global.GetLoggerProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(originalTracer)
		otel.SetMeterProvider(originalMeter)
		global.SetLoggerProvider(originalLogger)
	})
	var logs bytes.Buffer
	shutdown, err := Init(context.Background(), func(name string) string {
		if name == "OTEL_EXPORTER_OTLP_ENDPOINT" {
			return "https://user:top-secret@collector.example"
		}
		return ""
	}, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() == originalTracer || otel.GetMeterProvider() == originalMeter {
		t.Fatal("enabled telemetry did not install trace and metric providers")
	}
	if strings.Contains(logs.String(), "top-secret") {
		t.Fatalf("telemetry log leaked endpoint credentials: %s", logs.String())
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	for name, deadline := range map[string]time.Time{
		"metric": metricExporter.deadline(),
		"trace":  traceExporter.deadline(),
		"log":    logExporter.deadline(),
	} {
		remaining := time.Until(deadline)
		if deadline.IsZero() || remaining <= 0 || remaining > 5*time.Second {
			t.Fatalf("%s shutdown deadline = %v (remaining %s)", name, deadline, remaining)
		}
	}
}

func TestInitCleansMetricExporterWhenTraceSetupFails(t *testing.T) {
	metricExporter := &fakeMetricExporter{}
	failure := errors.New("trace exporter failed")
	restore := replaceExporterFactories(t,
		func(context.Context) (sdkmetric.Exporter, error) { return metricExporter, nil },
		func(context.Context) (sdktrace.SpanExporter, error) { return nil, failure },
		func(context.Context) (sdklog.Exporter, error) {
			t.Fatal("log exporter called after trace failure")
			return nil, nil
		},
	)
	defer restore()

	shutdown, err := Init(context.Background(), endpointGetenv, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
	if shutdown != nil {
		t.Fatal("shutdown is non-nil after initialization failure")
	}
	if !errors.Is(err, failure) {
		t.Fatalf("err = %v", err)
	}
	if metricExporter.deadline().IsZero() {
		t.Fatal("metric exporter was not shut down after partial initialization")
	}
}

func TestBuildResourceForcesProxySamplerServiceName(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "wrong-service")
	res, err := buildResource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	value, ok := res.Set().Value(attribute.Key("service.name"))
	if !ok || value.AsString() != "proxy-sampler" {
		t.Fatalf("service.name = %v, %v", value, ok)
	}
}

func endpointGetenv(name string) string {
	if name == "OTEL_EXPORTER_OTLP_ENDPOINT" {
		return "http://collector.example"
	}
	return ""
}

type exporterFactoryRestore func()

func replaceExporterFactories(
	t *testing.T,
	metricFactory func(context.Context) (sdkmetric.Exporter, error),
	traceFactory func(context.Context) (sdktrace.SpanExporter, error),
	logFactory func(context.Context) (sdklog.Exporter, error),
) exporterFactoryRestore {
	t.Helper()
	originalMetric := newMetricExporter
	originalTrace := newTraceExporter
	originalLog := newLogExporter
	newMetricExporter = metricFactory
	newTraceExporter = traceFactory
	newLogExporter = logFactory
	return func() {
		newMetricExporter = originalMetric
		newTraceExporter = originalTrace
		newLogExporter = originalLog
	}
}

type shutdownRecorder struct {
	mu         sync.Mutex
	deadlineAt time.Time
}

func (r *shutdownRecorder) record(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deadlineAt, _ = ctx.Deadline()
}

func (r *shutdownRecorder) deadline() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deadlineAt
}

type fakeMetricExporter struct{ shutdownRecorder }

func (*fakeMetricExporter) Temporality(sdkmetric.InstrumentKind) metricdata.Temporality {
	return metricdata.CumulativeTemporality
}
func (*fakeMetricExporter) Aggregation(kind sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(kind)
}
func (*fakeMetricExporter) Export(context.Context, *metricdata.ResourceMetrics) error { return nil }
func (*fakeMetricExporter) ForceFlush(context.Context) error                          { return nil }
func (e *fakeMetricExporter) Shutdown(ctx context.Context) error                      { e.record(ctx); return nil }

type fakeTraceExporter struct{ shutdownRecorder }

func (*fakeTraceExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (e *fakeTraceExporter) Shutdown(ctx context.Context) error                       { e.record(ctx); return nil }

type fakeLogExporter struct{ shutdownRecorder }

func (*fakeLogExporter) Export(context.Context, []sdklog.Record) error { return nil }
func (*fakeLogExporter) ForceFlush(context.Context) error              { return nil }
func (e *fakeLogExporter) Shutdown(ctx context.Context) error          { e.record(ctx); return nil }
