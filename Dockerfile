# syntax=docker/dockerfile:1
# All-in-one image: Go core (proxy :8000, API :8001) + Next.js dashboard (:3000)
# Build from the repo root:  docker build -t rota:all-in-one .

# Stage 1: Core builder
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

# Stage 2: Dashboard deps
FROM node:20-alpine AS dashboard-deps
WORKDIR /src

RUN corepack enable && corepack prepare pnpm@10.19.0 --activate

COPY dashboard/package.json dashboard/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

# Stage 3: Dashboard builder
FROM node:20-alpine AS dashboard-builder
WORKDIR /src

RUN corepack enable && corepack prepare pnpm@10.19.0 --activate

COPY --from=dashboard-deps /src/node_modules ./node_modules
COPY dashboard/ .

ENV NEXT_TELEMETRY_DISABLED=1
ENV NODE_ENV=production

# Baked into the client bundle at build time — must be reachable from the
# user's browser, not from inside the container.
ARG NEXT_PUBLIC_API_URL=http://localhost:8001
ENV NEXT_PUBLIC_API_URL=${NEXT_PUBLIC_API_URL}

RUN pnpm run build

# Stage 4: Runner (node base so the dashboard has a runtime; core is static)
FROM node:20-alpine AS runner

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1
ENV PORT=3000
ENV HOSTNAME=0.0.0.0

COPY --from=core-builder /out/server /app/server

COPY --from=dashboard-builder /src/public /app/dashboard/public
COPY --from=dashboard-builder /src/.next/standalone /app/dashboard
COPY --from=dashboard-builder /src/.next/static /app/dashboard/.next/static

COPY <<'EOF' /app/start.sh
#!/bin/sh
/app/server &
core=$!
node /app/dashboard/server.js &
dash=$!

trap 'kill -TERM $core $dash 2>/dev/null' TERM INT

# Exit when either process dies so the container restart policy kicks in
while kill -0 $core 2>/dev/null && kill -0 $dash 2>/dev/null; do
    sleep 1
done
kill -TERM $core $dash 2>/dev/null
wait $core $dash
exit 1
EOF

RUN chmod +x /app/start.sh && chown -R node:node /app

USER node

EXPOSE 3000 8000 8001

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8001/health && \
        wget --no-verbose --tries=1 --spider http://localhost:3000/ || exit 1

ENTRYPOINT ["/app/start.sh"]
