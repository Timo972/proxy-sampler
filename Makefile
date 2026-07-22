BINARY := bin/proxy-sampler
SQLC := go tool sqlc
OAPI := go tool oapi-codegen
GOOSE := go tool goose

.PHONY: build test go-test test-race fmt vet generate generate-check sqlc-generate openapi-generate web-install web-test web-build migrate-up ch-migrate-up integration-test docker-build

build: web-build generate
	go build -o $(BINARY) ./cmd/app

test: go-test web-test

go-test:
	go test ./...

test-race:
	go test -race ./...

fmt:
	gofmt -w $$(find cmd internal migrations -type f -name '*.go') web/embed.go

vet:
	go vet ./...

sqlc-generate:
	$(SQLC) generate

openapi-generate:
	$(OAPI) -config oapi-codegen.yaml api/openapi.yaml

generate: sqlc-generate openapi-generate

generate-check: generate
	git diff --exit-code

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

integration-test:
	go test -count=1 -race ./internal/migrate ./internal/chmigrate ./internal/db ./internal/ch -v

docker-build:
	docker build -t proxy-sampler:local .
