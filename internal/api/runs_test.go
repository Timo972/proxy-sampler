package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	cryptox "github.com/timo972/proxy-sampler/internal/crypto"
	"github.com/timo972/proxy-sampler/internal/session"
	"github.com/timo972/proxy-sampler/internal/variation"
)

func TestRunCSVStreamResponseAbortsOnMidStreamError(t *testing.T) {
	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write([]byte("ip,category\n"))
		_ = writer.CloseWithError(errors.New("store failed mid-stream"))
	}()
	response := runCSVStreamResponse{body: reader, contentDisposition: `attachment; filename="x.csv"`}

	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Fatalf("recover = %v, want http.ErrAbortHandler (abort the truncated stream)", r)
		}
	}()
	_ = response.VisitExportRunCSVResponse(httptest.NewRecorder())
	t.Fatal("expected the visitor to panic(http.ErrAbortHandler) on a mid-stream error")
}

func TestCreateRunExpandsVariants(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	control := &fakeControl{}
	handler := testRunHandler(t, store, runStore, control)

	body := `{"name":"poolcheck","template":"socks5h://u-cc-{country}:pw@gate.example:1080","axes":{"country":{"kind":"list","values":["de","us","fr"]}},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	if len(runStore.runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runStore.runs))
	}
	if got := runStore.children[runStore.runs[0].ID]; len(got) != 3 {
		t.Fatalf("children = %d, want 3", len(got))
	}
	if control.startCount != 3 {
		t.Fatalf("Start calls = %d, want 3", control.startCount)
	}
	// Template stored encrypted, never echoed.
	if runStore.runs[0].TemplateDisplay != "gate.example:1080" {
		t.Fatalf("template display = %q", runStore.runs[0].TemplateDisplay)
	}
	if len(runStore.runs[0].TemplateCiphertext) == 0 {
		t.Fatal("template not encrypted")
	}
}

func TestCreateRunPropagatesTargetCountryToChildren(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	handler := testRunHandler(t, store, runStore, &fakeControl{})

	body := `{"name":"geo","template":"socks5h://u-sid-{session}:pw@gate.example:1080","axes":{"session":{"kind":"random","count":2,"length":8}},"mode":"sticky","cadence_seconds":30,"target_country":"us"}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	children := runStore.children[runStore.runs[0].ID]
	if len(children) != 2 {
		t.Fatalf("children = %d, want 2", len(children))
	}
	for _, child := range children {
		if child.Session.TargetCountry != "US" {
			t.Errorf("child target country = %q, want US (normalized uppercase)", child.Session.TargetCountry)
		}
	}
}

func TestCreateRunRejectsInvalidTargetCountry(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	handler := testRunHandler(t, store, runStore, &fakeControl{})

	body := `{"name":"geo","template":"socks5h://u:pw@gate.example:1080","axes":{},"mode":"sticky","cadence_seconds":30,"target_country":"usa"}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
	if len(runStore.runs) != 0 {
		t.Fatal("run persisted despite invalid target country")
	}
}

func TestCreateRunRejectsCapExceeded(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	handler := testRunHandlerWithCap(t, store, runStore, &fakeControl{}, 2)
	body := `{"name":"big","template":"p://u-{a}:pw@gate.example:1080","axes":{"a":{"kind":"range","from":1,"to":10}},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.Code)
	}
}

