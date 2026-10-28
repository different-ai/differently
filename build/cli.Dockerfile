# differently CLI image with a shell, used by `helm test` and CI jobs.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETOS TARGETARCH VERSION=0.1.0-dev COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/different-ai/differently/internal/version.Version=${VERSION} -X github.com/different-ai/differently/internal/version.Commit=${COMMIT}" \
      -o /out/differently ./cmd/differently

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 65532 -g 65532 nonroot
COPY --from=build /out/differently /usr/local/bin/differently
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/differently"]
