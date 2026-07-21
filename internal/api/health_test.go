package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHealthzIsLiveWithoutCheckingDependencies(t *testing.T) {
	server := NewServer(nil, nil, nil, Defaults{})
	response := request(t, server.Handler(), http.MethodGet, "/healthz", "")
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("response = %d %s, want 200 status ok", response.Code, response.Body.String())
	}
}

func TestReadyzPingsPostgresAndClickHouseConcurrently(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	store := &reportStore{memoryStore: newMemoryStore(), ping: func(context.Context) error {
		entered <- "postgres"
		<-release
		return nil
	}}
	reader := &fakeReportReader{ping: func(context.Context) error {
		entered <- "clickhouse"
		<-release
		return nil
	}}
	server := reportTestServer(t, store, reader)
	done := make(chan *responseResult, 1)
	go func() {
		response := request(t, server.Handler(), http.MethodGet, "/readyz", "")
		done <- &responseResult{status: response.Code, body: response.Body.String()}
	}()

	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case dependency := <-entered:
			seen[dependency] = true
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("dependency pings did not start concurrently; entered=%v", seen)
		}
	}
	close(release)
	result := <-done
	if result.status != http.StatusOK || strings.TrimSpace(result.body) != `{"clickhouse":true,"postgres":true}` {
		t.Fatalf("response = %d %s, want both ready", result.status, result.body)
	}
}

func TestReadyzReturnsOnlyDependencyBooleansOnFailure(t *testing.T) {
	store := &reportStore{memoryStore: newMemoryStore(), ping: func(context.Context) error {
		return errors.New("postgres://admin:password@db.example/proxy")
	}}
	reader := &fakeReportReader{}
	response := request(t, reportTestServer(t, store, reader).Handler(), http.MethodGet, "/readyz", "")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", response.Code, response.Body.String())
	}
	if strings.TrimSpace(response.Body.String()) != `{"clickhouse":true,"postgres":false}` {
		t.Errorf("body = %s, want dependency booleans only", response.Body.String())
	}
	for _, secret := range []string{"admin", "password", "db.example", "proxy"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Errorf("readiness leaked %q: %s", secret, response.Body.String())
		}
	}
}

func TestReadyzAppliesSharedTwoSecondTimeout(t *testing.T) {
	deadlines := make(chan time.Duration, 2)
	blocked := func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			return errors.New("missing deadline")
		}
		deadlines <- time.Until(deadline)
		<-ctx.Done()
		return ctx.Err()
	}
	store := &reportStore{memoryStore: newMemoryStore(), ping: blocked}
	reader := &fakeReportReader{ping: blocked}
	started := time.Now()
	response := request(t, reportTestServer(t, store, reader).Handler(), http.MethodGet, "/readyz", "")
	elapsed := time.Since(started)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", response.Code, response.Body.String())
	}
	if elapsed < 1800*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("readiness elapsed = %s, want shared 2s timeout", elapsed)
	}
	for i := 0; i < 2; i++ {
		remaining := <-deadlines
		if remaining < 1800*time.Millisecond || remaining > 2100*time.Millisecond {
			t.Errorf("dependency deadline remaining = %s, want approximately 2s", remaining)
		}
	}
}

type responseResult struct {
	status int
	body   string
}
