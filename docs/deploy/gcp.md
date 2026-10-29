# Google Cloud (GKE)

> **Quickest path:** a single Compute Engine VM with k3s, following
> [One Linux VM on any cloud](single-vm.md). Use this page when you run a
> managed Kubernetes cluster.

Status: **unverified**. No step here has been run.

Differently's control plane runs on any GKE node pool. Sandboxes run on an
OpenSandbox server you install in the cluster
([kubernetes.md, step 2](kubernetes.md#2-install-opensandbox-and-choose-its-runtime)).
The Docker and Kubernetes runtimes need nothing special from GKE. The KVM
notes below apply only if OpenSandbox uses Fast Sandbox (Firecracker
microVMs), which GCP can offer only through nested virtualization on GKE
Standard. Firecracker documents nested virtualization as a development setup,
not a validated production platform.

## Where KVM is available on GCP (Fast Sandbox only)

| Path | Details |
|---|---|
| **GKE Standard, nested virtualization** | Enabled per node pool **at creation and immutable afterwards**. Intel VT-x, or AMD-V on N4D only. Not on E2, memory-optimized or Arm series, and not on Windows node pools. Linux KVM is the only L1 hypervisor. CPU-bound work runs 10% or more slower. Privileged pods are required. An organization policy can block it |
| GKE Autopilot | Only through ComputeClasses on GKE 1.37.0-gke.4713000+, and Autopilot allows privileged pods only for allowlisted workloads. **Not recommended** |
| Compute Engine bare metal (`-metal`: C3, C4, C4A, C4D, X4, X5, Z3) | No Persistent Disk, IDPF NIC only, no live migration. **GKE does not support C4, Z3, Z4D or Z4M `-metal`.** C3 `-metal` on GKE is unknown. Use these as plain Linux hosts for an OpenSandbox server ([bare-metal.md](bare-metal.md#register-an-opensandbox-server-on-a-plain-linux-host)) |

The GKE ComputeClass example lists C4D, an AMD series, while Compute Engine
documents only N4D among AMD series for nested virtualization. Which page is
current is **unverified**. Prefer Intel series (N2, C3, C4, N4).

Google's support does not cover software you run under nested virtualization.

## 1. Allow nested virtualization (Fast Sandbox only)

If your organization enforces `constraints/compute.disableNestedVirtualization`,
ask for an exception for the project.

## 2. Create a KVM node pool (Fast Sandbox only)

```sh
gcloud container node-pools create kvm \
  --cluster=<cluster> --location=<region-or-zone> \
  --machine-type=n2-standard-16 \
  --image-type=UBUNTU_CONTAINERD \
  --enable-nested-virtualization \
  --num-nodes=1 \
  --node-labels=sandbox-runtime=fast-sandbox \
  --node-taints=sandbox-runtime=fast-sandbox:NoSchedule \
  --disk-type=pd-ssd --disk-size=500
```

- The label and taint are examples. Differently does not read them; make
  OpenSandbox's installation select and tolerate whatever you choose.
- The `--enable-nested-virtualization` flag and its interaction with
  `--image-type` are **unverified**. Check the current GKE how-to for nested
  virtualization before running this.
- Ubuntu node images are chosen because their kernel ships the KVM modules.
  Whether Container-Optimized OS works is **unverified**.
- Check the rest of Fast Sandbox's host requirements on a node (kernel
  version, cgroup v2, an XFS image volume with reflink, `/dev/shm` size):
  [prerequisites.md](prerequisites.md#kvm-hosts-fast-sandbox-only).

## 3. Database

Use Cloud SQL for PostgreSQL (or AlloyDB) with a private IP in the cluster's
VPC, and require TLS:

```sh
kubectl create namespace differently
kubectl -n differently create secret generic differently-database \
  --from-literal=database-url='postgres://differently:PASSWORD@10.x.y.z:5432/differently?sslmode=require'
```

The chart has no Cloud SQL Auth Proxy sidecar. Connect over private IP, and
allow the instance's range in `networkPolicy.server.egress.databaseCIDRs`.

## 4. Install OpenSandbox

Follow OpenSandbox's documentation and
[kubernetes.md, step 2](kubernetes.md#2-install-opensandbox-and-choose-its-runtime),
then store its API key for Differently:

```sh
kubectl -n differently create secret generic opensandbox-api-key --from-literal=api-key='<api_key>'
```

## 5. Install Differently

```yaml
# values-gke.yaml
config:
  store: {driver: postgres}
postgresql:
  external: {existingSecret: differently-database}
opensandbox:
  apiURL: http://opensandbox-server.opensandbox-system.svc/v1
  trust: private
  isolation: container                 # microvm with Fast Sandbox
  existingSecret: opensandbox-api-key
server:
  replicas: 3
networkPolicy:
  server:
    egress:
      databaseCIDRs: ["10.x.y.0/24"]
ingress:
  enabled: true
  className: gce                       # or your controller
  hosts: [differently.example.com]
  tls:
    enabled: true
    certManager: {clusterIssuer: letsencrypt-prod}
```

```sh
helm install dly deploy/helm/differently -n differently -f values-gke.yaml
```

NetworkPolicy is enforced with GKE Dataplane V2, or with network policy
enforcement enabled on the cluster. Then continue with steps 6 to 8 of
[kubernetes.md](kubernetes.md).

## Security notes for GCP

- Block the metadata server (`169.254.169.254` and `metadata.google.internal`)
  from sandboxes, in OpenSandbox's network configuration or with
  NetworkPolicy in its namespace. Use Workload Identity, so node credentials
  are not what the metadata server hands out to pods.
- With Fast Sandbox, nested guests share the L1 VM's kernel trust boundary
  with OpenSandbox's node components. Keep the KVM pool dedicated (taint) and
  its service account minimal.

## Verify

```sh
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/pools" \
  | jq '.items[] | {name, trust, health: .health.status}'
helm test dly -n differently --logs
# Fast Sandbox only:
gcloud container node-pools describe kvm --cluster=<cluster> --location=<loc> \
  --format='value(config.advancedMachineFeatures)'          # nested virtualization enabled
NODE=$(kubectl get nodes -l sandbox-runtime=fast-sandbox -o jsonpath='{.items[0].metadata.name}')
kubectl debug node/$NODE -it --image=busybox:1.37 --profile=sysadmin -- \
  sh -c 'ls -l /host/dev/kvm; grep -c vmx /host/proc/cpuinfo; uname -r'
```
