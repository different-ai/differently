#!/usr/bin/env bash
# Install differently-agent as a systemd service on a plain Linux KVM host,
# so the host can join a Firecracker pool of an existing Differently control
# plane (no Kubernetes needed).
#
# Everything is installed from local files you provide. The one optional
# download (--firecracker-version) fetches the official Firecracker release
# from GitHub and refuses to install it without a matching --firecracker-sha256.
#
# Usage: sudo ./install.sh --agent-binary PATH --kernel PATH --rootfs PATH \
#          (--firecracker-tarball PATH | --firecracker-version vX.Y.Z) [options]
# Run ./install.sh --help for every option.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
BIN_DIR=/usr/local/bin
ETC_DIR=/etc/differently
DATA_DIR=/var/lib/differently
IMAGES_DIR=$DATA_DIR/images
UNIT_PATH=/etc/systemd/system/differently-agent.service

agent_binary=""
fc_tarball=""
fc_version=""
fc_sha256=""
kernel=""
rootfs=()
env_file=""
start=true
dry_run=false

usage() {
  cat <<'EOF'
Install the Differently node agent as a systemd service.

Required:
  --agent-binary PATH         differently-agent binary for this architecture
  --kernel PATH               guest kernel (vmlinux), see deploy/images/
  --rootfs PATH               guest root filesystem (<template>.ext4); repeatable
  --firecracker-tarball PATH  official firecracker-vX.Y.Z-<arch>.tgz, or:
  --firecracker-version VER   download that release from GitHub (needs --firecracker-sha256)

Optional:
  --firecracker-sha256 HEX    expected SHA-256 of the tarball (checked when given)
  --env-file PATH             agent.env to install (default: agent.env.example,
                              which you must then edit)
  --no-start                  install and enable, but do not start
  --dry-run                   print what would be done
  -h, --help                  this help
EOF
}

log() { printf '==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
run() {
  if $dry_run; then printf '[dry-run] %s\n' "$*"; else "$@"; fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    --agent-binary) agent_binary=$2; shift 2 ;;
    --firecracker-tarball) fc_tarball=$2; shift 2 ;;
    --firecracker-version) fc_version=$2; shift 2 ;;
    --firecracker-sha256) fc_sha256=$2; shift 2 ;;
    --kernel) kernel=$2; shift 2 ;;
    --rootfs) rootfs+=("$2"); shift 2 ;;
    --env-file) env_file=$2; shift 2 ;;
    --no-start) start=false; shift ;;
    --dry-run) dry_run=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
done

[ -n "$agent_binary" ] || die "--agent-binary is required"
[ -n "$kernel" ] || die "--kernel is required"
[ "${#rootfs[@]}" -gt 0 ] || die "at least one --rootfs is required"
if [ -z "$fc_tarball" ] && [ -z "$fc_version" ]; then
  die "--firecracker-tarball or --firecracker-version is required"
fi
if [ -n "$fc_version" ] && [ -z "$fc_sha256" ]; then
  die "--firecracker-version needs --firecracker-sha256 (copy it from the release's .sha256.txt and compare it with a second source)"
fi
for f in "$agent_binary" "$kernel" "${rootfs[@]}"; do
  [ -f "$f" ] || die "not a file: $f"
done
[ -z "$fc_tarball" ] || [ -f "$fc_tarball" ] || die "not a file: $fc_tarball"
[ -z "$env_file" ] || [ -f "$env_file" ] || die "not a file: $env_file"

# --- Host checks ---------------------------------------------------------------
log "Checking the host"
[ "$(uname -s)" = Linux ] || die "Linux is required"
if ! $dry_run; then
  [ "$(id -u)" -eq 0 ] || die "run as root (sudo)"
fi
command -v systemctl >/dev/null || die "systemd is required"

arch=$(uname -m)
case "$arch" in
  x86_64|aarch64) ;;
  *) die "unsupported architecture $arch (Firecracker supports x86_64 and aarch64)" ;;
esac

if [ ! -e /dev/kvm ]; then
  if [ "$arch" = x86_64 ] && ! grep -qE '\b(vmx|svm)\b' /proc/cpuinfo; then
    die "/dev/kvm is missing and the CPU exposes no vmx/svm flag: enable virtualization in firmware, use a .metal instance, or enable nested virtualization"
  fi
  die "/dev/kvm is missing: load the module (modprobe kvm_intel or kvm_amd) and check firmware settings"
fi
[ -r /dev/kvm ] && [ -w /dev/kvm ] || die "/dev/kvm is not readable and writable by root"

if [ ! -e /dev/net/tun ]; then
  run modprobe tun || true
  $dry_run || [ -e /dev/net/tun ] || die "/dev/net/tun is missing (modprobe tun)"
fi

if [ "$(stat -fc %T /sys/fs/cgroup 2>/dev/null || true)" != cgroup2fs ]; then
  warn "cgroup v2 is not mounted at /sys/fs/cgroup; Firecracker snapshot restore is slow on cgroup v1"
