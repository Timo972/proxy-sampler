// Package api exposes the generated HTTP boundary for sampling sessions.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/api/openapi"
	"github.com/timo972/proxy-sampler/internal/ch"
	cryptox "github.com/timo972/proxy-sampler/internal/crypto"
	"github.com/timo972/proxy-sampler/internal/proxydial"
	"github.com/timo972/proxy-sampler/internal/session"
	"github.com/timo972/proxy-sampler/internal/variation"
)

const (
	defaultProbeTarget        = "https://speed.cloudflare.com/cdn-cgi/trace"
	defaultDialTimeout        = 10 * time.Second
	maxCreateSessionBodyBytes = 1 << 20
	maxPersistedInteger       = 2147483647
)

// Control owns the sampler worker lifecycle behind the HTTP API.
type Control interface {
	Start(context.Context, uuid.UUID) error
	Stop(context.Context, uuid.UUID) error
	Reenable(context.Context, uuid.UUID) error
	Delete(context.Context, uuid.UUID) error
}

// Defaults supplies request values that are not mode-specific.
type Defaults struct {
	ProbeTarget string
	DialTimeout time.Duration
}

// Server implements the generated strict server interface.
type Server struct {
	store       session.Store
	control     Control
	cipher      *cryptox.Cipher
	reader      Reader
	defaults    Defaults
	now         func() time.Time
	runStore    variation.Store
	maxVariants int
}

// Reader supplies ClickHouse-backed report, sample, export, and readiness data.
type Reader interface {
	Ping(context.Context) error
	Samples(context.Context, uuid.UUID, *time.Time, *time.Time, int) (ch.SamplePage, error)
	StreamSamples(context.Context, uuid.UUID, *time.Time, *time.Time, func(ch.Event) error) error
	Series(context.Context, uuid.UUID, time.Time, time.Time, time.Duration) ([]ch.SeriesPoint, error)
	Stickiness(context.Context, uuid.UUID, time.Time, time.Time) (ch.Stickiness, error)
	PoolGrowth(context.Context, uuid.UUID, time.Time, time.Time) ([]ch.GrowthPoint, error)
	SeriesForSessions(context.Context, []uuid.UUID, time.Time, time.Time, time.Duration) ([]ch.SeriesPoint, error)
}

// NewServer constructs the session and run control API.
// The optional reader preserves compatibility for control-only construction.
func NewServer(store session.Store, control Control, cipher *cryptox.Cipher, defaults Defaults, runStore variation.Store, maxVariants int, readers ...Reader) *Server {
	if defaults.ProbeTarget == "" {
		defaults.ProbeTarget = defaultProbeTarget
	}
	if defaults.DialTimeout == 0 {
		defaults.DialTimeout = defaultDialTimeout
	}
	if maxVariants <= 0 {
		maxVariants = 128
	}
	var reader Reader
	if len(readers) > 0 {
		reader = readers[0]
	}
	return &Server{
		store: store, control: control, cipher: cipher, reader: reader, defaults: defaults, now: time.Now,
		runStore: runStore, maxVariants: maxVariants,
	}
}

// Handler registers generated paths directly on a root chi router.
func (s *Server) Handler() http.Handler {
	router := chi.NewRouter()
	router.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, &apiError{status: http.StatusNotFound, code: errorCodeNotFound, message: "route not found"})
	})
	strict := openapi.NewStrictHandlerWithOptions(s, nil, openapi.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  requestErrorHandler,
		ResponseErrorHandlerFunc: responseErrorHandler,
	})
	return openapi.HandlerWithOptions(strict, openapi.ChiServerOptions{
		BaseRouter:       router,
		ErrorHandlerFunc: requestErrorHandler,
		Middlewares:      []openapi.MiddlewareFunc{validateCreateSessionRequest},
	})
}

// validateCreateSessionRequest caps the body size of POST /api/sessions and
// POST /api/runs, and additionally validates the field allowlist for
// /api/sessions (runs have a different, axes-based body shape that is
// validated downstream in the run-expansion path instead).
func validateCreateSessionRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions":
			raw, ok := readCappedBody(w, r)
			if !ok {
				return
			}
			if err := validateCreateSessionJSON(raw); err != nil {
				requestErrorHandler(w, r, err)
				return
			}
		case r.Method == http.MethodPost && r.URL.Path == "/api/runs":
			raw, ok := readCappedBody(w, r)
			if !ok {
				return
			}
			if err := validateCreateRunJSON(raw); err != nil {
				requestErrorHandler(w, r, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// readCappedBody reads r.Body through a maxCreateSessionBodyBytes-limited
// reader and resets r.Body/r.ContentLength so downstream handlers can read it
// again. On error it writes the request-error response and returns ok=false;
// callers must stop processing the request in that case.
func readCappedBody(w http.ResponseWriter, r *http.Request) (raw []byte, ok bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCreateSessionBodyBytes))
	if err != nil {
		requestErrorHandler(w, r, err)
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	return raw, true
}

func validateCreateSessionJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return invalidRequest()
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invalidRequest()
	}
	for name, value := range fields {
		switch name {
		case "name", "proxy", "mode", "cadence_seconds", "max_samples", "max_duration_seconds":
		case "probes_per_sample", "probe_target", "dial_timeout_ms":
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return invalidRequest()
			}
		default:
			return invalidRequest()
		}
	}
	return nil
}

