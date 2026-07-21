BINARY := bin/proxy-sampler
SQLC := go tool sqlc
OAPI := go tool oapi-codegen
GOOSE := go tool goose

.PHONY: build test generate sqlc-generate openapi-generate web-install web-test web-build migrate-up ch-migrate-up

build: web-build generate
	go build -o $(BINARY) ./cmd/app

test:
	go test ./...
	cd web && npm test -- --run

sqlc-generate:
	$(SQLC) generate

openapi-generate:
	$(OAPI) -config oapi-codegen.yaml api/openapi.yaml

generate: sqlc-generate openapi-generate

web-install:
	cd web && npm ci

web-test:
	cd web && npm test -- --run

web-build:
	cd web && npm run build

migrate-up:
	$(GOOSE) -dir migrations/postgres postgres "$${DATABASE_URL}" up

ch-migrate-up:
	$(GOOSE) -dir migrations/clickhouse clickhouse "$${CLICKHOUSE_DSN}" up
