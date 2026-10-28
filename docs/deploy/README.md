# Deploying Differently

Differently is a self-hosted sandbox orchestration platform, licensed under
Apache-2.0. It has two parts:

- **Control plane, `differently-server`:** a Go service. It serves the HTTP API,
  the built-in dashboard (`/dashboard/`) and Prometheus metrics (`/metrics`) on
  port 8080. It keeps state in PostgreSQL (or in memory for evaluation),
  places sandboxes on pools by policy, and keeps a tamper-evident audit chain.
- **Node agent, `differently-agent`:** runs on each KVM-capable host and starts
  Firecracker microVMs there. It listens on port 9090 and sends heartbeats to the
  control plane. Inside each microVM, a guest agent (`differently-guest`) answers
  on vsock port 10000.

Pools are where sandboxes run:

- **`firecracker`:** your own KVM hosts, `trust=private`.
- **`daytona`, `freestyle`:** optional external providers. These are always
  `trust=external`, and sandboxes are placed on them only when policy allows it.

```
             clients, CLI, dashboard
                       |
                       | HTTPS (ingress) / :8080
                       v
  +----------------------------------------+      +---------------------+
  | differently-server (Deployment, N pods) |----->| PostgreSQL          |
  |   API, dashboard, scheduler, audit      |      | (external / CNPG)   |
  +----------------------------------------+      +---------------------+
        |  :9090 (agent token)        ^  heartbeats (agent token)
        v                             |
  +-------------------------------+   |        policy-gated, disabled by default
  | differently-agent (DaemonSet) |---+        +------------------------------+
  |   on nodes differently.dev/   |            | Daytona / Freestyle (HTTPS)  |
  |   kvm=true, privileged        |            +------------------------------+
  |   Firecracker microVMs        |
  |     differently-guest (vsock) |
  +-------------------------------+
```

## Fail-closed defaults

- No external provider is registered. The global policy is `external: never`,
  so a sandbox can only land on a private pool, whatever its classification.
- Agents schedule only on nodes labelled `differently.dev/kvm=true`, and
  tolerate the matching taint.
- The chart's NetworkPolicy lets agents accept connections only from
  control-plane pods.
- Secrets are never written into the server config. Pools reference
  credentials by environment variable, and the chart keeps tokens in Secrets.
- Differently has no telemetry or phone-home code paths (see [air-gapped.md](air-gapped.md)).

## Guides

| Guide | Use it for |
|---|---|
| [prerequisites.md](prerequisites.md) | KVM checks, node labels and taints, kernel and root filesystem, ports |
| [kubernetes.md](kubernetes.md) | Installing on an existing cluster with Helm, step by step to the first sandbox |
| [aws.md](aws.md) | EKS with `.metal` or nested-virtualization node groups (Terraform in `deploy/terraform/aws`) |
| [gcp.md](gcp.md) | GKE Standard with a nested-virtualization node pool |
| [azure.md](azure.md) | AKS with nested-virtualization-capable sizes (experimental) |
| [bare-metal.md](bare-metal.md) | k3s, RKE2, Talos, and plain Linux hosts with systemd |
| [air-gapped.md](air-gapped.md) | Mirroring images and the chart; running with no external egress |
| [day-2.md](day-2.md) | Upgrade, rollback, migrations, backup and restore, certificates, node drain, uninstall |

Deployment artifacts:

| Path | Contents |
|---|---|
| `deploy/helm/differently/` | Helm chart (server, agent DaemonSet, optional embedded PostgreSQL, NetworkPolicy, ServiceMonitor, `helm test`) |
| `deploy/terraform/aws/` | EKS cluster with system and KVM node groups |
| `deploy/systemd/` | Unit file, env file example and installer for plain Linux KVM hosts |
| `deploy/images/` | Guest kernel and root filesystem build scripts |

## Support matrix

Status values:

- **tested:** a maintainer has run it end to end on real infrastructure.
- **documented:** steps are written from vendor documentation and this code,
  but have not been executed.
- **unverified:** depends on a claim from research or secondary sources that
  still needs a hands-on check.

**Nothing is tested on real infrastructure yet.** The chart has only been
checked locally: `helm lint` and `helm template` pass for the default,
external-PostgreSQL-plus-ingress and hybrid-Daytona value sets, and the
rendered `config.yaml` parses with the server's own strict config loader.
No manifest has been applied to a cluster.

| Environment | KVM path | Status | Notes |
|---|---|---|---|
| Any conformant Kubernetes 1.29+ with KVM nodes | Privileged agent DaemonSet | documented | [kubernetes.md](kubernetes.md) |
| AWS EKS, `.metal` node group | Bare metal | documented | Matches Firecracker's tested platforms. The Terraform module is **unvalidated** |
| AWS EKS, nested-virtualization node group | `NestedVirtualization` CPU option (M7i/M8i/C7i/C8i/R7i/R8i…) | unverified | EKS managed node group support was reported fixed in 2026. Firecracker calls nested a development setup |
| AWS with Karpenter | `EC2NodeClass` nested virtualization (v1.13.0+) | unverified | Not covered by the Terraform module |
| GCP GKE Standard | Nested virtualization node pool (Intel; not E2, Arm or most AMD) | unverified | Set at pool creation, can't be changed later. Privileged pods required. CPU-bound work runs 10%+ slower |
| GCP GKE Autopilot | ComputeClasses (GKE 1.37.0-gke.4713000+) | unverified, not recommended | Privileged pods only for allowlisted workloads |
| GCP bare metal | `-metal` machine types | unverified | GKE does not support C4/Z3 `-metal`. C3 `-metal` on GKE is unknown |
| Azure AKS | Nested-capable sizes such as Dsv6 | unverified | KVM inside Azure Linux VMs is described as not officially supported. There is no AKS doc for `/dev/kvm` |
| Azure AKS Pod Sandboxing | Kata on MSHV | not a KVM path | The Firecracker agent cannot use it |
| k3s, RKE2 | Privileged DaemonSet | documented | Pod Security Admission notes in [bare-metal.md](bare-metal.md) |
| Talos | Privileged DaemonSet | unverified | Running Firecracker from a container without extra kernel modules is untested |
| Plain Linux + systemd | Host KVM | documented | [bare-metal.md](bare-metal.md) |
| Air-gapped | Any of the above | documented | [air-gapped.md](air-gapped.md) |
| arm64 KVM hosts (Graviton `.metal`) | Bare metal | unverified | Firecracker supports aarch64. Multi-arch agent images are assumed |
| Daytona, Freestyle pools | Provider APIs | unverified | Adapters are checked against the providers' published OpenAPI specs, not live accounts |

## Security model in one paragraph

Root on a KVM node and Kubernetes cluster-admins can read guest memory and
disks. Differently does not protect workloads from the infrastructure
operator, and a private pool is only as private as the hardware under it.
External pools see the code, data and environment of the sandboxes placed on
them.

Traffic between the control plane and agents is plain HTTP. Nodes register
with the shared agent token, and each agent is then bound to its own secret,
so a node cannot impersonate another, but the traffic is not encrypted. Keep
it on a private network, or use an encrypting CNI or mesh.

`/metrics` has no authentication; the chart serves it on a separate port
(`metrics.port`, default 9091) that the ingress does not expose. Sandbox egress is controlled only by the node
networking you configure. Block the cloud metadata endpoint
(`169.254.169.254`) from sandboxes.

`GET /v1/admin/trust-report` shows the live trust posture, including which
external pools policy currently approves.
