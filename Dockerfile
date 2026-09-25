# syntax=docker/dockerfile:1
# All-in-one image: a single Go binary that serves the proxy (:8000), the REST
# API (:8001), AND the built dashboard SPA (on :8001, same origin as the API).
# There is no separate Node/Next runtime — the dashboard is a static Vite build
# the Go server serves via WEB_DIR.
#
# Build from the repo root:  docker build -t rota .

# The build stages run on the build host's own platform: the dashboard is
# platform-independent static files and Go cross-compiles, so a multi-arch
# build never runs Node or the Go toolchain under QEMU emulation.

# Stage 1: Build the dashboard (static SPA)
FROM --platform=$BUILDPLATFORM node:22-alpine AS dashboard-builder
WORKDIR /src
RUN corepack enable && corepack prepare pnpm@10.19.0 --activate
COPY dashboard/package.json dashboard/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY dashboard/ .
RUN pnpm run build

# Stage 2: Build the Go core
FROM --platform=$BUILDPLATFORM golang:1.26.8-alpine AS core-builder
RUN apk add --no-cache git ca-certificates tzdata
WORKDIR /src
COPY core/go.mod core/go.sum ./
RUN go mod download
COPY core/ .
ARG TARGETARCH
# The build context has no .git, so pass the version in, e.g.
#   --build-arg VERSION=$(git describe --tags --always --dirty)
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags="-w -s -extldflags '-static' -X github.com/alpkeskin/rota/core/internal/version.Version=${VERSION}" \
    -o /out/server \
    ./cmd/server/main.go

# Stage 3: Runner — just the static binary + the built SPA. No Node.
FROM alpine:3.24 AS runner
RUN apk --no-cache add ca-certificates tzdata wget
WORKDIR /app

COPY --from=core-builder /out/server /app/server
COPY --from=dashboard-builder /src/dist /app/web

# Serve the dashboard from /app/web on the API port (same origin as the API).
ENV WEB_DIR=/app/web

# /app/data holds downloaded GeoIP databases; creating it here lets a volume
# mounted there inherit the rota user's ownership.
RUN adduser -D -u 1000 rota && mkdir -p /app/data && chown -R rota:rota /app
USER rota

EXPOSE 8000 8001

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget --no-verbose --tries=1 -O /dev/null http://localhost:8001/health || exit 1

ENTRYPOINT ["/app/server"]
