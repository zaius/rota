# Stage 1: Dependencies
# Run native to the build host (not the target arch): the JS build output is
# arch-independent, so we avoid emulating Node under QEMU.
FROM --platform=$BUILDPLATFORM node:20-alpine AS deps
WORKDIR /app

# Install pnpm
RUN corepack enable && corepack prepare pnpm@10.19.0 --activate

# Install dependencies based on the preferred package manager
COPY package.json pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

# Stage 2: Builder (also native to the build host — produces arch-independent JS)
FROM --platform=$BUILDPLATFORM node:20-alpine AS builder
WORKDIR /app

# Install pnpm
RUN corepack enable && corepack prepare pnpm@10.19.0 --activate

COPY --from=deps /app/node_modules ./node_modules
COPY . .

# Set environment variables for build
ENV NEXT_TELEMETRY_DISABLED=1
ENV NODE_ENV=production

# Bake a placeholder into the client bundle; docker-entrypoint.sh swaps in the
# real value from the runtime $API_URL at container start. Override this ARG only
# if you want to hard-code the URL at build time instead (legacy behaviour).
# Keep the default token in sync with PLACEHOLDER in docker-entrypoint.sh.
ARG NEXT_PUBLIC_API_URL=__RUNTIME_API_URL__
ENV NEXT_PUBLIC_API_URL=${NEXT_PUBLIC_API_URL}

# Build the application
RUN pnpm run build

# Stage 3: Runner
FROM node:20-alpine AS runner
WORKDIR /app

ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1

# Create a non-root user
RUN addgroup --system --gid 1001 nodejs
RUN adduser --system --uid 1001 nextjs

# Copy necessary files from builder
COPY --from=builder /app/public ./public
COPY --from=builder /app/.next/standalone ./
COPY --from=builder /app/.next/static ./.next/static

# Runtime config: injects $API_URL into the bundle before starting the server
COPY docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh

# Set correct permissions
RUN chown -R nextjs:nodejs /app

USER nextjs

EXPOSE 3000

ENV PORT=3000
ENV HOSTNAME="0.0.0.0"

# API_URL must be provided at runtime (the entrypoint fails fast if it's unset)
ENTRYPOINT ["/app/docker-entrypoint.sh"]
