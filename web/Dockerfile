# ----- deps -----
FROM node:22-alpine AS deps
WORKDIR /app
RUN corepack enable
COPY package.json pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

# ----- build -----
FROM node:22-alpine AS build
WORKDIR /app
RUN corepack enable
COPY package.json pnpm-lock.yaml ./
COPY --from=deps /app/node_modules ./node_modules
COPY . .
ENV NEXT_TELEMETRY_DISABLED=1
RUN pnpm build

# ----- runtime -----
FROM node:22-alpine AS runtime
WORKDIR /app
ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1

RUN addgroup -S -g 10001 appgroup \
    && adduser -S -u 10001 -G appgroup appuser

# standalone output: server.js + required node_modules (already pruned by Next.js)
COPY --from=build --chown=appuser:appgroup /app/.next/standalone ./
# static assets must be copied separately (not included in standalone dir)
COPY --from=build --chown=appuser:appgroup /app/.next/static ./.next/static
# public/ is optional in Next.js; this project has no public directory so the
# COPY is omitted. Add back if web/public/ is created later.

USER appuser
EXPOSE 3000
ENV HOSTNAME=0.0.0.0
ENV PORT=3000

CMD ["node", "server.js"]
