#!/usr/bin/env bash
# Package a guest kernel and root filesystems into an OCI image that the Helm
# chart's agent.guestImages init container copies onto each KVM node
# (<hostDataPath>/images).
#
# Usage:
#   ./package-oci.sh --tag REGISTRY/differently-guest-images:VERSION \
#     --kernel out/vmlinux --rootfs out/default.ext4 [--rootfs ...] \
#     [--platform linux/amd64] [--push]
#
# The image is busybox plus /images/*. Root filesystems are stored uncompressed
# in the layer (sparse regions are expanded), so keep them small or stage
# images on nodes another way (see README.md). Not run in CI.
set -euo pipefail

tag=""
kernel=""
rootfs=()
platform=""
push=false
runtime=${CONTAINER_RUNTIME:-docker}
# Pin by digest for repeatable builds.
base_image=${BASE_IMAGE:-docker.io/library/busybox:1.37}

usage() {
  sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
  case "$1" in
    --tag) tag=$2; shift 2 ;;
    --kernel) kernel=$2; shift 2 ;;
    --rootfs) rootfs+=("$2"); shift 2 ;;
    --platform) platform=$2; shift 2 ;;
    --push) push=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ -n "$tag" ] || { echo "--tag is required" >&2; exit 2; }
[ -f "$kernel" ] || { echo "--kernel must name the vmlinux file" >&2; exit 2; }
[ "${#rootfs[@]}" -gt 0 ] || { echo "at least one --rootfs is required" >&2; exit 2; }

ctx=$(mktemp -d "${TMPDIR:-/tmp}/differently-oci.XXXXXX")
trap 'rm -rf "$ctx"' EXIT
mkdir -p "$ctx/images"
cp "$kernel" "$ctx/images/vmlinux"
for f in "${rootfs[@]}"; do
  [ -f "$f" ] || { echo "not a file: $f" >&2; exit 2; }
  case "$f" in *.ext4) ;; *) echo "root filesystems must end in .ext4: $f" >&2; exit 2 ;; esac
  cp "$f" "$ctx/images/"
done
sumcmd=(sha256sum)
command -v sha256sum >/dev/null || sumcmd=(shasum -a 256)
(cd "$ctx/images" && "${sumcmd[@]}" ./* > SHA256SUMS)

cat > "$ctx/Dockerfile" <<EOF
FROM $base_image
LABEL org.opencontainers.image.title="differently-guest-images" \\
      org.opencontainers.image.description="Firecracker guest kernel and root filesystems for Differently" \\
      org.opencontainers.image.licenses="Apache-2.0"
COPY images/ /images/
EOF

platform_args=()
[ -z "$platform" ] || platform_args=(--platform "$platform")
"$runtime" build ${platform_args[@]+"${platform_args[@]}"} -t "$tag" "$ctx"
echo "==> Built $tag"
cat "$ctx/images/SHA256SUMS"
if $push; then
  "$runtime" push "$tag"
fi
echo
echo "Use it with:  --set agent.guestImages.enabled=true \\"
echo "              --set agent.guestImages.image.repository=<repo> --set agent.guestImages.image.tag=<tag>"
