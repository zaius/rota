# syntax=docker/dockerfile:1
# All-in-one image: a single Go binary that serves the proxy (:8000), the REST
# API (:8001), AND the built dashboard SPA (on :8001, same origin as the API).
# There is no separate Node/Next runtime — the dashboard is a static Vite build
# the Go server serves via WEB_DIR.
#
# Build from the repo root:  docker build -t rota .

# Stage 1: Build the dashboard (static SPA)
FROM node:20-alpine AS dashboard-builder
WORKDIR /src
RUN corepack enable && corepack prepare pnpm@10.19.0 --activate
COPY dashboard/package.json dashboard/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY dashboard/ .
RUN pnpm run build

# Stage 2: Build the Go core
FROM golang:1.25.3-alpine AS core-builder
RUN apk add --no-cache git ca-certificates tzdata
WORKDIR /src
COPY core/go.mod core/go.sum ./
RUN go mod download
COPY core/ .
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags='-w -s -extldflags "-static"' \
    -o /out/server \
    ./cmd/server/main.go

# Stage 3: Runner — just the static binary + the built SPA. No Node.
FROM alpine:3.20 AS runner
RUN apk --no-cache add ca-certificates tzdata wget
WORKDIR /app

COPY --from=core-builder /out/server /app/server
COPY --from=dashboard-builder /src/dist /app/web

# Serve the dashboard from /app/web on the API port (same origin as the API).
ENV WEB_DIR=/app/web

RUN adduser -D -u 1000 rota && chown -R rota:rota /app
USER rota

EXPOSE 8000 8001

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8001/health || exit 1

ENTRYPOINT ["/app/server"]