func TestCreateRunEnforcesRequestBodyLimit(t *testing.T) {
	const bodyLimit = 1 << 20

	t.Run("body over the limit is rejected before expansion", func(t *testing.T) {
		store := newMemoryStore()
		runStore := newMemoryRunStore()
		control := &fakeControl{}
		handler := testRunHandler(t, store, runStore, control)
		// Padding pushes the body past the 1 MiB cap; readCappedBody rejects it
		// during the read, before any parsing or expansion happens.
		prefix := `{"name":"Over","template":"socks5h://u-{c}:pw@proxy.example:1080","mode":"sticky","cadence_seconds":10,"axes":{"c":{"kind":"list","values":["`
		oversized := prefix + strings.Repeat("a", bodyLimit) + `"]}}}`
		response := request(t, handler, http.MethodPost, "/api/runs", oversized)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", response.Code)
		}
		if len(runStore.runs) != 0 || control.startCount != 0 {
			t.Fatalf("created/started = %d/%d, want 0/0 (rejected before expansion)", len(runStore.runs), control.startCount)
		}
	})

	t.Run("normal body under the limit is accepted", func(t *testing.T) {
		store := newMemoryStore()
		runStore := newMemoryRunStore()
		control := &fakeControl{}
		handler := testRunHandler(t, store, runStore, control)
		body := `{"name":"OK","template":"socks5h://u-{c}:pw@proxy.example:1080","axes":{"c":{"kind":"list","values":["de"]}},"mode":"sticky","cadence_seconds":10}`
		response := request(t, handler, http.MethodPost, "/api/runs", body)
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body=%s", response.Code, response.Body.String())
		}
		if len(runStore.runs) != 1 || control.startCount != 1 {
			t.Fatalf("created/started = %d/%d, want 1/1", len(runStore.runs), control.startCount)
		}
	})
}

