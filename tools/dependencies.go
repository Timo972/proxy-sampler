//go:build tools

// Package tools pins dependencies that later implementation tasks consume.
package tools

import (
	_ "github.com/getkin/kin-openapi/openapi3"
	_ "github.com/go-chi/chi/v5"
	_ "github.com/google/uuid"
	_ "github.com/oapi-codegen/runtime"
	_ "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	_ "go.opentelemetry.io/otel"
	_ "golang.org/x/net/proxy"
	_ "golang.org/x/sync/semaphore"
)
