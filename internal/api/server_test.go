package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	cryptox "github.com/timo972/proxy-sampler/internal/crypto"
	"github.com/timo972/proxy-sampler/internal/session"
	"github.com/google/uuid"
)

const (
	testProxy       = "socks5://proxy-user:proxy-password@proxy.example:1080"
	testProbeTarget = "https://probe.example/trace"
)

func TestCreateSessionAppliesModeDefaultsAndPersistsRunningSession(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		probes int
	}{
		{name: "sticky", body: `{"name":"  Sticky session  ","proxy":"` + testProxy + `","mode":"sticky","cadence_seconds":30}`, probes: 3},
		{name: "pool", body: `{"name":"Pool session","proxy":"` + testProxy + `","mode":"pool","cadence_seconds":30}`, probes: 8},
		{name: "explicit probes", body: `{"name":"Explicit","proxy":"` + testProxy + `","mode":"pool","cadence_seconds":30,"probes_per_sample":5}`, probes: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMemoryStore()
			control := &fakeControl{}
			handler := testHandler(t, store, control)

			response := request(t, handler, http.MethodPost, "/api/sessions", tt.body)
			if response.Code != http.StatusCreated {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusCreated, response.Body.String())
			}
			assertJSONContentType(t, response)
			if len(store.sessions) != 1 {
				t.Fatalf("created sessions = %d, want 1", len(store.sessions))
			}
			created := store.sessions[0]
			if created.Name != strings.TrimSpace(jsonString(t, tt.body, "name")) {
				t.Errorf("name = %q, want trimmed input", created.Name)
			}
			if created.ProxyDisplay != "proxy.example:1080" {
				t.Errorf("proxy display = %q", created.ProxyDisplay)
			}
			if len(created.ProxyCiphertext) == 0 || len(created.ProxyNonce) == 0 {
				t.Error("proxy credentials were not encrypted")
			}
			if created.ProbesPerSample != tt.probes {
				t.Errorf("probes per sample = %d, want %d", created.ProbesPerSample, tt.probes)
			}
			if created.ProbeTarget != testProbeTarget {
				t.Errorf("probe target = %q, want %q", created.ProbeTarget, testProbeTarget)
			}
			if created.DialTimeout != 10*time.Second {
				t.Errorf("dial timeout = %s, want 10s", created.DialTimeout)
			}
			if created.Status != session.StatusRunning {
				t.Errorf("status = %q, want running", created.Status)
			}
			if created.StartedAt == nil || !created.StartedAt.Equal(created.CreatedAt) {
				t.Errorf("started_at = %v, created_at = %v", created.StartedAt, created.CreatedAt)
			}
			if created.CreatedAt.Location() != time.UTC {
				t.Errorf("created_at location = %v, want UTC", created.CreatedAt.Location())
			}
			if len(control.started) != 1 || control.started[0] != created.ID {
				t.Errorf("started IDs = %v, want [%s]", control.started, created.ID)
			}
			assertNoSecret(t, response.Body.String())
		})
	}
}

func TestCreateSessionUsesExplicitTargetTimeoutAndCaps(t *testing.T) {
	store := newMemoryStore()
	handler := testHandler(t, store, &fakeControl{})
	body := `{"name":"Custom","proxy":"` + testProxy + `","mode":"sticky","cadence_seconds":4,"probes_per_sample":7,"probe_target":"http://probe.example/check","dial_timeout_ms":750,"max_samples":12,"max_duration_seconds":90}`

	response := request(t, handler, http.MethodPost, "/api/sessions", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", response.Code, response.Body.String())
	}
	created := store.sessions[0]
	if created.ProbeTarget != "http://probe.example/check" || created.DialTimeout != 750*time.Millisecond {
		t.Errorf("target/timeout = %q/%s", created.ProbeTarget, created.DialTimeout)
	}
	if created.MaxSamples == nil || *created.MaxSamples != 12 {
		t.Errorf("max samples = %v", created.MaxSamples)
	}
	if created.MaxDuration == nil || *created.MaxDuration != 90*time.Second {
		t.Errorf("max duration = %v", created.MaxDuration)
	}
}

