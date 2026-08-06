package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	chimw "github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	webassets "github.com/timo972/proxy-sampler/web"
)

func TestSPAHandlerServesIndexAssetsAndDeepLinks(t *testing.T) {
	handler := testRouter(t, testAssets(), NewServer(nil, nil, nil, Defaults{}, nil, 128).Handler(), discardRouterLogger())

	tests := []struct {
		name         string
		path         string
		status       int
		contentType  string
		cacheControl string
		body         string
	}{
		{name: "root", path: "/", status: http.StatusOK, contentType: "text/html; charset=utf-8", cacheControl: "no-cache", body: `<div id="root">`},
		{name: "deep link", path: "/sessions/123e4567-e89b-12d3-a456-426614174000", status: http.StatusOK, contentType: "text/html; charset=utf-8", cacheControl: "no-cache", body: `<div id="root">`},
		{name: "javascript", path: "/assets/index-abc12345.js", status: http.StatusOK, contentType: "text/javascript; charset=utf-8", cacheControl: "public, max-age=31536000, immutable", body: `console.log("ok")`},
		{name: "stylesheet", path: "/assets/index-def67890.css", status: http.StatusOK, contentType: "text/css; charset=utf-8", cacheControl: "public, max-age=31536000, immutable", body: `body{color:black}`},
		{name: "missing asset", path: "/assets/missing.js", status: http.StatusNotFound},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := serveRouterRequest(handler, test.path)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, test.status, response.Body.String())
			}
			if test.contentType != "" && response.Header().Get("Content-Type") != test.contentType {
				t.Fatalf("Content-Type = %q, want %q", response.Header().Get("Content-Type"), test.contentType)
			}
			if test.cacheControl != "" && response.Header().Get("Cache-Control") != test.cacheControl {
				t.Fatalf("Cache-Control = %q, want %q", response.Header().Get("Cache-Control"), test.cacheControl)
			}
			if test.body != "" && !strings.Contains(response.Body.String(), test.body) {
				t.Fatalf("body = %q, want substring %q", response.Body.String(), test.body)
			}
			if test.status == http.StatusNotFound && strings.Contains(response.Body.String(), `<div id="root">`) {
				t.Fatal("missing asset fell back to SPA index")
			}
		})
	}
}

func TestSPAHandlerRejectsTraversalInsteadOfFallingBack(t *testing.T) {
	handler := testRouter(t, testAssets(), NewServer(nil, nil, nil, Defaults{}, nil, 128).Handler(), discardRouterLogger())
	for _, target := range []string{
		"http://example.com/../go.mod",
		"http://example.com/%2e%2e/go.mod",
		"http://example.com/assets/%2e%2e/index.html",
		"http://example.com/assets/..%2findex.html",
	} {
		t.Run(target, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, target, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %q", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), `<div id="root">`) {
				t.Fatal("traversal request received SPA index")
			}
		})
	}
}

func TestRouterKeepsUnknownAPIRoutesJSON(t *testing.T) {
	handler := testRouter(t, testAssets(), NewServer(nil, nil, nil, Defaults{}, nil, 128).Handler(), discardRouterLogger())
	response := serveRouterRequest(handler, "/api/not-a-route")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode JSON 404: %v; body = %q", err, response.Body.String())
	}
	if body["code"] != "not_found" || strings.Contains(response.Body.String(), `<div id="root">`) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestRouterAddsRequestContextAndStructuredCredentialSafeLogs(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "%s|%s", chimw.GetReqID(r.Context()), r.RemoteAddr)
	})
	handler := testRouter(t, testAssets(), apiHandler, logger)
	request := httptest.NewRequest(http.MethodGet, "http://example.com/healthz?token=top-secret", nil)
	request.Header.Set("Authorization", "Bearer hidden-password")
	request.Header.Set("X-Forwarded-For", "203.0.113.7")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	parts := strings.Split(response.Body.String(), "|")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "203.0.113.7" {
		t.Fatalf("request context body = %q", response.Body.String())
	}
	logged := logs.String()
	for _, secret := range []string{"top-secret", "hidden-password", "Authorization", "token="} {
		if strings.Contains(logged, secret) {
			t.Fatalf("request log leaked %q: %s", secret, logged)
		}
	}
	for _, field := range []string{`"method":"GET"`, `"path":"/healthz"`, `"status":200`, `"request_id":"`} {
		if !strings.Contains(logged, field) {
			t.Fatalf("request log missing %q: %s", field, logged)
		}
	}
}

func TestRouterRecoversPanicsAndLogsServerError(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	apiHandler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	handler := testRouter(t, testAssets(), apiHandler, logger)
	response := serveRouterRequest(handler, "/healthz")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	if !strings.Contains(logs.String(), `"status":500`) {
		t.Fatalf("request log = %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"msg":"http panic recovered"`) {
		t.Fatalf("panic log is not structured: %s", logs.String())
	}
	if strings.Contains(logs.String(), "boom") {
		t.Fatalf("panic value leaked into logs: %s", logs.String())
	}
}

func TestCommittedDistAssetsAreServedWithImmutableCaching(t *testing.T) {
	handler := testRouter(t, webassets.FS, NewServer(nil, nil, nil, Defaults{}, nil, 128).Handler(), discardRouterLogger())
	entries, err := fs.ReadDir(webassets.FS, "dist/assets")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("embedded dist/assets is empty")
	}
	for _, entry := range entries {
		response := serveRouterRequest(handler, "/assets/"+entry.Name())
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d", entry.Name(), response.Code)
		}
		if response.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Fatalf("%s Cache-Control = %q", entry.Name(), response.Header().Get("Cache-Control"))
		}
		if response.Header().Get("Content-Type") == "" {
			t.Fatalf("%s has no Content-Type", entry.Name())
		}
	}
}

func TestRouterUsesFinalOTelHTTPWrapper(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	original := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(original)
	})

	handler := testRouter(t, testAssets(), NewServer(nil, nil, nil, Defaults{}, nil, 128).Handler(), discardRouterLogger())
	response := serveRouterRequest(handler, "/")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if spans := recorder.Ended(); len(spans) != 1 {
		t.Fatalf("ended spans = %d, want one server span", len(spans))
	}
}

func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"dist/index.html":                {Data: []byte(`<!doctype html><div id="root"></div>`)},
		"dist/assets/index-abc12345.js":  {Data: []byte(`console.log("ok")`)},
		"dist/assets/index-def67890.css": {Data: []byte(`body{color:black}`)},
	}
}

func testRouter(t *testing.T, assets fs.FS, apiHandler http.Handler, logger *slog.Logger) http.Handler {
	t.Helper()
	handler, err := NewRouter(apiHandler, assets, logger)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func serveRouterRequest(handler http.Handler, target string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func discardRouterLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}