// validateCreateRunJSON enforces the same strict shape for POST /api/runs that
// validateCreateSessionJSON enforces for sessions: an exact field allowlist, no
// trailing data, no null on non-nullable optional fields, and per-axis key
// allowlisting. Without it the generated decoder silently drops typo'd fields
// (e.g. max_sample) and nested axis typos, despite additionalProperties:false.
func validateCreateRunJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return invalidRequest()
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invalidRequest()
	}
	for name, value := range fields {
		switch name {
		case "name", "template", "mode", "cadence_seconds", "max_samples", "max_duration_seconds":
		case "probes_per_sample", "probe_target", "dial_timeout_ms":
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return invalidRequest()
			}
		case "axes":
			if err := validateAxesJSON(value); err != nil {
				return err
			}
		default:
			return invalidRequest()
		}
	}
	return nil
}

// validateAxesJSON rejects a null axes object and any axis carrying a key
// outside the AxisSpec allowlist, so a typo like "value" (for "values") fails
// loudly instead of being silently ignored.
func validateAxesJSON(raw json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return invalidRequest()
	}
	var axes map[string]json.RawMessage
	if err := json.Unmarshal(raw, &axes); err != nil {
		return invalidRequest()
	}
	for _, spec := range axes {
		var axisFields map[string]json.RawMessage
		if err := json.Unmarshal(spec, &axisFields); err != nil {
			return invalidRequest()
		}
		for key := range axisFields {
			switch key {
			case "kind", "values", "from", "to", "count", "length":
			default:
				return invalidRequest()
			}
		}
	}
	return nil
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
	if !validPersistedInteger(body.CadenceSeconds, 1) || (body.Mode != openapi.CreateSessionRequestModeSticky && body.Mode != openapi.CreateSessionRequestModePool) {
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
		if !validPersistedInteger(*body.DialTimeoutMs, 100) {
			return nil, invalidRequest()
		}
		dialTimeout = time.Duration(*body.DialTimeoutMs) * time.Millisecond
	}
	if dialTimeout < 100*time.Millisecond || dialTimeout > time.Duration(maxPersistedInteger)*time.Millisecond ||
		!validOptionalPersistedInteger(body.MaxSamples) || !validOptionalPersistedInteger(body.MaxDurationSeconds) {
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
		cleanupContext := context.WithoutCancel(ctx)
		if err := s.store.Stop(cleanupContext, created.ID, s.now().UTC()); err != nil {
			_ = s.store.Delete(cleanupContext, created.ID)
		}
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
	mapped := mapSession(value)
	if s.cipher != nil {
		if plaintext, err := s.cipher.Decrypt(value.ProxyCiphertext, value.ProxyNonce); err == nil {
			if u, err := url.Parse(plaintext); err == nil {
				if username := u.User.Username(); username != "" {
					mapped.ProxyUsername = &username
				}
			}
		}
	}
	return openapi.SessionByID200JSONResponse(mapped), nil
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

// ReenableSession restarts a stopped or finished session with reset per-run
// counters, preserving prior samples, and returns the refreshed snapshot.
func (s *Server) ReenableSession(ctx context.Context, request openapi.ReenableSessionRequestObject) (openapi.ReenableSessionResponseObject, error) {
	if s.control == nil || s.store == nil {
		return nil, internalError()
	}
	err := s.control.Reenable(ctx, request.Id)
	switch {
	case err == nil:
		value, err := s.store.SessionByID(ctx, request.Id)
		if err != nil {
			return nil, internalError()
		}
		return openapi.ReenableSession200JSONResponse(mapSession(value)), nil
	case errors.Is(err, session.ErrNotFound):
		return nil, notFound()
	case errors.Is(err, session.ErrAlreadyRunning):
		return nil, sessionAlreadyRunning()
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

func validPersistedInteger(value, minimum int) bool {
	return value >= minimum && value <= maxPersistedInteger
}

func validOptionalPersistedInteger(value *int) bool {
	return value == nil || validPersistedInteger(*value, 1)
}

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

var _ openapi.StrictServerInterface = (*Server)(nil)