func TestCreateSessionRejectsMalformedProxyBeforeEncryption(t *testing.T) {
	store := newMemoryStore()
	server := NewServer(store, &fakeControl{}, nil, Defaults{ProbeTarget: testProbeTarget, DialTimeout: 10 * time.Second})

	response := request(t, server.Handler(), http.MethodPost, "/api/sessions", `{"name":"Invalid","proxy":"not a proxy URL","mode":"sticky","cadence_seconds":10}`)
	assertAPIError(t, response, http.StatusBadRequest, "invalid_request")
	if len(store.sessions) != 0 {
		t.Fatalf("created sessions = %d, want 0", len(store.sessions))
	}
	assertNoSecret(t, response.Body.String())
}

func TestCreateSessionRejectsInvalidProbeTarget(t *testing.T) {
	store := newMemoryStore()
	handler := testHandler(t, store, &fakeControl{})
	body := `{"name":"Invalid target","proxy":"` + testProxy + `","mode":"sticky","cadence_seconds":10,"probe_target":"ftp://probe.example/file"}`

	response := request(t, handler, http.MethodPost, "/api/sessions", body)
	assertAPIError(t, response, http.StatusBadRequest, "invalid_request")
	if len(store.sessions) != 0 {
		t.Fatalf("created sessions = %d, want 0", len(store.sessions))
	}
}

