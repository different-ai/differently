# Competitive landscape

Where Differently fits among sandbox platforms, and why. Evidence date
**2026-10-28**; star counts and versions are point-in-time readings from public
repositories and documentation. Anything not confirmed by a primary source is
marked UNVERIFIED. Re-check before quoting any of this externally.

## The short version

1. **Self-hosted, open-source Firecracker on Kubernetes is already done.**
   [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) (Apache-2.0)
   has a Fast Sandbox backend whose per-node DaemonSet agent owns Firecracker
   VMs, with memory-preserving pause/resume. Tencent
   [CubeSandbox](https://github.com/TencentCloud/CubeSandbox) (Apache-2.0) runs
   KVM sandboxes behind an E2B-compatible API. E2B's
   [runtime](https://github.com/e2b-dev/runtime) open-sources its control plane
   (officially single-node). Differently does not claim to be first here.
2. **Nobody routes sandboxes across your own microVMs and other vendors by
   policy, under a control plane you run.** Vendors route only among their own
   backends; multi-provider tools are client-side libraries
   ([ComputeSDK](https://github.com/computesdk/computesdk)) or agent platforms
   that accept several sandbox providers. That server-side,
   policy-and-classification-driven placement — with per-sandbox placement
   explanations and an operator trust report — is Differently's position.
3. **The position is real but copyable.** OpenSandbox or CubeSandbox could add
   external providers. Differently's durable advantages have to be execution
   quality: operability (one Helm install, preflight, doctor), honest security
   boundaries, and a clean adapter contract that others can implement.

## Ownership models

"Self-hosted" in this document means you can run and operate the control
plane with no vendor service involved. "BYOC" means your compute with the
vendor running or managing the control plane.

| Platform | License | Isolation | Self-hosted control plane | BYOC | Routes to other vendors |
|---|---|---|---|---|---|
| **Differently** | Apache-2.0 | Firecracker (own nodes) + whatever external pools use | Yes | n/a | **Yes, by policy, fail-closed** |
| Boat | Proprietary | Full VM per sandbox | No ([self-host comparison](https://boat.dev/compare/boat-vs-self-host-baremetal)) | No | No |
| Daytona | Archived AGPL-3.0 repo; current core private | Containers or VMs | No maintained path | Yes — your runners, Daytona's control plane ([BYOC](https://www.daytona.io/docs/en/bring-your-own-compute.md)) | No |
| Freestyle | Proprietary (SDK MIT) | Hardware-virtualized microVMs on its own racks | No | No (colocation offered) | No |
| E2B | Apache-2.0 | Firecracker | Single node ("Embed"); multi-node via Enterprise | Enterprise | No |
| Modal, Fly.io, Vercel Sandbox, Cloudflare | Proprietary platforms | gVisor / microVMs | No | No | No |
| Northflank | Proprietary | Kata + Cloud Hypervisor (vendor claim) | No | Yes (BYOC/BYOK) | No |
| OpenSandbox | Apache-2.0 | Docker, Kubernetes, or Firecracker (Fast Sandbox) | Yes | n/a | Among its own backends |
| CubeSandbox | Apache-2.0 | RustVMM on KVM | Yes (Kubernetes deploy in preview) | n/a | No |
| kubernetes-sigs/agent-sandbox | Apache-2.0 | Delegated to a RuntimeClass (gVisor, Kata) | Yes (controller only) | n/a | Per RuntimeClass |
| microsandbox | Apache-2.0 | libkrun microVMs | Single host library | n/a | No |

## What this means for each relationship

- **Daytona and Freestyle are backends, not targets.** Differently routes *to*
  them for burst capacity, special hardware or regions, under policy. It should
  not be positioned against "open-source Daytona", which is no longer
  maintained in the open.
- **Boat is the developer-experience bar.** Its API is clean and well
  documented. Differently matches the portable core (sandbox CRUD, readiness,
  exec, streaming, files, stop/start, fork, snapshots, idempotency) and goes
  further on pause/resume, per-command env and stdin, and placement
  transparency. It is behind on detached processes, SSH, port URLs on its own
  pools, webhooks and file-management helpers. See
  [feature-parity.md](feature-parity.md).
- **OpenSandbox is now also a backend.** The `opensandbox` adapter places
  Differently sandboxes on an OpenSandbox server, so Differently can
  orchestrate OpenSandbox alongside its own Firecracker nodes, Daytona and
  Freestyle. Whether OpenSandbox (with Fast Sandbox) should replace
  Differently's own node agent as the default runtime is open until the
  Fast Sandbox spike ([ADR 0019](adr/0019-opensandbox-backend-first.md),
  [spike](research/opensandbox-spike.md)).
- **OpenSandbox and CubeSandbox are the closest open-source peers.** Worth
  tracking closely and borrowing from: OpenSandbox's host checks (kernel ≥5.10,
  `/dev/kvm`, `/dev/net/tun`, cgroup v2, reflink filesystems) are a good
  reference for `differently-agent doctor`. If either adds policy-gated external
  providers, Differently's differentiation narrows to operability and trust
  tooling.
- **agent-sandbox is a possible substrate.** It standardizes a Sandbox CRD with
  warm pools; a future Differently adapter could target it for organizations
  that want gVisor/Kata isolation through Kubernetes instead of Firecracker
  nodes.

## Building blocks and why Differently drives Firecracker directly

| Option | Why not the MVP |
|---|---|
| Kata Containers + Firecracker | Needs a hand-configured devmapper thin-pool, has no filesystem sharing, its Firecracker profile is amd64-only, and Kata offers no checkpoint/restore — so pause, snapshot and fork would be lost. |
| firecracker-containerd | No published releases; pinned to containerd 1.7, which reached end of life on 2026-09-30. |
| KubeVirt | Full VMs with heavier boot and density trade-offs; a good later adapter for long-lived VMs. |
| Kueue | Quota and admission for batch Jobs/JobSets/pods; preemption deletes pods, which does not fit interactive, stateful sandboxes. A fit for a future ephemeral-jobs API. |

Firecracker (v1.17.0, 2026-09-10) provides full snapshots, vsock and a stable
API; it has no GPU passthrough in any release and is validated by its
maintainers on AWS `.metal` instances. Details and sources:
[architecture/README.md](architecture/README.md) and
[adr/](adr/).

## Where KVM is available (affects where Firecracker pools can run)

| Environment | Status |
|---|---|
| AWS | `.metal` instances; nested virtualization now available on Intel 7th/8th-gen C/M/R 7i and 8i families (EKS managed node group support reported, verify before relying on it). |
| GCP | Nested virtualization on Intel series and AMD N4D; GKE needs privileged pods (Autopilot only for allowlisted workloads). |
| Azure | Nested virtualization on selected sizes (for example Dsv6); KVM inside Linux VMs works but is not officially supported. AKS Pod Sandboxing uses Microsoft's hypervisor, not KVM, so it cannot host Firecracker agents. |
| Bare metal | k3s, RKE2 and Talos work; Talos and RKE2's CIS profile block privileged pods by default, so the agent namespace needs an explicit exemption. |

Step-by-step guides: [deploy/](deploy/README.md).