func TestCreateRunRejectsUnmatchedAxis(t *testing.T) {
	handler := testRunHandler(t, newMemoryStore(), newMemoryRunStore(), &fakeControl{})
	body := `{"name":"x","template":"p://u:pw@gate.example:1080","axes":{"country":{"kind":"list","values":["de"]}},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.Code)
	}
}

func TestStopRunFansOutToChildren(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	control := &fakeControl{}
	handler := testRunHandler(t, store, runStore, control)
	create := request(t, handler, http.MethodPost, "/api/runs",
		`{"name":"r","template":"p://u-{c}:pw@gate.example:1080","axes":{"c":{"kind":"list","values":["de","us"]}},"mode":"sticky","cadence_seconds":30}`)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d", create.Code)
	}
	runID := runStore.runs[0].ID.String()
	resp := request(t, handler, http.MethodPost, "/api/runs/"+runID+"/stop", "")
	if resp.Code != http.StatusOK {
		t.Fatalf("stop status = %d body=%s", resp.Code, resp.Body.String())
	}
	if control.stopCount != 2 {
		t.Fatalf("Stop calls = %d, want 2", control.stopCount)
	}
}

func TestDeleteRunDeletesChildrenThenRun(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	control := &fakeControl{}
	handler := testRunHandler(t, store, runStore, control)
	request(t, handler, http.MethodPost, "/api/runs",
		`{"name":"r","template":"p://u-{c}:pw@gate.example:1080","axes":{"c":{"kind":"list","values":["de","us"]}},"mode":"sticky","cadence_seconds":30}`)
	runID := runStore.runs[0].ID
	resp := request(t, handler, http.MethodDelete, "/api/runs/"+runID.String(), "")
	if resp.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.Code)
	}
	// Children are deleted in a single batch, then the run row.
	if len(control.deletedBatches) != 1 {
		t.Fatalf("DeleteSessions calls = %d, want 1 batch", len(control.deletedBatches))
	}
	if got := len(control.deletedBatches[0]); got != 2 {
		t.Fatalf("batch size = %d, want 2 children", got)
	}
	if len(runStore.runs) != 0 {
		t.Fatal("run row not deleted")
	}
}

func TestStopRunNotFound(t *testing.T) {
	handler := testRunHandler(t, newMemoryStore(), newMemoryRunStore(), &fakeControl{})
	resp := request(t, handler, http.MethodPost, "/api/runs/"+uuid.NewString()+"/stop", "")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.Code)
	}
}

type memoryRunStore struct {
	runs                 []variation.Run
	children             map[uuid.UUID][]variation.ChildSession
	poolIPs              map[uuid.UUID][]variation.IPRow
	observations         map[uuid.UUID][]variation.IPObservation
	lastObservationLimit int
	streamErr            error
	runByIDErr           error
	renameRunErr         error
}

func newMemoryRunStore() *memoryRunStore {
	return &memoryRunStore{
		children:     map[uuid.UUID][]variation.ChildSession{},
		poolIPs:      map[uuid.UUID][]variation.IPRow{},
		observations: map[uuid.UUID][]variation.IPObservation{},
	}
}

func (m *memoryRunStore) StreamPoolIPs(_ context.Context, id uuid.UUID, visit func(variation.IPRow) error) error {
	if m.streamErr != nil {
		return m.streamErr
	}
	for _, row := range m.poolIPs[id] {
		if err := visit(row); err != nil {
			return err
		}
	}
	return nil
}

func (m *memoryRunStore) CreateRun(_ context.Context, run variation.Run, children []variation.ChildSession) error {
	m.runs = append(m.runs, run)
	m.children[run.ID] = children
	return nil
}
func (m *memoryRunStore) Runs(context.Context) ([]variation.RunSummary, error) {
	out := make([]variation.RunSummary, 0, len(m.runs))
	for _, r := range m.runs {
		out = append(out, variation.RunSummary{Run: r, VariantCount: len(m.children[r.ID]), Status: variation.RunRunning})
	}
	return out, nil
}
func (m *memoryRunStore) RunByID(_ context.Context, id uuid.UUID) (variation.RunSummary, error) {
	if m.runByIDErr != nil {
		return variation.RunSummary{}, m.runByIDErr
	}
	for _, r := range m.runs {
		if r.ID == id {
			return variation.RunSummary{Run: r, VariantCount: len(m.children[id]), Status: variation.RunRunning}, nil
		}
	}
	return variation.RunSummary{}, variation.ErrRunNotFound
}
func (m *memoryRunStore) RunSessions(_ context.Context, id uuid.UUID) ([]variation.VariantSession, error) {
	out := []variation.VariantSession{}
	for _, c := range m.children[id] {
		out = append(out, variation.VariantSession{SessionID: c.Session.ID, Name: c.Session.Name, Params: c.Params, CellKey: c.CellKey, Status: c.Session.Status})
	}
	return out, nil
}
func (m *memoryRunStore) RunIPObservations(_ context.Context, id uuid.UUID, limit int) ([]variation.IPObservation, error) {
	m.lastObservationLimit = limit
	obs := m.observations[id]
	if limit > 0 && len(obs) > limit {
		obs = obs[:limit]
	}
	return obs, nil
}

// RenameRun mirrors the real store: the run is renamed, and a child follows
// only while it still carries the name generated from the old run name.
func (m *memoryRunStore) RenameRun(_ context.Context, id uuid.UUID, name string) error {
	if m.renameRunErr != nil {
		return m.renameRunErr
	}
	for i := range m.runs {
		if m.runs[i].ID != id {
			continue
		}
		previous := m.runs[i].Name
		m.runs[i].Name = name
		children := m.children[id]
		for j := range children {
			var params map[string]string
			if len(children[j].Params) > 0 {
				if err := json.Unmarshal(children[j].Params, &params); err != nil {
					continue
				}
			}
			if children[j].Session.Name != variation.VariantName(previous, params) {
				continue
			}
			children[j].Session.Name = variation.VariantName(name, params)
		}
		return nil
	}
	return variation.ErrRunNotFound
}

func (m *memoryRunStore) DeleteRun(_ context.Context, id uuid.UUID) error {
	delete(m.children, id)
	for i, r := range m.runs {
		if r.ID == id {
			m.runs = append(m.runs[:i], m.runs[i+1:]...)
		}
	}
	return nil
}

func testRunHandler(t *testing.T, store session.Store, runStore variation.Store, control *fakeControl) http.Handler {
	return testRunHandlerWithCap(t, store, runStore, control, 128)
}

func testRunHandlerWithCap(t *testing.T, store session.Store, runStore variation.Store, control *fakeControl, cap int) http.Handler {
	t.Helper()
	cipher, err := cryptox.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, control, cipher, Defaults{ProbeTarget: testProbeTarget, DialTimeout: 10 * time.Second}, runStore, cap)
	return server.Handler()
}

func TestRunConfigReturnsConfiguredCap(t *testing.T) {
	handler := testRunHandlerWithCap(t, newMemoryStore(), newMemoryRunStore(), &fakeControl{}, 42)
	resp := request(t, handler, http.MethodGet, "/api/config", "")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `"max_variants_per_run":42`) {
		t.Fatalf("body = %s, want max_variants_per_run 42", resp.Body.String())
	}
}

func TestCreateRunRejectsMalformedJSON(t *testing.T) {
	cases := map[string]string{
		"unknown top-level field": `{"name":"r","template":"p://u-{c}:pw@g.example:1080","axes":{"c":{"kind":"list","values":["de"]}},"mode":"sticky","cadence_seconds":30,"max_sample":5}`,
		"null on non-nullable":    `{"name":"r","template":"p://u-{c}:pw@g.example:1080","axes":{"c":{"kind":"list","values":["de"]}},"mode":"sticky","cadence_seconds":30,"probes_per_sample":null}`,
		"unknown axis field":      `{"name":"r","template":"p://u-{c}:pw@g.example:1080","axes":{"c":{"kind":"list","value":["de"]}},"mode":"sticky","cadence_seconds":30}`,
		"trailing data":           `{"name":"r","template":"p://u-{c}:pw@g.example:1080","axes":{"c":{"kind":"list","values":["de"]}},"mode":"sticky","cadence_seconds":30}{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			handler := testRunHandler(t, newMemoryStore(), newMemoryRunStore(), &fakeControl{})
			resp := request(t, handler, http.MethodPost, "/api/runs", body)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
			}
		})
	}
}