func TestCreateSessionRejectsRuntimeSchemaViolations(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown property", body: `{"name":"Unknown","proxy":"` + testProxy + `","mode":"sticky","cadence_seconds":10,"unexpected":true}`},
		{name: "null probes", body: `{"name":"Null probes","proxy":"` + testProxy + `","mode":"sticky","cadence_seconds":10,"probes_per_sample":null}`},
		{name: "null probe target", body: `{"name":"Null target","proxy":"` + testProxy + `","mode":"sticky","cadence_seconds":10,"probe_target":null}`},
		{name: "null dial timeout", body: `{"name":"Null timeout","proxy":"` + testProxy + `","mode":"sticky","cadence_seconds":10,"dial_timeout_ms":null}`},
		{name: "trailing JSON", body: `{"name":"Trailing","proxy":"` + testProxy + `","mode":"sticky","cadence_seconds":10} {}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMemoryStore()
			response := request(t, testHandler(t, store, &fakeControl{}), http.MethodPost, "/api/sessions", tt.body)
			assertAPIError(t, response, http.StatusBadRequest, "invalid_request")
			if len(store.sessions) != 0 {
				t.Fatalf("created sessions = %d, want 0", len(store.sessions))
			}
		})
	}
}

func TestCreateSessionEnforcesRequestBodyLimit(t *testing.T) {
	const bodyLimit = 1 << 20
	prefix := `{"name":"Boundary","proxy":"socks5://proxy.example:1080/`
	suffix := `","mode":"sticky","cadence_seconds":10}`
	bodyAtLimit := prefix + strings.Repeat("a", bodyLimit-len(prefix)-len(suffix)) + suffix
	if len(bodyAtLimit) != bodyLimit {
		t.Fatalf("boundary body length = %d, want %d", len(bodyAtLimit), bodyLimit)
	}

	t.Run("exact limit accepted and replayed", func(t *testing.T) {
		store := newMemoryStore()
		control := &fakeControl{}
		response := request(t, testHandler(t, store, control), http.MethodPost, "/api/sessions", bodyAtLimit)
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body=%s", response.Code, response.Body.String())
		}
		if len(store.sessions) != 1 || len(control.started) != 1 {
			t.Fatalf("created/started = %d/%d, want 1/1", len(store.sessions), len(control.started))
		}
	})

	t.Run("limit plus one rejected before encryption", func(t *testing.T) {
		store := newMemoryStore()
		control := &fakeControl{}
		server := NewServer(store, control, nil, Defaults{ProbeTarget: testProbeTarget, DialTimeout: 10 * time.Second})
		response := request(t, server.Handler(), http.MethodPost, "/api/sessions", bodyAtLimit+" ")
		assertAPIError(t, response, http.StatusBadRequest, "invalid_request")
		if len(store.sessions) != 0 || len(control.started) != 0 {
			t.Fatalf("created/started = %d/%d, want 0/0", len(store.sessions), len(control.started))
		}
	})
}

func TestCreateSessionEnforcesPersistedIntegerBounds(t *testing.T) {
	const maxInt32 = 2147483647
	t.Run("maximum accepted", func(t *testing.T) {
		store := newMemoryStore()
		body := `{"name":"Maximum","proxy":"` + testProxy + `","mode":"sticky","cadence_seconds":2147483647,"dial_timeout_ms":2147483647,"max_samples":2147483647,"max_duration_seconds":2147483647}`
		response := request(t, testHandler(t, store, &fakeControl{}), http.MethodPost, "/api/sessions", body)
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body=%s", response.Code, response.Body.String())
		}
		created := store.sessions[0]
		if created.Cadence != time.Duration(maxInt32)*time.Second || created.DialTimeout != time.Duration(maxInt32)*time.Millisecond {
			t.Errorf("durations = %s/%s, want max persisted values", created.Cadence, created.DialTimeout)
		}
		if created.MaxSamples == nil || *created.MaxSamples != maxInt32 || created.MaxDuration == nil || *created.MaxDuration != time.Duration(maxInt32)*time.Second {
			t.Errorf("caps = %v/%v, want max persisted values", created.MaxSamples, created.MaxDuration)
		}
	})

	tests := []struct {
		name string
		body string
	}{
		{name: "cadence", body: `{"name":"Overflow","proxy":"` + testProxy + `","mode":"sticky","cadence_seconds":2147483648}`},
		{name: "dial timeout", body: `{"name":"Overflow","proxy":"` + testProxy + `","mode":"sticky","cadence_seconds":10,"dial_timeout_ms":2147483648}`},
		{name: "max samples", body: `{"name":"Overflow","proxy":"` + testProxy + `","mode":"sticky","cadence_seconds":10,"max_samples":2147483648}`},
		{name: "max duration", body: `{"name":"Overflow","proxy":"` + testProxy + `","mode":"sticky","cadence_seconds":10,"max_duration_seconds":2147483648}`},
	}
	for _, tt := range tests {
		t.Run(tt.name+" overflow rejected", func(t *testing.T) {
			store := newMemoryStore()
			response := request(t, testHandler(t, store, &fakeControl{}), http.MethodPost, "/api/sessions", tt.body)
			assertAPIError(t, response, http.StatusBadRequest, "invalid_request")
			if len(store.sessions) != 0 {
				t.Fatalf("created sessions = %d, want 0", len(store.sessions))
			}
		})
	}
}

func TestCreateSessionStartFailureStopsPersistedRow(t *testing.T) {
	store := newMemoryStore()
	control := &fakeControl{startErr: errors.New("prepare worker with secret proxy-password failed")}
	handler := testHandler(t, store, control)

	response := request(t, handler, http.MethodPost, "/api/sessions", `{"name":"Fails","proxy":"`+testProxy+`","mode":"sticky","cadence_seconds":10}`)
	assertAPIError(t, response, http.StatusInternalServerError, "internal_error")
	if len(store.sessions) != 1 {
		t.Fatalf("created sessions = %d, want 1", len(store.sessions))
	}
	created := store.sessions[0]
	if created.Status != session.StatusStopped || created.StoppedAt == nil {
		t.Errorf("persisted row after failure = status %q, stopped_at %v", created.Status, created.StoppedAt)
	}
	assertNoSecret(t, response.Body.String())
}

