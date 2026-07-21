// Package api exposes the generated HTTP boundary for sampling sessions.
package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/api/openapi"
	cryptox "github.com/timo972/proxy-sampler/internal/crypto"
	"github.com/timo972/proxy-sampler/internal/proxydial"
	"github.com/timo972/proxy-sampler/internal/session"
)

const (
	defaultProbeTarget = "https://speed.cloudflare.com/cdn-cgi/trace"
	defaultDialTimeout = 10 * time.Second
)

// Control owns the sampler worker lifecycle behind the HTTP API.
type Control interface {
	Start(context.Context, uuid.UUID) error
	Stop(context.Context, uuid.UUID) error
	Delete(context.Context, uuid.UUID) error
}

// Defaults supplies request values that are not mode-specific.
type Defaults struct {
	ProbeTarget string
	DialTimeout time.Duration
}

// Server implements the generated strict server interface.
type Server struct {
	store    session.Store
	control  Control
	cipher   *cryptox.Cipher
	defaults Defaults
	now      func() time.Time
}

// NewServer constructs the session control API.
func NewServer(store session.Store, control Control, cipher *cryptox.Cipher, defaults Defaults) *Server {
	if defaults.ProbeTarget == "" {
		defaults.ProbeTarget = defaultProbeTarget
	}
	if defaults.DialTimeout == 0 {
		defaults.DialTimeout = defaultDialTimeout
	}
	return &Server{store: store, control: control, cipher: cipher, defaults: defaults, now: time.Now}
}

// Handler registers generated paths directly on a root chi router.
func (s *Server) Handler() http.Handler {
	router := chi.NewRouter()
	strict := openapi.NewStrictHandlerWithOptions(s, nil, openapi.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  requestErrorHandler,
		ResponseErrorHandlerFunc: responseErrorHandler,
	})
	return openapi.HandlerWithOptions(strict, openapi.ChiServerOptions{
		BaseRouter:       router,
		ErrorHandlerFunc: requestErrorHandler,
	})
}

// CreateSession encrypts, persists, and starts a new sampling session.
func (s *Server) CreateSession(ctx context.Context, request openapi.CreateSessionRequestObject) (openapi.CreateSessionResponseObject, error) {
	if request.Body == nil {
		return nil, invalidRequest()
	}
	body := request.Body
	name := strings.TrimSpace(body.Name)
	if name == "" || utf8.RuneCountInString(name) > 100 || body.Proxy == nil || *body.Proxy == "" {
		return nil, invalidRequest()
	}
	if body.CadenceSeconds < 1 || (body.Mode != openapi.CreateSessionRequestModeSticky && body.Mode != openapi.CreateSessionRequestModePool) {
		return nil, invalidRequest()
	}
	probes := defaultProbes(body.Mode)
	if body.ProbesPerSample != nil {
		probes = *body.ProbesPerSample
	}
	if probes < 1 || probes > 255 {
		return nil, invalidRequest()
	}
	probeTarget := s.defaults.ProbeTarget
	if body.ProbeTarget != nil {
		probeTarget = *body.ProbeTarget
	}
	if !validProbeTarget(probeTarget) {
		return nil, invalidRequest()
	}
	dialTimeout := s.defaults.DialTimeout
	if body.DialTimeoutMs != nil {
		dialTimeout = time.Duration(*body.DialTimeoutMs) * time.Millisecond
	}
	if dialTimeout < 100*time.Millisecond || !validOptionalPositive(body.MaxSamples) || !validOptionalPositive(body.MaxDurationSeconds) {
		return nil, invalidRequest()
	}

	proxyDisplay, err := proxydial.Display(*body.Proxy)
	if err != nil {
		return nil, invalidRequest()
	}
	if s.cipher == nil || s.store == nil || s.control == nil {
		return nil, internalError()
	}
	ciphertext, nonce, err := s.cipher.Encrypt(*body.Proxy)
	if err != nil {
		return nil, internalError()
	}
	now := s.now().UTC()
	value := session.Session{
		ID: uuid.New(), Name: name, ProxyCiphertext: ciphertext, ProxyNonce: nonce,
		ProxyDisplay: proxyDisplay, Mode: session.Mode(body.Mode),
		Cadence: time.Duration(body.CadenceSeconds) * time.Second, ProbesPerSample: probes,
		ProbeTarget: probeTarget, DialTimeout: dialTimeout, MaxSamples: cloneInt(body.MaxSamples),
		MaxDuration: secondsPointer(body.MaxDurationSeconds), Status: session.StatusRunning,
		CreatedAt: now, StartedAt: timePointer(now),
	}
	created, err := s.store.Create(ctx, value)
	if err != nil {
		return nil, internalError()
	}
	if err := s.control.Start(ctx, created.ID); err != nil {
		_ = s.store.Stop(context.WithoutCancel(ctx), created.ID, s.now().UTC())
		return nil, internalError()
	}
	return openapi.CreateSession201JSONResponse(mapSession(created)), nil
}

// ListSessions returns Postgres-backed snapshots without proxy secrets.
func (s *Server) ListSessions(ctx context.Context, _ openapi.ListSessionsRequestObject) (openapi.ListSessionsResponseObject, error) {
	if s.store == nil {
		return nil, internalError()
	}
	values, err := s.store.Sessions(ctx)
	if err != nil {
		return nil, internalError()
	}
	result := make(openapi.ListSessions200JSONResponse, 0, len(values))
	for _, value := range values {
		result = append(result, mapSession(value))
	}
	return result, nil
}

