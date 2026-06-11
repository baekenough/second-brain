# ----- deps -----
# Install all dependencies using the frozen lockfile.
# This layer is invalidated only when bun.lock or package.json change.
FROM oven/bun:1.3-alpine AS deps
WORKDIR /app
COPY package.json bun.lock ./
RUN bun install --frozen-lockfile

# ----- build -----
# Copy source and produce the Next.js standalone output.
FROM oven/bun:1.3-alpine AS build
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
ENV NEXT_TELEMETRY_DISABLED=1
RUN bun run build

# ----- runtime -----
# Minimal runtime image: only the standalone server artefact is included.
# next.config.ts sets output: "standalone", so .next/standalone is a
# self-contained directory with server.js + pruned node_modules.
# .next/static must be copied separately (Next.js intentionally excludes it).
# public/ is omitted — this project has no web/public/ directory.
FROM oven/bun:1.3-alpine AS runtime
WORKDIR /app

ENV NODE_ENV=production \
    NEXT_TELEMETRY_DISABLED=1 \
    HOSTNAME=0.0.0.0 \
    PORT=3000

# Create non-root user (uid/gid 10001) for security.
RUN addgroup -S -g 10001 appgroup \
    && adduser -S -u 10001 -G appgroup appuser

# Copy standalone server (includes pruned node_modules) and static assets.
COPY --from=build --chown=appuser:appgroup /app/.next/standalone ./
COPY --from=build --chown=appuser:appgroup /app/.next/static ./.next/static

USER appuser
EXPOSE 3000

# Next.js standalone server.js is plain Node-compatible and runs fine with bun.
CMD ["bun", "server.js"]