func TestCreateSessionStartFailureStopsRowAfterRequestCancellation(t *testing.T) {
	store := newMemoryStore()
	store.rejectCanceledStop = true
	requestContext, cancel := context.WithCancel(context.Background())
	control := &fakeControl{
		start: func(context.Context, uuid.UUID) error {
			cancel()
			return context.Canceled
		},
	}
	handler := testHandler(t, store, control)
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"name":"Canceled","proxy":"`+testProxy+`","mode":"sticky","cadence_seconds":10}`)).WithContext(requestContext)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, req)

	assertAPIError(t, response, http.StatusInternalServerError, "internal_error")
	if len(store.sessions) != 1 || store.sessions[0].Status != session.StatusStopped {
		t.Fatalf("persisted row after canceled start = %#v, want stopped", store.sessions)
	}
}

func TestCreateSessionStartFailureDeletesRowWhenStopCleanupFails(t *testing.T) {
	store := newMemoryStore()
	store.stopErr = errors.New("stop failed")
	control := &fakeControl{startErr: errors.New("start failed")}
	handler := testHandler(t, store, control)

	response := request(t, handler, http.MethodPost, "/api/sessions", `{"name":"Cleanup","proxy":"`+testProxy+`","mode":"sticky","cadence_seconds":10}`)

	assertAPIError(t, response, http.StatusInternalServerError, "internal_error")
	if len(store.sessions) != 0 {
		t.Fatalf("persisted sessions after cleanup = %#v, want deleted", store.sessions)
	}
	if store.deleteCalls != 1 {
		t.Fatalf("delete calls = %d, want 1", store.deleteCalls)
	}
}

func TestListSessionsRedactsSecretsAndComputesSuccessRate(t *testing.T) {
	store := newMemoryStore()
	value := sampleSession()
	value.Snapshot.ProbesOK = 3
	value.Snapshot.ProbesTotal = 4
	store.sessions = []session.Session{value}
	handler := testHandler(t, store, &fakeControl{})

	response := request(t, handler, http.MethodGet, "/api/sessions", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	assertJSONContentType(t, response)
	assertNoSecret(t, response.Body.String())
	var body []map[string]any
	decodeJSON(t, response, &body)
	if len(body) != 1 || body[0]["success_rate"] != 0.75 {
		t.Fatalf("body = %#v, want success_rate 0.75", body)
	}
	if _, exists := body[0]["proxy_ciphertext"]; exists {
		t.Error("response contains proxy_ciphertext")
	}
	if _, exists := body[0]["proxy_nonce"]; exists {
		t.Error("response contains proxy_nonce")
	}
	if _, exists := body[0]["proxy"]; exists {
		t.Error("response contains proxy")
	}
}

func TestSessionByIDRedactsSecretsAndUsesZeroSuccessRate(t *testing.T) {
	store := newMemoryStore()
	value := sampleSession()
	store.sessions = []session.Session{value}
	handler := testHandler(t, store, &fakeControl{})

	response := request(t, handler, http.MethodGet, "/api/sessions/"+value.ID.String(), "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	assertNoSecret(t, response.Body.String())
	var body map[string]any
	decodeJSON(t, response, &body)
	if body["success_rate"] != float64(0) {
		t.Fatalf("success_rate = %v, want 0", body["success_rate"])
	}
}

func TestSessionByIDMissingReturnsStableJSONError(t *testing.T) {
	store := newMemoryStore()
	handler := testHandler(t, store, &fakeControl{})
	response := request(t, handler, http.MethodGet, "/api/sessions/"+uuid.NewString(), "")
	assertAPIError(t, response, http.StatusNotFound, "not_found")
}

func TestStopSessionMapsNotRunningToConflict(t *testing.T) {
	control := &fakeControl{stopErr: session.ErrNotRunning}
	handler := testHandler(t, newMemoryStore(), control)
	id := uuid.New()

	response := request(t, handler, http.MethodPost, "/api/sessions/"+id.String()+"/stop", "")
	assertAPIError(t, response, http.StatusConflict, "session_not_running")
	if len(control.stopped) != 1 || control.stopped[0] != id {
		t.Fatalf("stopped IDs = %v, want [%s]", control.stopped, id)
	}
}

func TestStopSessionReturnsNoContent(t *testing.T) {
	handler := testHandler(t, newMemoryStore(), &fakeControl{})
	response := request(t, handler, http.MethodPost, "/api/sessions/"+uuid.NewString()+"/stop", "")
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("response = %d %q, want 204 empty", response.Code, response.Body.String())
	}
}

func TestDeleteSessionMapsMissingToNotFound(t *testing.T) {
	control := &fakeControl{deleteErr: session.ErrNotFound}
	handler := testHandler(t, newMemoryStore(), control)
	id := uuid.New()

	response := request(t, handler, http.MethodDelete, "/api/sessions/"+id.String(), "")
	assertAPIError(t, response, http.StatusNotFound, "not_found")
	if len(control.deleted) != 1 || control.deleted[0] != id {
		t.Fatalf("deleted IDs = %v, want [%s]", control.deleted, id)
	}
}

func TestDeleteSessionReturnsNoContent(t *testing.T) {
	handler := testHandler(t, newMemoryStore(), &fakeControl{})
	response := request(t, handler, http.MethodDelete, "/api/sessions/"+uuid.NewString(), "")
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("response = %d %q, want 204 empty", response.Code, response.Body.String())
	}
}

func TestGeneratedRequestErrorsUseStableJSONShape(t *testing.T) {
	handler := testHandler(t, newMemoryStore(), &fakeControl{})
	tests := []struct {
		name, method, path, body string
	}{
		{name: "malformed JSON", method: http.MethodPost, path: "/api/sessions", body: "{"},
		{name: "malformed UUID", method: http.MethodGet, path: "/api/sessions/not-a-uuid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := request(t, handler, tt.method, tt.path, tt.body)
			assertAPIError(t, response, http.StatusBadRequest, "invalid_request")
		})
	}
}

func TestControlRoutesAreRegisteredAtPublicPaths(t *testing.T) {
	handler := testHandler(t, newMemoryStore(), &fakeControl{})
	response := request(t, handler, http.MethodGet, "/api/api/sessions", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("double-prefixed path status = %d, want 404", response.Code)
	}
}

func TestReadyPlaceholderReturnsDeclaredInternalError(t *testing.T) {
	response := request(t, testHandler(t, newMemoryStore(), &fakeControl{}), http.MethodGet, "/readyz", "")
	assertAPIError(t, response, http.StatusInternalServerError, "internal_error")
}

func testHandler(t *testing.T, store session.Store, control Control) http.Handler {
	t.Helper()
	cipher, err := cryptox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(store, control, cipher, Defaults{ProbeTarget: testProbeTarget, DialTimeout: 10 * time.Second}).Handler()
}

func request(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func assertAPIError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, status, response.Body.String())
	}
	assertJSONContentType(t, response)
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	decodeJSON(t, response, &body)
	if body.Code != code || body.Message == "" {
		t.Fatalf("error = %#v, want code %q and non-empty message", body, code)
	}
}

func assertJSONContentType(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func assertNoSecret(t *testing.T, body string) {
	t.Helper()
	for _, secret := range []string{"proxy-user", "proxy-password", testProxy} {
		if strings.Contains(body, secret) {
			t.Errorf("response contains proxy secret %q: %s", secret, body)
		}
	}
}

func decodeJSON(t *testing.T, response *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
}

func jsonString(t *testing.T, raw, key string) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	return body[key].(string)
}

func sampleSession() session.Session {
	created := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	started := created
	lastSample := created.Add(time.Minute)
	lastRTT := 125 * time.Millisecond
	return session.Session{
		ID: uuid.New(), Name: "Existing", ProxyCiphertext: []byte("ciphertext-secret"),
		ProxyNonce: []byte("nonce-secret"), ProxyDisplay: "proxy.example:1080",
		Mode: session.ModeSticky, Cadence: 30 * time.Second, ProbesPerSample: 3,
		ProbeTarget: testProbeTarget, DialTimeout: 10 * time.Second, Status: session.StatusRunning,
		Snapshot: session.Snapshot{
			SamplesTaken: 1, DistinctIPs: 1, LastSampleAt: &lastSample,
			LastPrimaryIP: netip.MustParseAddr("203.0.113.1"), LastCategory: "residential",
			LastRTT: &lastRTT,
		},
		CreatedAt: created, StartedAt: &started,
	}
}

type fakeControl struct {
	startErr, stopErr, deleteErr error
	started, stopped, deleted    []uuid.UUID
	start                        func(context.Context, uuid.UUID) error
}

func (f *fakeControl) Start(ctx context.Context, id uuid.UUID) error {
	f.started = append(f.started, id)
	if f.start != nil {
		return f.start(ctx, id)
	}
	return f.startErr
}

func (f *fakeControl) Stop(_ context.Context, id uuid.UUID) error {
	f.stopped = append(f.stopped, id)
	return f.stopErr
}

func (f *fakeControl) Delete(_ context.Context, id uuid.UUID) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

type memoryStore struct {
	sessions           []session.Session
	listErr            error
	getErr             error
	createErr          error
	stopErr            error
	deleteErr          error
	rejectCanceledStop bool
	deleteCalls        int
}

func newMemoryStore() *memoryStore { return &memoryStore{sessions: []session.Session{}} }

func (s *memoryStore) Create(_ context.Context, value session.Session) (session.Session, error) {
	if s.createErr != nil {
		return session.Session{}, s.createErr
	}
	s.sessions = append(s.sessions, value)
	return value, nil
}

func (s *memoryStore) Sessions(context.Context) ([]session.Session, error) {
	return append([]session.Session(nil), s.sessions...), s.listErr
}

func (s *memoryStore) SessionByID(_ context.Context, id uuid.UUID) (session.Session, error) {
	if s.getErr != nil {
		return session.Session{}, s.getErr
	}
	for _, value := range s.sessions {
		if value.ID == id {
			return value, nil
		}
	}
	return session.Session{}, session.ErrNotFound
}

func (s *memoryStore) RunningSessions(ctx context.Context) ([]session.Session, error) {
	return s.Sessions(ctx)
}

func (s *memoryStore) Stop(ctx context.Context, id uuid.UUID, at time.Time) error {
	if s.rejectCanceledStop && ctx.Err() != nil {
		return ctx.Err()
	}
	if s.stopErr != nil {
		return s.stopErr
	}
	for i := range s.sessions {
		if s.sessions[i].ID == id && s.sessions[i].Status == session.StatusRunning {
			s.sessions[i].Status = session.StatusStopped
			s.sessions[i].StoppedAt = &at
			return nil
		}
	}
	return session.ErrNotRunning
}

func (s *memoryStore) Finish(context.Context, uuid.UUID, time.Time) error { return nil }

func (s *memoryStore) Delete(_ context.Context, id uuid.UUID) error {
	s.deleteCalls++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	for i := range s.sessions {
		if s.sessions[i].ID == id {
			s.sessions = append(s.sessions[:i], s.sessions[i+1:]...)
			return nil
		}
	}
	return session.ErrNotFound
}

func (s *memoryStore) SaveTick(context.Context, uuid.UUID, session.Snapshot, []session.IPHit) error {
	return nil
}

func (s *memoryStore) SessionIPs(context.Context, uuid.UUID) ([]session.IPRecord, error) {
	return nil, nil
}

func (s *memoryStore) ReputationByIP(context.Context, netip.Addr) (session.Reputation, bool, error) {
	return session.Reputation{}, false, nil
}

func (s *memoryStore) SaveReputation(context.Context, session.Reputation) error { return nil }
