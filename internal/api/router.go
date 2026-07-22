package api

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const serviceName = "proxy-sampler"

// NewRouter places generated API and health routes ahead of the SPA fallback,
// installs the process HTTP middleware, and returns the final OTel wrapper.
func NewRouter(apiHandler http.Handler, assets fs.FS, logger *slog.Logger) (http.Handler, error) {
	spa, err := newSPAHandler(assets)
	if err != nil {
		return nil, err
	}
	if apiHandler == nil {
		return nil, fmt.Errorf("API handler is required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	router := chi.NewRouter()
	router.Use(chimw.RequestID)
	router.Use(chimw.RealIP)
	router.Use(requestLogger(logger))
	router.Use(chimw.Recoverer)
	router.Handle("/api", apiHandler)
	router.Handle("/api/*", apiHandler)
	router.Handle("/healthz", apiHandler)
	router.Handle("/readyz", apiHandler)
	router.NotFound(spa.ServeHTTP)

	return otelhttp.NewHandler(router, serviceName), nil
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wrapped := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			started := time.Now()
			recoveryLog := &structuredRecoveryLog{
				logger: logger, ctx: r.Context(), method: r.Method, path: r.URL.Path,
				requestID: chimw.GetReqID(r.Context()),
			}
			next.ServeHTTP(wrapped, chimw.WithLogEntry(r, recoveryLog))
			status := wrapped.Status()
			if status == 0 {
				status = http.StatusOK
			}
			logger.InfoContext(r.Context(), "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"bytes", wrapped.BytesWritten(),
				"duration_ms", time.Since(started).Milliseconds(),
				"request_id", chimw.GetReqID(r.Context()),
				"remote_ip", r.RemoteAddr,
			)
		})
	}
}

type structuredRecoveryLog struct {
	logger    *slog.Logger
	ctx       context.Context
	method    string
	path      string
	requestID string
}

func (*structuredRecoveryLog) Write(int, int, http.Header, time.Duration, any) {}

func (entry *structuredRecoveryLog) Panic(any, []byte) {
	entry.logger.ErrorContext(entry.ctx, "http panic recovered",
		"method", entry.method,
		"path", entry.path,
		"request_id", entry.requestID,
	)
}
