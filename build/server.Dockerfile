# differently-server image: control plane plus the CLI (used by `helm test`).
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETOS TARGETARCH VERSION=0.1.0-dev COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/different-ai/differently/internal/version.Version=${VERSION} -X github.com/different-ai/differently/internal/version.Commit=${COMMIT}" \
      -o /out/ ./cmd/differently-server ./cmd/differently

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/differently-server /out/differently /usr/local/bin/
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/differently-server"]