fi

kver=$(uname -r)
case "$kver" in
  5.10.*|6.1.*|6.18.*) ;;
  *) warn "host kernel $kver is not one Firecracker lists as supported (5.10, 6.1, 6.18)" ;;
esac

# --- Binaries ------------------------------------------------------------------
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if [ -n "$fc_version" ]; then
  fc_tarball=$tmp/firecracker-$fc_version-$arch.tgz
  url="https://github.com/firecracker-microvm/firecracker/releases/download/$fc_version/firecracker-$fc_version-$arch.tgz"
  log "Downloading $url"
  run curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 -o "$fc_tarball" "$url"
fi

if [ -n "$fc_sha256" ] && ! $dry_run; then
  log "Verifying the Firecracker tarball checksum"
  echo "$fc_sha256  $fc_tarball" | sha256sum -c - >/dev/null || die "SHA-256 mismatch for $fc_tarball"
fi

log "Installing binaries into $BIN_DIR"
if ! $dry_run; then
  tar -xzf "$fc_tarball" -C "$tmp"
  fc_bin=$(find "$tmp" -type f -name 'firecracker-v*' ! -name '*.debug' ! -name '*.sha256*' | head -n 1)
  jailer_bin=$(find "$tmp" -type f -name 'jailer-v*' ! -name '*.debug' ! -name '*.sha256*' | head -n 1)
  [ -n "$fc_bin" ] || die "no firecracker binary in $fc_tarball"
  [ -n "$jailer_bin" ] || die "no jailer binary in $fc_tarball"
else
  fc_bin="<firecracker from tarball>"
  jailer_bin="<jailer from tarball>"
fi
run install -o root -g root -m 0755 "$agent_binary" "$BIN_DIR/differently-agent"
run install -o root -g root -m 0755 "$fc_bin" "$BIN_DIR/firecracker"
run install -o root -g root -m 0755 "$jailer_bin" "$BIN_DIR/jailer"

# --- Directories and guest images ---------------------------------------------
log "Installing guest images into $IMAGES_DIR"
run install -d -o root -g root -m 0750 "$ETC_DIR" "$DATA_DIR" "$DATA_DIR/agent" "$IMAGES_DIR"
run install -o root -g root -m 0644 "$kernel" "$IMAGES_DIR/vmlinux"
for f in "${rootfs[@]}"; do
  # Copy then rename, so running microVMs keep the file they opened.
  b=$(basename "$f")
  run install -o root -g root -m 0644 "$f" "$IMAGES_DIR/.$b.tmp"
  run mv -f "$IMAGES_DIR/.$b.tmp" "$IMAGES_DIR/$b"
done

# --- Configuration -------------------------------------------------------------
if [ -e "$ETC_DIR/agent.env" ]; then
  log "Keeping the existing $ETC_DIR/agent.env"
else
  src=${env_file:-$SCRIPT_DIR/agent.env.example}
  log "Installing $ETC_DIR/agent.env from $src (mode 0600)"
  run install -o root -g root -m 0600 "$src" "$ETC_DIR/agent.env"
fi

log "Installing $UNIT_PATH"
run install -o root -g root -m 0644 "$SCRIPT_DIR/differently-agent.service" "$UNIT_PATH"
run systemctl daemon-reload
run systemctl enable differently-agent.service

if $dry_run; then
  log "Dry run complete"
  exit 0
fi

# --- Doctor and start ----------------------------------------------------------
missing=$(
  set -a
  # shellcheck disable=SC1091
  . "$ETC_DIR/agent.env"
  for v in DIFFERENTLY_SERVER_URL DIFFERENTLY_AGENT_TOKEN DIFFERENTLY_POOL DIFFERENTLY_ADVERTISE_URL DIFFERENTLY_NODE_NAME; do
    [ -n "${!v:-}" ] || printf '%s ' "$v"
  done
)

log "Running differently-agent doctor"
if ! "$BIN_DIR/differently-agent" doctor \
  --state-dir="$DATA_DIR/agent" --images-dir="$IMAGES_DIR" \
  --kernel="$IMAGES_DIR/vmlinux" --firecracker="$BIN_DIR/firecracker"; then
  die "doctor reported problems; fix them, then: systemctl start differently-agent"
fi

if [ -n "$missing" ]; then
  warn "set these in $ETC_DIR/agent.env, then run 'systemctl start differently-agent': $missing"
  exit 0
fi

if $start; then
  log "Starting differently-agent"
  systemctl restart differently-agent.service
  sleep 3
  systemctl --no-pager --lines=20 status differently-agent.service || true
  cat <<EOF

Verify the agent registered (from a machine with an operator token):
  curl -fsS -H "Authorization: Bearer \$DIFFERENTLY_API_KEY" "\$DIFFERENTLY_URL/v1/admin/agents"
EOF
else
  log "Installed and enabled; start with: systemctl start differently-agent"
fi
