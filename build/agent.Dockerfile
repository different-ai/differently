# differently-agent image: node agent, Firecracker and the networking tools it
# drives. Also carries differently-guest for the guest image build scripts.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETOS TARGETARCH VERSION=0.1.0-dev COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/different-ai/differently/internal/version.Version=${VERSION} -X github.com/different-ai/differently/internal/version.Commit=${COMMIT}" \
      -o /out/ ./cmd/differently-agent ./cmd/differently-guest

FROM debian:bookworm-slim AS firecracker
ARG TARGETARCH FIRECRACKER_VERSION=v1.17.0
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    arch=$([ "$TARGETARCH" = "arm64" ] && echo aarch64 || echo x86_64); \
    base="https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}"; \
    tgz="firecracker-${FIRECRACKER_VERSION}-${arch}.tgz"; \
    curl -fsSLO "$base/$tgz"; curl -fsSLO "$base/$tgz.sha256.txt"; \
    sha256sum -c "$tgz.sha256.txt"; \
    tar -xzf "$tgz"; \
    install -m 0755 "release-${FIRECRACKER_VERSION}-${arch}/firecracker-${FIRECRACKER_VERSION}-${arch}" /firecracker; \
    install -m 0755 "release-${FIRECRACKER_VERSION}-${arch}/jailer-${FIRECRACKER_VERSION}-${arch}" /jailer

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends iproute2 iptables e2fsprogs procps ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=firecracker /firecracker /jailer /usr/local/bin/
COPY --from=build /out/differently-agent /usr/local/bin/
COPY --from=build /out/differently-guest /usr/local/share/differently/differently-guest
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/differently-agent"]
