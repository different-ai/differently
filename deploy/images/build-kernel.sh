#!/usr/bin/env bash
# Build a Firecracker guest kernel (vmlinux) inside a container.
#
# Starts from Firecracker's own CI guest config for the kernel line, then
# forces the options Differently guests rely on (virtio-mmio block, net and
# vsock, ext4, devtmpfs). The host needs only Docker or Podman.
#
# Usage:
#   ./build-kernel.sh --version 6.1.<patch> [--arch x86_64|aarch64] \
#     [--firecracker-ref v1.17.0] [--config FILE] [--out DIR] [--sha256 HEX]
#
# Output: <out>/vmlinux (on aarch64 this is the uncompressed `Image`) and
# <out>/vmlinux.config. Not run in CI; see README.md.
set -euo pipefail

version=""
arch=$(uname -m)
[ "$arch" = arm64 ] && arch=aarch64
fc_ref=v1.17.0
config=""
out=$PWD/out
sha256=""
runtime=${CONTAINER_RUNTIME:-docker}
builder_image=${BUILDER_IMAGE:-docker.io/library/debian:bookworm-slim}

usage() {
  sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
  case "$1" in
    --version) version=$2; shift 2 ;;
    --arch) arch=$2; shift 2 ;;
    --firecracker-ref) fc_ref=$2; shift 2 ;;
    --config) config=$2; shift 2 ;;
    --out) out=$2; shift 2 ;;
    --sha256) sha256=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ -n "$version" ] || { echo "--version is required (a 6.1.y or 6.18.y longterm release from kernel.org)" >&2; exit 2; }
case "$arch" in x86_64|aarch64) ;; *) echo "--arch must be x86_64 or aarch64" >&2; exit 2 ;; esac
case "$version" in
  5.10.*|6.1.*|6.18.*) ;;
  *) echo "warning: $version is not a guest kernel line Firecracker supports (5.10, 6.1, 6.18)" >&2 ;;
esac
command -v "$runtime" >/dev/null || { echo "$runtime not found (set CONTAINER_RUNTIME=podman to use Podman)" >&2; exit 1; }

line=$(echo "$version" | cut -d. -f1,2)
major=$(echo "$version" | cut -d. -f1)
mkdir -p "$out"
out=$(cd "$out" && pwd)

config_mount=()
if [ -n "$config" ]; then
  [ -f "$config" ] || { echo "not a file: $config" >&2; exit 1; }
  config_mount=(-v "$(cd "$(dirname "$config")" && pwd)/$(basename "$config"):/in/kernel.config:ro")
fi

echo "==> Building Linux $version for $arch (Firecracker config ref $fc_ref) into $out"
# The script below runs inside the builder container, so it is single-quoted.
# shellcheck disable=SC2016
"$runtime" run --rm \
  -e VERSION="$version" -e LINE="$line" -e MAJOR="$major" -e ARCH_NAME="$arch" \
  -e FC_REF="$fc_ref" -e SHA256="$sha256" -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
  -v "$out:/out" ${config_mount[@]+"${config_mount[@]}"} \
  "$builder_image" bash -euo pipefail -c '
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    pkgs="build-essential bc bison flex libelf-dev libssl-dev curl ca-certificates xz-utils cpio kmod"
    cross=""
    if [ "$ARCH_NAME" = aarch64 ] && [ "$(uname -m)" != aarch64 ]; then
      pkgs="$pkgs gcc-aarch64-linux-gnu"; cross="aarch64-linux-gnu-"
    fi
    if [ "$ARCH_NAME" = x86_64 ] && [ "$(uname -m)" != x86_64 ]; then
      pkgs="$pkgs gcc-x86-64-linux-gnu"; cross="x86_64-linux-gnu-"
    fi
    apt-get install -y -qq --no-install-recommends $pkgs >/dev/null

    cd /tmp
    base="https://cdn.kernel.org/pub/linux/kernel/v${MAJOR}.x"
    curl -fsSL --proto "=https" -o "linux-$VERSION.tar.xz" "$base/linux-$VERSION.tar.xz"
    if [ -z "$SHA256" ]; then
      # Same-origin checksum over TLS. For stronger assurance pass --sha256 from
      # a second source, or verify sha256sums.asc with the kernel.org keys.
      curl -fsSL --proto "=https" -o sha256sums.asc "$base/sha256sums.asc"
      SHA256=$(grep " linux-$VERSION.tar.xz\$" sha256sums.asc | cut -d" " -f1)
      [ -n "$SHA256" ] || { echo "no checksum for linux-$VERSION.tar.xz in sha256sums.asc" >&2; exit 1; }
    fi
    echo "$SHA256  linux-$VERSION.tar.xz" | sha256sum -c -
    tar -xf "linux-$VERSION.tar.xz"
    cd "linux-$VERSION"

    if [ -f /in/kernel.config ]; then
      cp /in/kernel.config .config
    else
      url="https://raw.githubusercontent.com/firecracker-microvm/firecracker/$FC_REF/resources/guest_configs/microvm-kernel-ci-$ARCH_NAME-$LINE.config"
      echo "==> Base config: $url"
      curl -fsSL --proto "=https" -o .config "$url" || {
        echo "Could not fetch the Firecracker guest config for $ARCH_NAME $LINE at $FC_REF." >&2
        echo "Check the path in the Firecracker repo, or pass --config." >&2
        exit 1
      }
    fi

    karch=x86_64; target=vmlinux; artifact=vmlinux
    if [ "$ARCH_NAME" = aarch64 ]; then karch=arm64; target=Image; artifact=arch/arm64/boot/Image; fi

    # Options Differently guests rely on; the Firecracker CI configs already
    # set most of them.
    for opt in VIRTIO VIRTIO_MMIO VIRTIO_MMIO_CMDLINE_DEVICES VIRTIO_BLK VIRTIO_NET \
               VSOCKETS VIRTIO_VSOCKETS EXT4_FS DEVTMPFS DEVTMPFS_MOUNT TMPFS \
               UNIX INET IP_PNP CGROUPS MAGIC_SYSRQ SERIAL_8250_CONSOLE; do
      scripts/config --enable "$opt"
    done
    make ARCH=$karch CROSS_COMPILE=$cross olddefconfig >/dev/null
    make ARCH=$karch CROSS_COMPILE=$cross -j"$(nproc)" $target
    install -m 0644 "$artifact" /out/vmlinux
    install -m 0644 .config /out/vmlinux.config
    chown "$HOST_UID:$HOST_GID" /out/vmlinux /out/vmlinux.config
  '

echo "==> Wrote $out/vmlinux"
sha256sum "$out/vmlinux" 2>/dev/null || shasum -a 256 "$out/vmlinux"
