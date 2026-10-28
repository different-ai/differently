#!/usr/bin/env bash
# Turn a container image into an ext4 root filesystem for Differently microVMs,
# with the guest agent (differently-guest, vsock port 10000) started at boot.
#
# Usage:
#   ./build-rootfs.sh --image IMAGE --guest-binary PATH --output NAME.ext4 \
#     [--platform linux/amd64|linux/arm64] [--size 4G] [--init auto|systemd|shim]
#
# The output's basename is the template name the agent advertises (for
# example default.ext4 -> template "default"). The image must contain
# /bin/sh. --init auto uses systemd when the image has it, otherwise installs
# the minimal init shim from files/differently-init. Needs Docker or Podman;
# no root on the host. Not run in CI; see README.md.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
image=""
guest=""
output=""
platform=""
size=""
init_mode=auto
runtime=${CONTAINER_RUNTIME:-docker}
builder_image=${BUILDER_IMAGE:-docker.io/library/debian:bookworm-slim}

usage() {
  sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
  case "$1" in
    --image) image=$2; shift 2 ;;
    --guest-binary) guest=$2; shift 2 ;;
    --output) output=$2; shift 2 ;;
    --platform) platform=$2; shift 2 ;;
    --size) size=$2; shift 2 ;;
    --init) init_mode=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ -n "$image" ] || { echo "--image is required" >&2; exit 2; }
[ -n "$guest" ] && [ -f "$guest" ] || { echo "--guest-binary must name the differently-guest binary" >&2; exit 2; }
[ -n "$output" ] || { echo "--output is required" >&2; exit 2; }
case "$output" in *.ext4) ;; *) echo "--output must end in .ext4" >&2; exit 2 ;; esac
case "$init_mode" in auto|systemd|shim) ;; *) echo "--init must be auto, systemd or shim" >&2; exit 2 ;; esac
command -v "$runtime" >/dev/null || { echo "$runtime not found (set CONTAINER_RUNTIME=podman to use Podman)" >&2; exit 1; }

out_dir=$(cd "$(dirname "$output")" && pwd)
out_name=$(basename "$output")
work=$(mktemp -d)
cid=""
cleanup() {
  [ -z "$cid" ] || "$runtime" rm -f "$cid" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

platform_args=()
[ -z "$platform" ] || platform_args=(--platform "$platform")

echo "==> Exporting $image"
"$runtime" pull ${platform_args[@]+"${platform_args[@]}"} "$image" >/dev/null
cid=$("$runtime" create ${platform_args[@]+"${platform_args[@]}"} "$image" /bin/sh)
"$runtime" export "$cid" -o "$work/rootfs.tar"
"$runtime" rm -f "$cid" >/dev/null
cid=""

cp "$guest" "$work/differently-guest"
cp "$SCRIPT_DIR/files/differently-init" "$SCRIPT_DIR/files/differently-guest.service" "$work/"

echo "==> Building $out_name (init: $init_mode)"
# The script below runs inside the builder container, so it is single-quoted.
# shellcheck disable=SC2016
"$runtime" run --rm \
  -e INIT_MODE="$init_mode" -e SIZE="$size" -e OUT_NAME="$out_name" \
  -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
  -v "$work:/work" -v "$out_dir:/out" \
  "$builder_image" bash -euo pipefail -c '
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq && apt-get install -y -qq --no-install-recommends e2fsprogs >/dev/null
    root=/work/root
    mkdir -p "$root"
    tar -xpf /work/rootfs.tar -C "$root" --numeric-owner
    rm -f "$root/.dockerenv"

    # Resolve a path inside $root, following one level of top-level symlinks
    # such as merged-/usr (sbin -> usr/sbin), absolute or relative.
    inroot() {
      local top=${1%%/*} rest=${1#*/}
      if [ -L "$root/$top" ]; then
        local t; t=$(readlink "$root/$top"); t=${t#/}
        echo "$root/$t/$rest"
      else
        echo "$root/$1"
      fi
    }

    install -D -m 0755 /work/differently-guest "$(inroot usr/local/bin/differently-guest)"
    install -d -m 0755 "$(inroot etc/default)"
    [ -e "$(inroot etc/default/differently-guest)" ] || \
      printf "# Extra arguments for differently-guest.\nDIFFERENTLY_GUEST_ARGS=\n" > "$(inroot etc/default/differently-guest)"

    has_systemd=false
    for p in lib/systemd/systemd usr/lib/systemd/systemd; do
      [ -x "$(inroot "$p")" ] && has_systemd=true
    done
    mode=$INIT_MODE
    if [ "$mode" = auto ]; then mode=$($has_systemd && echo systemd || echo shim); fi
    if [ "$mode" = systemd ] && ! $has_systemd; then
      echo "--init systemd, but the image has no systemd" >&2; exit 1
    fi

    if [ "$mode" = systemd ]; then
      unit_dir=$(inroot etc/systemd/system)
      install -D -m 0644 /work/differently-guest.service "$unit_dir/differently-guest.service"
      mkdir -p "$unit_dir/sysinit.target.wants"
      ln -sf ../differently-guest.service "$unit_dir/sysinit.target.wants/differently-guest.service"
      # Units that only slow a microVM boot.
      for u in systemd-networkd-wait-online.service systemd-timesyncd.service getty@tty1.service; do
        ln -sf /dev/null "$unit_dir/$u"
      done
      init_target=$(inroot sbin/init)
      if [ ! -e "$init_target" ] && [ ! -L "$init_target" ]; then
        ln -s /lib/systemd/systemd "$init_target"
      fi
    else
      install -D -m 0755 /work/differently-init "$(inroot sbin/differently-init)"
      init_target=$(inroot sbin/init)
      if [ -e "$init_target" ] || [ -L "$init_target" ]; then
        echo "note: the image has its own /sbin/init; boot with init=/sbin/differently-init to use the shim"
      else
        ln -s differently-init "$init_target"
      fi
    fi

    # Docker leaves these as empty runtime mount points.
    echo sandbox > "$(inroot etc/hostname)"
    printf "127.0.0.1 localhost\n::1 localhost\n127.0.1.1 sandbox\n" > "$(inroot etc/hosts)"
    if [ ! -s "$(inroot etc/resolv.conf)" ] || [ -L "$(inroot etc/resolv.conf)" ]; then
      rm -f "$(inroot etc/resolv.conf)"
      printf "# Written at boot by the guest agent or your template; no default resolver.\n" > "$(inroot etc/resolv.conf)"
    fi
    printf "/dev/vda / ext4 defaults,noatime 0 1\n" > "$(inroot etc/fstab)"

    if [ -z "$SIZE" ]; then
      used_mib=$(du -sm "$root" | cut -f1)
      SIZE="$(( used_mib * 13 / 10 + 256 ))M"
    fi
    rm -f "/out/$OUT_NAME"
    truncate -s "$SIZE" "/out/$OUT_NAME"
    mkfs.ext4 -q -F -L rootfs -E root_owner=0:0 -d "$root" "/out/$OUT_NAME"
    e2fsck -fn "/out/$OUT_NAME" >/dev/null
    chown "$HOST_UID:$HOST_GID" "/out/$OUT_NAME"
    echo "init: $mode; size: $SIZE"
  '

echo "==> Wrote $out_dir/$out_name (template \"${out_name%.ext4}\")"
