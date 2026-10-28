#!/usr/bin/env bash
# Build everything the e2e stack needs on the current Docker host:
#   - the server and agent images (native architecture),
#   - a guest kernel (Firecracker's CI build) and a Debian rootfs with the
#     guest agent, baked into an e2e agent image.
#
# Usage: DOCKER_CONTEXT=colima-differently test/e2e/prepare.sh
# Set KERNEL_URL to use your own kernel instead of Firecracker's CI one.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
art="$here/.artifacts"
mkdir -p "$art/images" "$art/tmp"
# Bind mounts must come from a directory the Docker VM can see (Colima
# shares $HOME), so keep temporary files inside the repository.
export TMPDIR="$art/tmp"

arch=$(docker info --format '{{.Architecture}}')
case "$arch" in
  aarch64|arm64) goarch=arm64; fcarch=aarch64 ;;
  x86_64|amd64) goarch=amd64; fcarch=x86_64 ;;
  *) echo "unsupported Docker host architecture $arch" >&2; exit 1 ;;
esac
echo "==> Docker host: $(docker context show) ($arch)"

if ! docker run --rm --device /dev/kvm busybox:1.37 test -c /dev/kvm 2>/dev/null; then
  echo "the Docker host has no /dev/kvm; on Apple Silicon M3 or newer start Colima with" >&2
  echo "  colima start differently --vm-type vz --nested-virtualization --cpu 4 --memory 8" >&2
  exit 1
fi

echo "==> Building images"
docker build -q -f "$repo/build/server.Dockerfile" -t differently-server:e2e "$repo" >/dev/null
docker build -q -f "$repo/build/agent.Dockerfile" -t differently-agent:e2e "$repo" >/dev/null
docker build -q -f "$here/guest.Dockerfile" -t differently-guest-base:e2e "$here" >/dev/null

echo "==> Building the guest agent"
(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -trimpath -o "$art/differently-guest" ./cmd/differently-guest)

if [ ! -s "$art/images/vmlinux" ]; then
  echo "==> Downloading a guest kernel"
  if [ -z "${KERNEL_URL:-}" ]; then
    s3=https://s3.amazonaws.com/spec.ccfc.min
    prefix=$(curl -fsSL "$s3?list-type=2&prefix=firecracker-ci/&delimiter=/" \
      | grep -oE '<Prefix>firecracker-ci/[0-9]{8}-[^/<]+/' | sed 's/<Prefix>//' | sort | tail -1)
    key=$(curl -fsSL "$s3?list-type=2&prefix=${prefix}${fcarch}/vmlinux-6.1." \
      | grep -oE "<Key>${prefix}${fcarch}/vmlinux-6\.1\.[0-9]+</Key>" | sed 's/<[^>]*>//g' | sort -V | tail -1)
    KERNEL_URL="$s3/$key"
  fi
  curl -fsSL -o "$art/images/vmlinux" "$KERNEL_URL"
  echo "    $KERNEL_URL"
fi

echo "==> Building the guest rootfs"
"$repo/deploy/images/build-rootfs.sh" --image differently-guest-base:e2e \
  --guest-binary "$art/differently-guest" --output "$art/images/default.ext4" --size 1G

docker build -q -f "$here/agent.Dockerfile" -t differently-agent-e2e:latest "$here" >/dev/null
echo "==> Ready: docker compose -f test/e2e/compose.yaml up -d"
