# Deploying Differently

## Choose where to run

| You have | Start with | Time to a first sandbox | Status |
|---|---|---|---|
| A laptop | [Quickstart](../guides/quickstart.md) (`differently-server --dev`, no isolation) | 2 minutes | tested |
| Any Docker host | The [end-to-end stack](../../test/e2e/README.md): Postgres, Differently and OpenSandbox in Compose | 10 minutes | tested |
| One Linux VM: Azure, AWS, Google Cloud or bare metal | [One Linux VM on any cloud](single-vm.md): k3s, OpenSandbox's charts, Differently's chart | 30 minutes | tested on Azure |
| A Kubernetes cluster (EKS, GKE, AKS, k3s, RKE2) | [Kubernetes with Helm](kubernetes.md), then the notes for [AWS](aws.md), [Google Cloud](gcp.md), [Azure](azure.md) or [bare metal](bare-metal.md) | about an hour | Helm install tested on k3s; managed clusters documented |
| No internet access | [Air-gapped](air-gapped.md) | | documented |

Every path uses the same chart and images, from the
[latest release](https://github.com/different-ai/differently/releases):
`ghcr.io/different-ai/differently-server:<version>`, with no leading `v`.

Differently is a self-hosted sandbox orchestration platform, licensed under
Apache-2.0. It does not run sandboxes itself. A deployment has two parts:

- **Control plane, `differently-server`:** a Go service. It serves the HTTP API,
  the built-in dashboard (`/dashboard/`) and Prometheus metrics (`/metrics`) on
  port 8080. It keeps state in PostgreSQL (or in memory for evaluation),
  enforces tenancy and quotas, places sandboxes on pools by policy, and keeps
  a tamper-evident audit chain.
- **A sandbox runtime behind each pool.** The default private runtime is an
  [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) server that
  you install and operate yourself, with its own tooling, and register as a
  pool (`adapter: opensandbox`). OpenSandbox runs the sandboxes with the
  runtime you choose for it: Docker or Kubernetes containers, or Firecracker
  microVMs with Fast Sandbox.

Pools are where sandboxes run:

- **`opensandbox`:** an OpenSandbox server. `trust` must be declared:
  `private` if you operate the server and its runtime, `external` if someone
  else does.
- **Managed providers** (`daytona`, `freestyle`, `e2b`, `vercel` and the
  others in the [provider catalog](../providers/README.md)): optional
  capacity someone else runs. Managed kinds are always `trust=external`, and
  sandboxes are placed on them only when policy allows it.
- **`remote`:** an out-of-process adapter plugin speaking the
  [adapter protocol](../adapters/specification.md). Its trust is the
  operator's declaration (default `private`).

```
             clients, CLI, dashboard
                       |
                       | HTTPS (ingress) / :8080
                       v
  +-----------------------------------------+      +---------------------+
  | differently-server (Deployment, N pods) |----->| PostgreSQL          |
  |   API, dashboard, scheduler, audit      |      | (external / CNPG)   |
  +-----------------------------------------+      +---------------------+
        |  :80 (OpenSandbox API key,               policy-gated, disabled by default
        |   by reference)                          +------------------------------+
        v                                          | Daytona / Freestyle (HTTPS)  |
  +-----------------------------------------+      +------------------------------+
  | OpenSandbox server (installed by you)   |
  |   runtime: Docker | Kubernetes |        |
  |            Fast Sandbox (KVM nodes)     |
  |   sandboxes, each with OpenSandbox's    |
  |   execd                                 |
  +-----------------------------------------+
```

Installing means: install OpenSandbox with its own Helm chart or manifests
and pick its runtime, then install Differently's chart pointing at it
([kubernetes.md](kubernetes.md)). Only Fast Sandbox needs KVM hosts; the
cloud KVM notes in [aws.md](aws.md), [gcp.md](gcp.md) and [azure.md](azure.md)
are OpenSandbox prerequisites for that runtime.

## Fail-closed defaults

- No external provider is registered. The global policy is `external: never`,
  so a sandbox can only land on a private pool, whatever its classification.
- An `opensandbox` pool without an explicit `trust` is rejected at start-up,
  because Differently cannot tell who operates the server.
- The chart's NetworkPolicy limits server egress to DNS, the database,
  OpenSandbox (port 80 by default) and enabled providers.
- Secrets are never written into the server config. Pools reference
  credentials by environment variable or file, or name a provider key that
  operators stored in Differently, sealed under the chart's secrets key. The
  chart keeps the bootstrap token, the OpenSandbox API key and the secrets
  key in Secrets.
- Differently has no telemetry or phone-home code paths (see [air-gapped.md](air-gapped.md)).

## Guides

| Guide | Use it for |
|---|---|
| [prerequisites.md](prerequisites.md) | Control-plane requirements, the OpenSandbox server, KVM hosts for Fast Sandbox, ports |
| [single-vm.md](single-vm.md) | One Linux VM on Azure, AWS, Google Cloud or bare metal: k3s, OpenSandbox and Differently from release artifacts |
| [kubernetes.md](kubernetes.md) | Installing on an existing cluster with Helm, step by step to the first sandbox |
| [aws.md](aws.md) | EKS, with `.metal` or nested-virtualization node groups for Fast Sandbox (Terraform in `deploy/terraform/aws`) |
| [gcp.md](gcp.md) | GKE Standard, with a nested-virtualization node pool for Fast Sandbox |
| [azure.md](azure.md) | AKS, with nested-virtualization-capable sizes for Fast Sandbox (experimental) |
| [bare-metal.md](bare-metal.md) | k3s, RKE2, Talos, and an OpenSandbox server on a plain Linux host |
| [air-gapped.md](air-gapped.md) | Mirroring images and the chart; running with no external egress |
| [day-2.md](day-2.md) | Upgrade, rollback, migrations, backup and restore, certificates, replacing a backend, uninstall |
| [../guides/opensandbox-pools.md](../guides/opensandbox-pools.md) | Configuring an OpenSandbox pool and what it supports |

Deployment artifacts:

| Path | Contents |
|---|---|
| `deploy/helm/differently/` | Helm chart: control plane, an `opensandbox` pool (on by default), optional embedded PostgreSQL, NetworkPolicy, ServiceMonitor, `helm test` |
| `deploy/terraform/aws/` | EKS cluster with a system node group, plus a KVM node group for Fast Sandbox only when `kvm_node_group_enabled = true` |
| `test/opensandbox/` | A Docker Compose OpenSandbox server (Docker runtime) for checking the adapter |

The chart does not install OpenSandbox.

## Support matrix

Status values:

- **tested:** a maintainer has run it end to end on real infrastructure.
- **documented:** steps are written from vendor documentation and this code,
  but have not been executed.
- **unverified:** depends on a claim from research or secondary sources, or
  on a runtime nobody has run with Differently yet.

**The Helm chart has run on a real cluster against a live OpenSandbox
server**: single-node k3s on Azure, with OpenSandbox installed from its own
charts and its Kubernetes runtime ([one Linux VM](single-vm.md),
2026-10-28 and 29). The end-to-end suite passed 14/14 there, with stop/start,
snapshots, fork, signed port URLs and egress policy. The published
`v0.1.0-alpha.1` chart and images installed from public artifacts alone. In
CI, `helm lint` and `helm template` pass for every value set in
`deploy/helm/differently/ci/`. Managed EKS, GKE and AKS clusters and
multi-node clusters have not been run.

What has run: the `opensandbox` adapter against a real OpenSandbox server
(release 1.2.0, Docker runtime, `execd` v1.1.0) on a Colima VM (Apple Silicon,
arm64), both through its live test (`go test -tags live
./internal/adapters/opensandbox/`) and through `differently-server` with an
`opensandbox` pool: the pool was healthy, private-first placement chose it,
exec and pause/resume worked, stop was refused as unsupported, the trust
report showed the endpoint and the credential only as a reference, delete
cleaned up in OpenSandbox, and the API key did not appear in the server log.
See the [spike](../research/opensandbox-spike.md). The
[end-to-end suite](../../test/e2e/README.md) runs the real control plane and
Postgres against a real OpenSandbox server with the Docker runtime and passed
10/10 on arm64 Colima on 2026-10-28.

| Environment | Runtime path | Status | Notes |
|---|---|---|---|
| OpenSandbox 1.2.0 with the Docker runtime (Docker Compose on Colima, arm64) | Containers on one Docker host | **tested** (end-to-end suite 14/14, 2026-10-29) | Checks the API contract, not isolation: containers share the host kernel |
| Single-node k3s on an Azure VM, Differently's chart plus OpenSandbox's charts | OpenSandbox Kubernetes runtime (pods) | **tested** (end-to-end suite 14/14, 2026-10-29) | [single-vm.md](single-vm.md). Stop/start, snapshots, fork, port URLs and egress need the [Kubernetes runtime setup](../guides/opensandbox-pools.md#kubernetes-runtime-snapshot-registry-signed-ports-and-egress) |
| Any conformant Kubernetes 1.29+, Differently's chart plus an OpenSandbox server | Whatever OpenSandbox runs | documented | [kubernetes.md](kubernetes.md); multi-node not yet run ([POC 12](../proof-of-concepts.md#poc-12-opensandbox-kubernetes-runtime)) |
| OpenSandbox Fast Sandbox on KVM nodes | Firecracker microVMs | unverified | Not run on any target yet. [POC 11](../proof-of-concepts.md#poc-11-fast-sandbox-through-the-adapter), [backlog E18](../backlog.md#e18-fast-sandbox-spike) |
| AWS EKS, `.metal` node group (Fast Sandbox) | Bare metal | unverified | Matches Firecracker's tested platforms. The Terraform module is **unvalidated** |
| AWS EKS, nested-virtualization node group (Fast Sandbox) | `NestedVirtualization` CPU option (M7i/M8i/C7i/C8i/R7i/R8i…) | unverified | EKS managed node group support was reported fixed in 2026. Firecracker calls nested a development setup |
| AWS with Karpenter (Fast Sandbox) | `EC2NodeClass` nested virtualization (v1.13.0+) | unverified | Not covered by the Terraform module |
| GCP GKE Standard (Fast Sandbox) | Nested virtualization node pool (Intel; not E2, Arm or most AMD) | unverified | Set at pool creation, can't be changed later. CPU-bound work runs 10%+ slower |
| GCP GKE Autopilot (Fast Sandbox) | ComputeClasses (GKE 1.37.0-gke.4713000+) | unverified, not recommended | Privileged pods only for allowlisted workloads |
| GCP bare metal (Fast Sandbox) | `-metal` machine types | unverified | GKE does not support C4/Z3 `-metal`. C3 `-metal` on GKE is unknown |
| Azure AKS (Fast Sandbox) | Nested-capable sizes such as Dsv6 | unverified | KVM inside Azure Linux VMs is described as not officially supported. There is no AKS doc for `/dev/kvm` |
| Azure AKS Pod Sandboxing | Kata on MSHV | not a KVM path; unverified for Kata | Fast Sandbox needs KVM, but OpenSandbox's Kubernetes runtime can run sandboxes as Kata VMs here (its AKS example); not run with Differently |
| k3s, RKE2 | Differently's chart; OpenSandbox per its docs | documented | Pod Security Admission notes in [bare-metal.md](bare-metal.md) |
| Talos | Differently's chart; OpenSandbox per its docs | unverified | |
| OpenSandbox on a plain Linux host, Differently elsewhere | Docker runtime on that host | documented | [bare-metal.md](bare-metal.md#register-an-opensandbox-server-on-a-plain-linux-host) |
| Air-gapped | Any of the above | documented | [air-gapped.md](air-gapped.md) |
| Managed provider pools | Provider APIs | unverified | Adapters are checked against each provider's published API with fake servers, not live accounts ([provider catalog](../providers/README.md)) |

## Security model in one paragraph

Differently does not isolate sandboxes itself. Isolation and network egress
come from each pool's runtime: with OpenSandbox's Docker or Kubernetes
runtime, sandboxes are containers that share the host kernel; Fast Sandbox
gives each one a Firecracker microVM, but that path is unverified. Whoever
operates the OpenSandbox server and its hosts (and any cloud or external
provider) can read the workloads it runs. The OpenSandbox API key is held by
the control plane as an environment variable or file and never reaches
tenants or sandboxes; the adapter refuses port endpoints that would need it.
Differently does not wire OpenSandbox's network policy yet, so restrict
sandbox egress in OpenSandbox's own configuration, with Kubernetes
NetworkPolicy or with host and VPC firewalls, and always block cloud metadata
endpoints (`169.254.169.254`). Keep control-plane-to-OpenSandbox traffic on a
private network or use an `https` `apiURL`. `/metrics` has no
authentication; the chart serves it on a separate port (`metrics.port`,
default 9091) that the ingress does not expose. `GET /v1/admin/trust-report`
shows the live trust posture: each pool's declared isolation, endpoint and
credential reference, which external pools policy currently approves, and the
residual risks Differently cannot control. More in the
[trust model](../security/trust-model.md).
