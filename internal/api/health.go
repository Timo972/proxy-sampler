package api

import (
	"context"
	"time"

	"github.com/timo972/proxy-sampler/internal/api/openapi"
)

const readinessTimeout = 2 * time.Second

type dependencyPinger interface {
	Ping(context.Context) error
}

// Healthz reports process liveness without touching external dependencies.
func (s *Server) Healthz(context.Context, openapi.HealthzRequestObject) (openapi.HealthzResponseObject, error) {
	return openapi.Healthz200JSONResponse{Status: openapi.HealthStatusOk}, nil
}

// Readyz checks Postgres and ClickHouse concurrently under one timeout.
func (s *Server) Readyz(ctx context.Context, _ openapi.ReadyzRequestObject) (openapi.ReadyzResponseObject, error) {
	ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()

	type result struct {
		postgres bool
		ready    bool
	}
	results := make(chan result, 2)
	postgres, _ := s.store.(dependencyPinger)
	check := func(pinger dependencyPinger, isPostgres bool) {
		ready := pinger != nil && pinger.Ping(ctx) == nil
		results <- result{postgres: isPostgres, ready: ready}
	}
	go check(postgres, true)
	go check(s.reader, false)

	response := openapi.Readiness{}
	for completed := 0; completed < 2; completed++ {
		select {
		case value := <-results:
			if value.postgres {
				response.Postgres = value.ready
			} else {
				response.Clickhouse = value.ready
			}
		case <-ctx.Done():
			return openapi.Readyz503JSONResponse(response), nil
		}
	}
	if response.Postgres && response.Clickhouse {
		return openapi.Readyz200JSONResponse(response), nil
	}
	return openapi.Readyz503JSONResponse(response), nil
}
