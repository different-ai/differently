# ADR 0002: Drive Firecracker directly through a node agent instead of Kata on Kubernetes

- Status: Accepted
- Date: 2026-10-28

## Context

Private pools need hardware-isolated sandboxes on KVM hosts the operator owns,
with stop/start, pause/resume, disk snapshots and fork, and later memory
snapshots. Two broad ways exist to run Firecracker under Kubernetes:

1. **Kata Containers with the Firecracker hypervisor**, so each sandbox is a pod
   with a RuntimeClass.
2. **A node agent** that owns Firecracker processes directly on each KVM host
   and is driven by the control plane.

Facts from the research notes (2026-10-28):

- Kata's Firecracker profile needs a devmapper thin-pool that kata-deploy does
  not create, has no filesystem sharing (Firecracker has no virtio-fs), and its
  example profile is amd64-only.
- Kata 4.0 made the Rust runtime the default; the release lists QEMU, Cloud
  Hypervisor and Dragonball as runtime-rs hypervisors, not Firecracker, so the
  Firecracker path appears to be on the deprecated Go runtime.
- Kata documents no checkpoint/restore. k7's Kata+Firecracker backend rejects
  named snapshots and forks and implements pause as scale to zero.
- firecracker-containerd has no releases and is pinned to containerd 1.7, which
  reached end of life on 2026-09-30.
- Firecracker itself supports pause/resume, full snapshots (diff snapshots are
  in developer preview), vsock and the jailer.
- OpenSandbox's Fast Sandbox and Tencent CubeSandbox use the node-agent shape,
  so it is a known-workable architecture (and not, by itself, a
  differentiator).

## Decision

Run one `differently-agent` per KVM host. The agent launches and configures
Firecracker through its API socket (`internal/firecracker/client.go`), keeps a
state directory per VM, sets up per-VM networking, reaches the in-guest agent
over vsock, and serves everything to the control plane through the same
adapter protocol community plugins use (`pkg/adapter/adapterhttp`). The
control plane's `firecracker` adapter (`internal/adapters/firecracker/pool.go`)
aggregates the agents that registered under a pool name and places each
sandbox on the agent with the most free memory that fits it.

## Consequences

- Differently controls the whole VM lifecycle, so pause/resume and disk
  snapshots are implemented today (`internal/agent/agent.go`), and memory
  snapshots can be added without waiting for another project
  ([ADR 0014](0014-defer-memory-snapshots-until-restore-proven.md)).
- The same agent runs with or without Kubernetes.
- Differently owns code that Kata would otherwise own: rootfs preparation, tap
  and iptables setup, process supervision, restart recovery. It also owns the
  security hardening Kata would partly provide, notably the jailer
  ([ADR 0015](0015-defer-jailer-integration.md)).
- Kubernetes does not know about VM resource usage; the agent reserves host
  CPU and memory itself (`--reserve-cpu`, `--reserve-memory-mib`,
  `--cpu-overcommit`).
- Images are raw ext4 rootfs files plus a guest kernel per node, not OCI
  images. An OCI-to-rootfs pipeline is outside the MVP.
- **Unproven:** nothing in this design has run on real KVM yet. The agent is
  tested with a fake VMM and fake guest. VMs are started with `setsid`, but in
  a container the agent is PID 1 of its namespace and the systemd unit uses
  `KillMode=control-group`, so in both shipped deployments VMs are expected to
  stop when the agent restarts. See [POC 1 and POC 2](../proof-of-concepts.md).

## Alternatives considered

- **Kata + Firecracker RuntimeClass.** Rejected for the reasons above: no
  snapshots or fork, devmapper setup burden, a deprecated runtime path, amd64
  only.
- **Kata + Cloud Hypervisor.** Kata still exposes no checkpoint/restore, so the
  lifecycle gap remains. Cloud Hypervisor on its own remains the most likely
  second VMM when virtio-fs or GPU passthrough is needed.
- **firecracker-containerd.** Rejected: no releases, EOL containerd line.
- **flintlock.** Active and close in scope (gRPC microVM lifecycle on
  Firecracker or Cloud Hypervisor, vsock guest agent). Rejected for the MVP to
  avoid an MPL-2.0 dependency and a second control API in the data path; it
  remains a candidate for an adapter.
- **KubeVirt.** Full QEMU VMs with CSI-level disk snapshots and no memory
  snapshot API; heavier per sandbox than needed.
