package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/session"
	"github.com/timo972/proxy-sampler/internal/variation"
)

func TestEditSessionUpdatesName(t *testing.T) {
	store := newMemoryStore()
	s := sampleSession()
	store.sessions = append(store.sessions, s)
	handler := testHandler(t, store, &fakeControl{})

	response := request(t, handler, http.MethodPatch, "/api/sessions/"+s.ID.String(), `{"name":"Frankfurt residential"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Name string `json:"name"`
	}
	decodeJSON(t, response, &body)
	if body.Name != "Frankfurt residential" {
		t.Errorf("response name = %q, want the new name", body.Name)
	}
	if store.sessions[0].Name != "Frankfurt residential" {
		t.Errorf("stored name = %q, want the new name", store.sessions[0].Name)
	}
}

func TestEditSessionTrimsSurroundingWhitespace(t *testing.T) {
	store := newMemoryStore()
	s := sampleSession()
	store.sessions = append(store.sessions, s)
	handler := testHandler(t, store, &fakeControl{})

	response := request(t, handler, http.MethodPatch, "/api/sessions/"+s.ID.String(), `{"name":"  Padded name  "}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if store.sessions[0].Name != "Padded name" {
		t.Errorf("stored name = %q, want the trimmed name", store.sessions[0].Name)
	}
}

func TestEditSessionRejectsInvalidNames(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty", `{"name":""}`},
		{"whitespace only", `{"name":"   "}`},
		{"over 100 runes", `{"name":"` + strings.Repeat("a", 101) + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMemoryStore()
			s := sampleSession()
			store.sessions = append(store.sessions, s)
			handler := testHandler(t, store, &fakeControl{})

			response := request(t, handler, http.MethodPatch, "/api/sessions/"+s.ID.String(), tt.body)
			assertAPIError(t, response, http.StatusBadRequest, "invalid_request")
			if store.sessions[0].Name != "Existing" {
				t.Errorf("stored name = %q, want it unchanged", store.sessions[0].Name)
			}
		})
	}
}