func TestCreateRunRejectsCredentialInPasswordPosition(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	handler := testRunHandler(t, store, runStore, &fakeControl{})
	body := `{"name":"r","template":"http://user:{password}@proxy.example:8080","axes":{"password":{"kind":"list","values":["s3cret"]}},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (credential placeholder in password position)", resp.Code)
	}
	if len(runStore.runs) != 0 {
		t.Fatal("a run was created despite a credential-position axis")
	}
}

func TestCreateRunRejectsCredentialInjectedViaAxisValue(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	handler := testRunHandler(t, store, runStore, &fakeControl{})
	// {cred} sits in the username position (the raw template userinfo has no
	// colon), but the axis value injects a password via a colon.
	body := `{"name":"r","template":"http://{cred}@proxy.example:8080","axes":{"cred":{"kind":"list","values":["alice:s3cret"]}},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (axis value injects a credential)", resp.Code)
	}
	if len(runStore.runs) != 0 {
		t.Fatal("a run was created despite a delimiter-bearing axis value")
	}
}

func TestCreateRunAllowsTargetingPlaceholderInUsername(t *testing.T) {
	handler := testRunHandler(t, newMemoryStore(), newMemoryRunStore(), &fakeControl{})
	// {country} is in the username (targeting), the password 'pw' is fixed.
	body := `{"name":"r","template":"socks5h://u-cc-{country}:pw@gate.example:1080","axes":{"country":{"kind":"list","values":["de"]}},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (username targeting placeholder is allowed); body=%s", resp.Code, resp.Body.String())
	}
}

func TestCreateRunRejectsCredentialReconstructedViaEmptyAxis(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	handler := testRunHandler(t, store, runStore, &fakeControl{})
	// The raw template has no "://", so the old raw-string guard missed it, but
	// an empty {x} reconstructs "//" and puts {pw}'s value in the password.
	body := `{"name":"r","template":"socks5:/{x}/user:{pw}@host:1080","axes":{"x":{"kind":"list","values":[""]},"pw":{"kind":"list","values":["s3cr3t"]}},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (credential reconstructed into password); body=%s", resp.Code, resp.Body.String())
	}
	if len(runStore.runs) != 0 {
		t.Fatal("a run was created despite a credential in the rendered password")
	}
}