// SessionByID returns one Postgres-backed snapshot without proxy secrets.
func (s *Server) SessionByID(ctx context.Context, request openapi.SessionByIDRequestObject) (openapi.SessionByIDResponseObject, error) {
	if s.store == nil {
		return nil, internalError()
	}
	value, err := s.store.SessionByID(ctx, request.Id)
	if errors.Is(err, session.ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, internalError()
	}
	return openapi.SessionByID200JSONResponse(mapSession(value)), nil
}

// StopSession stops a running sampler worker and its durable row.
func (s *Server) StopSession(ctx context.Context, request openapi.StopSessionRequestObject) (openapi.StopSessionResponseObject, error) {
	if s.control == nil {
		return nil, internalError()
	}
	err := s.control.Stop(ctx, request.Id)
	switch {
	case err == nil:
		return openapi.StopSession204Response{}, nil
	case errors.Is(err, session.ErrNotFound):
		return nil, notFound()
	case errors.Is(err, session.ErrNotRunning):
		return nil, sessionNotRunning()
	default:
		return nil, internalError()
	}
}

// DeleteSession deletes the sampler worker and all durable session data.
func (s *Server) DeleteSession(ctx context.Context, request openapi.DeleteSessionRequestObject) (openapi.DeleteSessionResponseObject, error) {
	if s.control == nil {
		return nil, internalError()
	}
	err := s.control.Delete(ctx, request.Id)
	switch {
	case err == nil:
		return openapi.DeleteSession204Response{}, nil
	case errors.Is(err, session.ErrNotFound):
		return nil, notFound()
	case errors.Is(err, session.ErrNotRunning):
		return nil, sessionNotRunning()
	default:
		return nil, internalError()
	}
}

func defaultProbes(mode openapi.CreateSessionRequestMode) int {
	if mode == openapi.CreateSessionRequestModePool {
		return 8
	}
	return 3
}

func validProbeTarget(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func validOptionalPositive(value *int) bool { return value == nil || *value >= 1 }

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	result := *value
	return &result
}

func secondsPointer(value *int) *time.Duration {
	if value == nil {
		return nil
	}
	result := time.Duration(*value) * time.Second
	return &result
}

func timePointer(value time.Time) *time.Time { return &value }

func mapSession(value session.Session) openapi.Session {
	result := openapi.Session{
		Id: value.ID, Name: value.Name, ProxyDisplay: value.ProxyDisplay,
		Mode: openapi.SessionMode(value.Mode), Status: openapi.SessionStatus(value.Status),
		CadenceSeconds: int(value.Cadence / time.Second), ProbesPerSample: value.ProbesPerSample,
		ProbeTarget: value.ProbeTarget, DialTimeoutMs: int(value.DialTimeout / time.Millisecond),
		MaxSamples: cloneInt(value.MaxSamples), MaxDurationSeconds: durationSeconds(value.MaxDuration),
		SamplesTaken: value.Snapshot.SamplesTaken, ProbesOk: value.Snapshot.ProbesOK,
		ProbesTotal: value.Snapshot.ProbesTotal, DistinctIps: value.Snapshot.DistinctIPs,
		CreatedAt: value.CreatedAt, StartedAt: value.StartedAt, StoppedAt: value.StoppedAt,
		LastSampleAt: value.Snapshot.LastSampleAt, LastRttMs: durationMillis(value.Snapshot.LastRTT),
	}
	if value.Snapshot.ProbesTotal > 0 {
		result.SuccessRate = float64(value.Snapshot.ProbesOK) / float64(value.Snapshot.ProbesTotal)
	}
	if value.Snapshot.LastPrimaryIP.IsValid() {
		lastPrimaryIP := value.Snapshot.LastPrimaryIP.String()
		result.LastPrimaryIp = &lastPrimaryIP
	}
	if value.Snapshot.LastCategory != "" {
		lastCategory := value.Snapshot.LastCategory
		result.LastPrimaryCategory = &lastCategory
	}
	if value.Snapshot.LastError != "" {
		lastError := value.Snapshot.LastError
		result.LastError = &lastError
	}
	return result
}

func durationSeconds(value *time.Duration) *int {
	if value == nil {
		return nil
	}
	result := int(*value / time.Second)
	return &result
}

func durationMillis(value *time.Duration) *int {
	if value == nil {
		return nil
	}
	result := int(*value / time.Millisecond)
	return &result
}

// Task 10 supplies report, sample, export, and health implementations. These
// strict-interface placeholders intentionally expose no behavior in Task 9.
func (s *Server) ExportSessionCSV(context.Context, openapi.ExportSessionCSVRequestObject) (openapi.ExportSessionCSVResponseObject, error) {
	return nil, internalError()
}

func (s *Server) SessionReport(context.Context, openapi.SessionReportRequestObject) (openapi.SessionReportResponseObject, error) {
	return nil, internalError()
}

func (s *Server) SessionSamples(context.Context, openapi.SessionSamplesRequestObject) (openapi.SessionSamplesResponseObject, error) {
	return nil, internalError()
}

func (s *Server) Healthz(context.Context, openapi.HealthzRequestObject) (openapi.HealthzResponseObject, error) {
	return nil, internalError()
}

func (s *Server) Readyz(context.Context, openapi.ReadyzRequestObject) (openapi.ReadyzResponseObject, error) {
	return nil, dependencyUnavailable()
}

var _ openapi.StrictServerInterface = (*Server)(nil)
