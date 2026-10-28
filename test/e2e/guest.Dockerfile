# Base image for the e2e guest rootfs: Debian plus the tools the scenarios use.
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends iproute2 curl ca-certificates procps netcat-openbsd \
 && rm -rf /var/lib/apt/lists/*