// TestEditSessionAcceptsExactly100Runes guards the boundary with multi-byte
// runes, so the limit stays a rune count rather than a byte length.
func TestEditSessionAcceptsExactly100Runes(t *testing.T) {
	store := newMemoryStore()
	s := sampleSession()
	store.sessions = append(store.sessions, s)
	handler := testHandler(t, store, &fakeControl{})

	name := strings.Repeat("ü", 100)
	response := request(t, handler, http.MethodPatch, "/api/sessions/"+s.ID.String(), `{"name":"`+name+`"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if store.sessions[0].Name != name {
		t.Errorf("stored name = %q, want the 100-rune name", store.sessions[0].Name)
	}
}

// TestEditSessionRejectsEmptyPayload covers the extensible request shape: every
// property is optional, so a payload that sets nothing has to be refused rather
// than silently succeeding as a no-op.
func TestEditSessionRejectsEmptyPayload(t *testing.T) {
	store := newMemoryStore()
	s := sampleSession()
	store.sessions = append(store.sessions, s)
	handler := testHandler(t, store, &fakeControl{})

	response := request(t, handler, http.MethodPatch, "/api/sessions/"+s.ID.String(), `{}`)
	assertAPIError(t, response, http.StatusBadRequest, "invalid_request")
	if store.sessions[0].Name != "Existing" {
		t.Errorf("stored name = %q, want it unchanged", store.sessions[0].Name)
	}
}

func TestEditSessionNotFound(t *testing.T) {
	store := newMemoryStore()
	handler := testHandler(t, store, &fakeControl{})

	response := request(t, handler, http.MethodPatch, "/api/sessions/"+uuid.New().String(), `{"name":"Anything"}`)
	assertAPIError(t, response, http.StatusNotFound, "not_found")
}

func TestEditRunUpdatesName(t *testing.T) {
	runStore := newMemoryRunStore()
	run := seedRun(runStore, "Alpha", map[string]string{"region": "eu"}, map[string]string{"region": "us"})
	handler := testRunHandler(t, newMemoryStore(), runStore, &fakeControl{})

	response := request(t, handler, http.MethodPatch, "/api/runs/"+run.String(), `{"name":"Beta"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Name string `json:"name"`
	}
	decodeJSON(t, response, &body)
	if body.Name != "Beta" {
		t.Errorf("response name = %q, want the new name", body.Name)
	}
	if runStore.runs[0].Name != "Beta" {
		t.Errorf("stored run name = %q, want the new name", runStore.runs[0].Name)
	}
}

// TestEditRunRenamesOnlyGeneratedVariantNames is the cascade rule: a generated
// child follows the rename, and a child renamed by hand keeps the name its user
// chose. Which is which comes from recorded provenance, not from the name's text.
func TestEditRunRenamesOnlyGeneratedVariantNames(t *testing.T) {
	runStore := newMemoryRunStore()
	run := seedRun(runStore, "Alpha", map[string]string{"region": "eu"}, map[string]string{"region": "us"})
	children := runStore.children[run]
	children[1].Session.Name = "My custom probe"
	runStore.customized[children[1].Session.ID] = true
	handler := testRunHandler(t, newMemoryStore(), runStore, &fakeControl{})

	response := request(t, handler, http.MethodPatch, "/api/runs/"+run.String(), `{"name":"Beta"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	updated := runStore.children[run]
	if updated[0].Session.Name != "Beta (region=eu)" {
		t.Errorf("generated variant name = %q, want it to follow the run rename", updated[0].Session.Name)
	}
	if updated[1].Session.Name != "My custom probe" {
		t.Errorf("hand-renamed variant = %q, want it preserved", updated[1].Session.Name)
	}
}

func TestEditRunRejectsInvalidNames(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty", `{"name":""}`},
		{"whitespace only", `{"name":"   "}`},
		{"over 100 runes", `{"name":"` + strings.Repeat("a", 101) + `"}`},
		{"no editable property", `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runStore := newMemoryRunStore()
			run := seedRun(runStore, "Alpha", map[string]string{"region": "eu"})
			handler := testRunHandler(t, newMemoryStore(), runStore, &fakeControl{})

			response := request(t, handler, http.MethodPatch, "/api/runs/"+run.String(), tt.body)
			assertAPIError(t, response, http.StatusBadRequest, "invalid_request")
			if runStore.runs[0].Name != "Alpha" {
				t.Errorf("stored run name = %q, want it unchanged", runStore.runs[0].Name)
			}
		})
	}
}

