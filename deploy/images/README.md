# Guest kernel and root filesystem images

A Differently Firecracker microVM boots from two files on the KVM host:

| File | Default path | Built by |
|---|---|---|
| Guest kernel | `/var/lib/differently/images/vmlinux` | `build-kernel.sh` |
| Root filesystem, one per template | `/var/lib/differently/images/<template>.ext4` | `build-rootfs.sh` |

The root filesystem runs the guest agent, `differently-guest`, from boot. The
node agent reaches it over Firecracker vsock on **port 10000** to run commands
and move files.

> **Status: unverified.** These scripts have been syntax-checked with
> `bash -n` and `shellcheck`, but have not been run end to end or booted
> under Firecracker. Two details are assumptions about the agent (see
> [Assumptions](#assumptions)).

## Requirements

- Docker or Podman (`CONTAINER_RUNTIME=podman`). The builds run inside a
  `debian:bookworm-slim` container, so they work from Linux or macOS hosts
  and need no root on the host.
- Network access to `cdn.kernel.org`, `raw.githubusercontent.com`, Debian's
  package mirrors and your image registry. For air-gapped builds, see
  [docs/deploy/air-gapped.md](../../docs/deploy/air-gapped.md).
- A `differently-guest` binary for the guest architecture (static Linux build).

## 1. Build the kernel

```sh
./build-kernel.sh --version 6.1.<patch> --arch x86_64 --out out/
```

- Use a longterm release from the 6.1 or 6.18 line (or 5.10). Those are the
  guest kernels Firecracker supports.
- The base config is Firecracker's CI guest config for that line
  (`resources/guest_configs/microvm-kernel-ci-<arch>-<line>.config` at
  `--firecracker-ref`, default `v1.17.0`). Check that path exists for your
  Firecracker version, or pass your own with `--config`.
- The script then forces virtio-mmio block, net and vsock, ext4, devtmpfs and
  `IP_PNP`.
- The tarball is checked against kernel.org's `sha256sums.asc`, fetched from the
  same origin. Pass `--sha256` from a second source for stronger assurance.
- On aarch64 the output is the uncompressed `Image`, still written as `vmlinux`.

Output: `out/vmlinux` and `out/vmlinux.config`.

## 2. Build a root filesystem from a container image

```sh
./build-rootfs.sh \
  --image docker.io/library/ubuntu:24.04 \
  --platform linux/amd64 \
  --guest-binary ./differently-guest \
  --output out/default.ext4
```

What it does:

1. Exports the image's filesystem (`docker create` + `docker export`).
2. Installs `differently-guest` as `/usr/local/bin/differently-guest`.
3. Wires it into boot (`--init auto`, the default):
   - **systemd images:** enables `files/differently-guest.service`
     (`WantedBy=sysinit.target`, `Restart=always`) and masks a few units that
     only slow a microVM boot.
   - **everything else:** installs `files/differently-init` as
     `/sbin/differently-init` and links `/sbin/init` to it. The init shim
     mounts `/proc`, `/sys`, `/dev`, `/run` and `/tmp`, brings up loopback,
     restarts the guest agent if it exits, and reaps orphans (best-effort).
     If the image already has `/sbin/init`, the script leaves it alone and
     tells you to boot with `init=/sbin/differently-init`.
4. Writes `/etc/hostname`, `/etc/hosts` and `/etc/fstab`. It leaves no default DNS
   resolver, because the guest network is configured at boot.
5. Creates the ext4 image with `mkfs.ext4 -d`, which needs no loop mounts and
   no root. The default size is the content plus 30% plus 256 MiB. Use
   `--size 4G` to override.

The image must contain `/bin/sh`. Extra guest-agent flags go in
`/etc/default/differently-guest` (`DIFFERENTLY_GUEST_ARGS=...`).

The file name is the template name: `default.ext4` is template `default`.

## 3. Put the images on KVM hosts

Choose one of these:

- **Kubernetes, OCI image (simplest):** package the files and let the chart's
  `agent.guestImages` init container copy them onto each node:

  ```sh
  ./package-oci.sh --tag registry.example.com/differently-guest-images:2026-10-28 \
    --kernel out/vmlinux --rootfs out/default.ext4 --push
  helm upgrade ... \
    --set agent.guestImages.enabled=true \
    --set agent.guestImages.image.registry=registry.example.com \
    --set agent.guestImages.image.repository=differently-guest-images \
    --set agent.guestImages.image.tag=2026-10-28
  ```

  Layers store the ext4 files uncompressed, with sparse regions expanded, so
  this suits images of a few GiB. Files are copied only when missing or
  changed, and replaced atomically.
- **Node bootstrap:** copy the files to `/var/lib/differently/images/` from
  cloud-init or user data, a golden AMI, or your configuration management.
- **Plain Linux hosts:** pass them to `deploy/systemd/install.sh --kernel ... --rootfs ...`.

## Verify

```sh
# Kernel: an ELF vmlinux on x86_64 (an arm64 boot Image on aarch64).
file out/vmlinux
grep -E '^CONFIG_(VIRTIO_VSOCKETS|VIRTIO_BLK|VIRTIO_NET|EXT4_FS)=y' out/vmlinux.config

# Root filesystem: clean ext4 with the guest agent and init wiring present.
docker run --rm -v "$PWD/out:/out:ro" debian:bookworm-slim sh -c '
  apt-get update -qq && apt-get install -y -qq e2fsprogs >/dev/null
  e2fsck -fn /out/default.ext4
  debugfs -R "stat /usr/local/bin/differently-guest" /out/default.ext4 | head -3
  debugfs -R "ls -l /sbin" /out/default.ext4 | grep -E " (init|differently-init)"'

# On a node: the agent doctor checks the kernel and images it will use.
kubectl logs -n differently <agent-pod> -c doctor
```

The end-to-end check is a sandbox: `differently sandbox create --profile small --wait`
followed by `differently sandbox exec <id> -- echo ok` (or `helm test`).

## Assumptions

- **Template discovery:** the agent advertises each `<images-dir>/<name>.ext4`
  as template `<name>`, and copies (and, if needed, grows) the file for each
  sandbox. **Unverified** against the agent implementation.
- **Guest agent invocation:** `differently-guest` with no arguments listens on
  vsock port 10000. Extra flags go in `DIFFERENTLY_GUEST_ARGS`.
- The kernel command line, including `root=`, `init=`, console and network
  `ip=` settings, comes from the node agent.
