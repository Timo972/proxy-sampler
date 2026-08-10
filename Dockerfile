# Builder stages run on the build host's native architecture; only the Go
# compile targets TARGETOS/TARGETARCH, so multi-platform builds need no
# emulation for the expensive npm/go steps.
FROM --platform=$BUILDPLATFORM node:24-bookworm-slim AS web-builder
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26 AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-builder /src/web/dist ./web/dist
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/proxy-sampler ./cmd/app

FROM debian:bookworm-slim
# The nonroot account matches the numeric USER below. Without it the runtime UID
# resolves to no account at all, which breaks every os/user lookup in the
# process -- including the OpenTelemetry process.owner resource detector.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 nonroot \
    && useradd --system --uid 65532 --gid 65532 \
        --home-dir /home/nonroot --create-home --shell /usr/sbin/nologin nonroot
COPY --from=go-builder /out/proxy-sampler /usr/local/bin/proxy-sampler
ENV HTTP_ADDR=:8080
EXPOSE 8080
# Numeric so Kubernetes runAsNonRoot can verify it without resolving the name.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/proxy-sampler"]