func TestCreateRunTolerantOfBenignStartTransition(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	// A concurrent Stop/Delete makes Start return ErrNotRunning — a benign race,
	// not a reason to roll back the whole run the user just created.
	control := &fakeControl{start: func(context.Context, uuid.UUID) error { return session.ErrNotRunning }}
	handler := testRunHandler(t, store, runStore, control)
	body := `{"name":"r","template":"socks5h://u-cc-{country}:pw@gate.example:1080","axes":{"country":{"kind":"list","values":["de","us"]}},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (benign concurrent transition); body=%s", resp.Code, resp.Body.String())
	}
	if len(runStore.runs) != 1 {
		t.Fatal("run was rolled back on a benign Start transition")
	}
}

func TestStopRunNilControlReturnsStructuredError(t *testing.T) {
	cipher, err := cryptox.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(newMemoryStore(), nil, cipher, Defaults{ProbeTarget: testProbeTarget, DialTimeout: 10 * time.Second}, newMemoryRunStore(), 128)
	handler := server.Handler()
	resp := request(t, handler, http.MethodPost, "/api/runs/"+uuid.NewString()+"/stop", "")
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.Code)
	}
	if !strings.Contains(resp.Body.String(), "internal_error") {
		t.Fatalf("body = %s, want structured internal_error (no panic)", resp.Body.String())
	}
}

func TestCreateRunRejectsOversizedTemplate(t *testing.T) {
	handler := testRunHandler(t, newMemoryStore(), newMemoryRunStore(), &fakeControl{})
	longTemplate := "socks5h://u:pw@gate.example:1080/" + strings.Repeat("a", 4100)
	body := `{"name":"r","template":"` + longTemplate + `","axes":{},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (template exceeds size cap); body=%s", resp.Code, resp.Body.String())
	}
}

func TestCreateRunRejectsRangeAxisMissingEndpoints(t *testing.T) {
	handler := testRunHandler(t, newMemoryStore(), newMemoryRunStore(), &fakeControl{})
	body := `{"name":"r","template":"p://gate:{port}","axes":{"port":{"kind":"range"}},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (range axis without from/to)", resp.Code)
	}
}

func TestCreateRunSucceedsWithoutPostStartRead(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	// A failing post-start read must not fail creation or leave a live run
	// behind a 500 — the response is built from the already-known run.
	runStore.runByIDErr = errors.New("transient read failure")
	control := &fakeControl{}
	handler := testRunHandler(t, store, runStore, control)

	body := `{"name":"r","template":"socks5h://u-cc-{country}:pw@gate.example:1080","axes":{"country":{"kind":"list","values":["de","us"]}},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (create must not depend on a post-start read); body=%s", resp.Code, resp.Body.String())
	}
	if control.startCount != 2 {
		t.Fatalf("Start calls = %d, want 2", control.startCount)
	}
}

func TestCreateRunKeepsRunWhenRollbackCleanupFails(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	control := &fakeControl{startErr: errors.New("start failed"), deleteErr: errors.New("delete failed")}
	handler := testRunHandler(t, store, runStore, control)

	body := `{"name":"r","template":"socks5h://u-cc-{country}:pw@gate.example:1080","axes":{"country":{"kind":"list","values":["de","us"]}},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)

	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.Code)
	}
	// Child cleanup failed, so the run must NOT be cascade-deleted out from
	// under a possibly-live worker; it stays durable and API-deletable.
	if len(runStore.runs) != 1 {
		t.Fatalf("runs = %d, want 1 (run kept when cleanup unconfirmed)", len(runStore.runs))
	}
}

func TestCreateRunRollsBackWhenChildStartFails(t *testing.T) {
	store := newMemoryStore()
	runStore := newMemoryRunStore()
	control := &fakeControl{startErr: errors.New("prepare worker failed")}
	handler := testRunHandler(t, store, runStore, control)

	body := `{"name":"r","template":"socks5h://u-cc-{country}:pw@gate.example:1080","axes":{"country":{"kind":"list","values":["de","us","fr"]}},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)

	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (rollback on start failure); body=%s", resp.Code, resp.Body.String())
	}
	if len(runStore.runs) != 0 {
		t.Fatalf("run not rolled back: %d runs remain", len(runStore.runs))
	}
	if control.deleteCount != 3 {
		t.Fatalf("Delete calls = %d, want 3 (every child cleaned up)", control.deleteCount)
	}
}