func TestEditRunTrimsSurroundingWhitespace(t *testing.T) {
	runStore := newMemoryRunStore()
	run := seedRun(runStore, "Alpha", map[string]string{"region": "eu"})
	handler := testRunHandler(t, newMemoryStore(), runStore, &fakeControl{})

	response := request(t, handler, http.MethodPatch, "/api/runs/"+run.String(), `{"name":"  Beta  "}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if runStore.runs[0].Name != "Beta" {
		t.Errorf("stored run name = %q, want the trimmed name", runStore.runs[0].Name)
	}
	if got := runStore.children[run][0].Session.Name; got != "Beta (region=eu)" {
		t.Errorf("variant name = %q, want it derived from the trimmed name", got)
	}
}

func TestEditRunNotFound(t *testing.T) {
	handler := testRunHandler(t, newMemoryStore(), newMemoryRunStore(), &fakeControl{})

	response := request(t, handler, http.MethodPatch, "/api/runs/"+uuid.New().String(), `{"name":"Beta"}`)
	assertAPIError(t, response, http.StatusNotFound, "not_found")
}

// seedRun adds a run whose children carry the names CreateRun would have
// generated for the given params, so rename tests start from a realistic state.
func seedRun(store *memoryRunStore, name string, params ...map[string]string) uuid.UUID {
	id := uuid.New()
	store.runs = append(store.runs, variation.Run{ID: id, Name: name})
	children := make([]variation.ChildSession, 0, len(params))
	for _, p := range params {
		raw, err := json.Marshal(p)
		if err != nil {
			panic(err)
		}
		children = append(children, variation.ChildSession{
			Session: session.Session{ID: uuid.New(), Name: variation.VariantName(name, p)},
			Params:  raw,
		})
	}
	store.children[id] = children
	return id
}

// TestEditRejectsOversizedBody covers the body cap on both edit routes: the
// request is refused during the read, before the generated decoder buffers it.
func TestEditRejectsOversizedBody(t *testing.T) {
	const bodyLimit = 1 << 20
	// The payload itself is valid and would rename successfully: a short name
	// followed by trailing whitespace, which JSON permits. Only the body cap can
	// reject it, so a 400 here cannot come from name-length validation instead.
	oversized := `{"name":"Beta"}` + strings.Repeat(" ", bodyLimit)

	t.Run("session", func(t *testing.T) {
		store := newMemoryStore()
		s := sampleSession()
		store.sessions = append(store.sessions, s)
		handler := testHandler(t, store, &fakeControl{})

		response := request(t, handler, http.MethodPatch, "/api/sessions/"+s.ID.String(), oversized)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", response.Code)
		}
		if store.sessions[0].Name != "Existing" {
			t.Errorf("stored name = %q, want it unchanged", store.sessions[0].Name)
		}
	})

	t.Run("run", func(t *testing.T) {
		runStore := newMemoryRunStore()
		run := seedRun(runStore, "Alpha", map[string]string{"region": "eu"})
		handler := testRunHandler(t, newMemoryStore(), runStore, &fakeControl{})

		response := request(t, handler, http.MethodPatch, "/api/runs/"+run.String(), oversized)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", response.Code)
		}
		if runStore.runs[0].Name != "Alpha" {
			t.Errorf("stored run name = %q, want it unchanged", runStore.runs[0].Name)
		}
	})
}

// TestEditSessionRejectsRuntimeSchemaViolations mirrors the create endpoints:
// the generated decoder silently drops unknown fields and trailing data despite
// additionalProperties:false, so the edit payload is validated explicitly.
func TestEditSessionRejectsRuntimeSchemaViolations(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown property", body: `{"name":"Beta","unexpected":true}`},
		{name: "typoed property", body: `{"name":"Beta","nmae":"ignored"}`},
		{name: "null name", body: `{"name":null}`},
		{name: "trailing JSON", body: `{"name":"Beta"} {}`},
		{name: "concatenated objects", body: `{"name":"Beta"}{"name":"Gamma"}`},
		{name: "JSON array", body: `[{"name":"Beta"}]`},
		{name: "JSON null", body: `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMemoryStore()
			s := sampleSession()
			store.sessions = append(store.sessions, s)
			handler := testHandler(t, store, &fakeControl{})

			response := request(t, handler, http.MethodPatch, "/api/sessions/"+s.ID.String(), tt.body)
			assertAPIError(t, response, http.StatusBadRequest, "invalid_request")
			if store.sessions[0].Name != "Existing" {
				t.Errorf("stored name = %q, want it unchanged", store.sessions[0].Name)
			}
		})
	}
}

func TestEditRunRejectsRuntimeSchemaViolations(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown property", body: `{"name":"Beta","unexpected":true}`},
		{name: "typoed property", body: `{"name":"Beta","nmae":"ignored"}`},
		{name: "null name", body: `{"name":null}`},
		{name: "trailing JSON", body: `{"name":"Beta"} {}`},
		{name: "concatenated objects", body: `{"name":"Beta"}{"name":"Gamma"}`},
		{name: "JSON array", body: `[{"name":"Beta"}]`},
		{name: "JSON null", body: `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runStore := newMemoryRunStore()
			run := seedRun(runStore, "Alpha", map[string]string{"region": "eu"})
			handler := testRunHandler(t, newMemoryStore(), runStore, &fakeControl{})

			response := request(t, handler, http.MethodPatch, "/api/runs/"+run.String(), tt.body)
			assertAPIError(t, response, http.StatusBadRequest, "invalid_request")
			if runStore.runs[0].Name != "Alpha" {
				t.Errorf("stored run name = %q, want it unchanged", runStore.runs[0].Name)
			}
			if got := runStore.children[run][0].Session.Name; got != "Alpha (region=eu)" {
				t.Errorf("variant name = %q, want it unchanged", got)
			}
		})
	}
}
