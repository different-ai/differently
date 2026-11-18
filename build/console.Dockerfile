# differently-console image: the admin console in server mode, with sign-in
# (Better Auth) and a proxy that calls Differently with a key only the server
# holds. See console/README.md for the settings; the Helm chart sets them
# with console.enabled=true.
FROM --platform=$BUILDPLATFORM node:24-bookworm-slim AS build
WORKDIR /src
# The console compiles the TypeScript SDK from source.
COPY sdk/typescript/src sdk/typescript/src
COPY console/package.json console/package-lock.json console/
RUN cd console && npm ci --no-audit --no-fund
COPY console console
RUN cd console && npm run build:server

# Runtime dependencies only, installed for the target platform.
FROM node:24-bookworm-slim AS deps
WORKDIR /app
COPY console/package.json console/package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund

FROM node:24-bookworm-slim
ENV NODE_ENV=production CONSOLE_MODE=server HOST=0.0.0.0 PORT=4321 CONSOLE_DB_PATH=/data/console.db
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY --from=deps /app/package.json ./
COPY --from=build /src/console/dist ./dist
# Users and sessions live in /data/console.db; mount a volume there.
RUN mkdir /data && chown 65532:65532 /data
USER 65532:65532
EXPOSE 4321
ENTRYPOINT ["node", "dist/server/entry.mjs"]
