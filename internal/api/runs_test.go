package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	cryptox "github.com/timo972/proxy-sampler/internal/crypto"
	"github.com/timo972/proxy-sampler/internal/session"
	"github.com/timo972/proxy-sampler/internal/variation"
)

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

func TestCreateRunRejectsUnmatchedAxis(t *testing.T) {
	handler := testRunHandler(t, newMemoryStore(), newMemoryRunStore(), &fakeControl{})
	body := `{"name":"x","template":"p://u:pw@gate.example:1080","axes":{"country":{"kind":"list","values":["de"]}},"mode":"sticky","cadence_seconds":30}`
	resp := request(t, handler, http.MethodPost, "/api/runs", body)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.Code)
	}
}

type memoryRunStore struct {
	runs     []variation.Run
	children map[uuid.UUID][]variation.ChildSession
}

func newMemoryRunStore() *memoryRunStore {
	return &memoryRunStore{children: map[uuid.UUID][]variation.ChildSession{}}
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
func (m *memoryRunStore) RunIPObservations(context.Context, uuid.UUID) ([]variation.IPObservation, error) {
	return nil, nil
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
