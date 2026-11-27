# differently-server image: control plane plus the CLI (used by `helm test`).
# The published image is built with no Go build tags. To include the
# experimental adapter kinds (docs/providers/README.md):
#   docker build -f build/server.Dockerfile --build-arg GO_TAGS=experimental_adapters .
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETOS TARGETARCH VERSION=0.1.0-dev COMMIT=unknown GO_TAGS=""
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -tags "${GO_TAGS}" \
      -ldflags "-s -w -X github.com/different-ai/differently/internal/version.Version=${VERSION} -X github.com/different-ai/differently/internal/version.Commit=${COMMIT}" \
      -o /out/ ./cmd/differently-server ./cmd/differently
# The default data directory, empty. The final image has no shell to make it.
RUN mkdir /data-dir

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/differently-server /out/differently /usr/local/bin/
# The server's user owns its data directory, so the default store (files
# under dataDir) can be written. Mount a volume here to keep it when the
# container is replaced; a new named volume takes this ownership.
COPY --from=build --chown=65532:65532 /data-dir /var/lib/differently
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/differently-server"]
