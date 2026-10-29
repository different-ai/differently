# Competitive landscape

Where Differently fits among sandbox platforms, and why. Evidence date
**2026-10-28**; star counts and versions are point-in-time readings from public
repositories and documentation. Anything not confirmed by a primary source is
marked UNVERIFIED. Re-check before quoting any of this externally.

## The short version

1. **Self-hosted, open-source sandbox runtimes already exist, so Differently
   uses one instead of building its own.**
   - [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox)
     (Apache-2.0) runs sandboxes on Docker, Kubernetes, or Firecracker
     microVMs through its Fast Sandbox backend, with memory-preserving
     pause/resume.
   - Tencent [CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
     (Apache-2.0) runs KVM sandboxes behind an E2B-compatible API.
   - E2B's [runtime](https://github.com/e2b-dev/runtime) open-sources its
     control plane (officially single-node).

   Differently ran its own Firecracker node agent until
   [ADR 0020](adr/0020-opensandbox-replaces-node-agent.md); OpenSandbox is
   now its default private runtime.
2. **Nobody routes sandboxes across your own runtimes and other vendors by
   policy, under a control plane you run.**
   - Vendors route only among their own backends.
   - Multi-provider tools are client-side libraries
     ([ComputeSDK](https://github.com/computesdk/computesdk)) or agent
     platforms that accept several sandbox providers.

   Differently's position is server-side placement driven by policy and
   data classification, with per-sandbox placement explanations, tenancy,
   quotas and fair queueing, a tamper-evident audit chain and an operator
   trust report.
3. **The position is real but copyable.** OpenSandbox or CubeSandbox could
   add external providers. Differently's durable advantages have to be
   execution quality:
   - operability (one Helm install, preflight, doctor);
   - honest security boundaries;
   - a clean adapter contract that others can implement.

## Ownership models

"Self-hosted" in this document means you can run and operate the control
plane with no vendor service involved. "BYOC" means your compute with the
vendor running or managing the control plane.

| Platform | License | Isolation | Self-hosted control plane | BYOC | Routes to other vendors |
|---|---|---|---|---|---|
| **Differently** | Apache-2.0 | Whatever each pool's runtime provides: OpenSandbox (containers or Fast Sandbox microVMs) by default, plus external pools | Yes | n/a | **Yes, by policy, fail-closed** |
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
- **OpenSandbox is the default runtime, not a competitor to beat.** The
  `opensandbox` adapter places Differently sandboxes on an OpenSandbox
  server the operator runs. Differently adds the tenancy, quotas, policy,
  audit and trust reporting around it, and the same API can also reach
  Daytona, Freestyle and plugins.

  Isolation is OpenSandbox's: its Docker runtime is verified end to end
  under Differently, while its Kubernetes and Fast Sandbox runtimes are not
  yet ([spike](research/opensandbox-spike.md)). Upstream changes to its
  lifecycle or execd APIs are a dependency risk. The adapter is written
  against its published specs and checked live, and the e2e suite pins a
  server release.
- **CubeSandbox is the closest other open-source peer.** It would make a
  good second private-runtime adapter. If OpenSandbox or CubeSandbox adds
  policy-gated external providers, Differently's differentiation narrows to
  operability and trust tooling.
- **agent-sandbox is a possible substrate.** It standardizes a Sandbox CRD with
  warm pools; a future Differently adapter could target it for organizations
  that want gVisor/Kata isolation through Kubernetes RuntimeClasses.

## Runtimes considered, and why OpenSandbox is the default

| Option | Outcome |
|---|---|
| Differently's own Firecracker node agent | Built and verified end to end on arm64, then removed ([ADR 0020](adr/0020-opensandbox-replaces-node-agent.md)). It duplicated what OpenSandbox maintains (per-node sandbox management, pause/resume, templates, snapshots) and pulled engineering away from orchestration. |
| OpenSandbox | Default private runtime. Self-hostable, Apache-2.0, published lifecycle and execd specs, and a choice of Docker, Kubernetes or Fast Sandbox (Firecracker) runtimes behind one API. |
| CubeSandbox | Candidate second private-runtime adapter; Kubernetes deployment in preview. |
| Kata Containers + Firecracker | Needs a hand-configured devmapper thin-pool, has no filesystem sharing, its Firecracker profile is amd64-only, and Kata offers no checkpoint/restore, so pause, snapshot and fork would be lost. |
| firecracker-containerd | No published releases; pinned to containerd 1.7, which reached end of life on 2026-09-30. |
| KubeVirt | Full VMs with heavier boot and density trade-offs; a possible later adapter for long-lived VMs. |
| Kueue | Quota and admission for batch Jobs/JobSets/pods; preemption deletes pods, which does not fit interactive, stateful sandboxes. Differently keeps its own quota and fair-queueing layer. |

## Where KVM is available (affects where microVM runtimes such as Fast Sandbox can run)

| Environment | Status |
|---|---|
| AWS | `.metal` instances; nested virtualization now available on Intel 7th/8th-gen C/M/R 7i and 8i families (EKS managed node group support reported, verify before relying on it). |
| GCP | Nested virtualization on Intel series and AMD N4D; GKE needs privileged pods (Autopilot only for allowlisted workloads). |
| Azure | Nested virtualization on selected sizes (for example Dsv6); KVM inside Linux VMs works but is not officially supported. AKS Pod Sandboxing uses Microsoft's hypervisor, not KVM, so it cannot host Firecracker-based runtimes. |
| Bare metal | k3s, RKE2 and Talos work; Talos and RKE2's CIS profile block privileged pods by default, so a microVM runtime's namespace needs an explicit exemption. |

Step-by-step guides: [deploy/](deploy/README.md).
