# Google Cloud (GKE)

Status: **unverified**. No step here has been run.

GCP offers KVM to Differently only through nested virtualization on GKE
Standard. Firecracker documents nested virtualization as a development setup,
not a validated production platform.

## Where KVM is available on GCP

| Path | Details |
|---|---|
| **GKE Standard, nested virtualization** | Enabled per node pool **at creation and immutable afterwards**. Intel VT-x, or AMD-V on N4D only. Not on E2, memory-optimized or Arm series, and not on Windows node pools. Linux KVM is the only L1 hypervisor. CPU-bound work runs 10% or more slower. Privileged pods are required. An organization policy can block it |
| GKE Autopilot | Only through ComputeClasses on GKE 1.37.0-gke.4713000+, and Autopilot allows privileged pods only for allowlisted workloads. **Not recommended** |
| Compute Engine bare metal (`-metal`: C3, C4, C4A, C4D, X4, X5, Z3) | No Persistent Disk, IDPF NIC only, no live migration. **GKE does not support C4, Z3, Z4D or Z4M `-metal`.** C3 `-metal` on GKE is unknown. Use these as plain Linux hosts with the systemd agent ([bare-metal.md](bare-metal.md)) |

The GKE ComputeClass example lists C4D, an AMD series, while Compute Engine
documents only N4D among AMD series for nested virtualization. Which page is
current is **unverified**. Prefer Intel series (N2, C3, C4, N4).

Google's support does not cover software you run under nested virtualization.

## 1. Allow nested virtualization

If your organization enforces `constraints/compute.disableNestedVirtualization`,
ask for an exception for the project.

## 2. Create a KVM node pool

```sh
gcloud container node-pools create kvm \
  --cluster=<cluster> --location=<region-or-zone> \
  --machine-type=n2-standard-16 \
  --image-type=UBUNTU_CONTAINERD \
  --enable-nested-virtualization \
  --num-nodes=1 \
  --node-labels=differently.dev/kvm=true \
  --node-taints=differently.dev/kvm=true:NoSchedule \
  --disk-type=pd-ssd --disk-size=500
```

- The `--enable-nested-virtualization` flag and its interaction with
  `--image-type` are **unverified**. Check the current GKE how-to for nested
  virtualization before running this.
- Ubuntu node images are chosen because their kernel ships the KVM modules.
  Whether Container-Optimized OS works is **unverified**.
- Check the host kernel version (`uname -r`) against Firecracker's supported
  list: 5.10, 6.1 or 6.18.

## 3. Database

Use Cloud SQL for PostgreSQL (or AlloyDB) with a private IP in the cluster's
VPC, and require TLS:

```sh
kubectl create namespace differently
kubectl label namespace differently pod-security.kubernetes.io/enforce=privileged
kubectl -n differently create secret generic differently-database \
  --from-literal=database-url='postgres://differently:PASSWORD@10.x.y.z:5432/differently?sslmode=require'
```

The chart has no Cloud SQL Auth Proxy sidecar. Connect over private IP, and
allow the instance's range in `networkPolicy.server.egress.databaseCIDRs`.

## 4. Install

```yaml
# values-gke.yaml
config:
  store: {driver: postgres}
postgresql:
  external: {existingSecret: differently-database}
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
enforcement enabled on the cluster. Then continue with steps 7 to 9 of
[kubernetes.md](kubernetes.md).

## Security notes for GCP

- Block the metadata server (`169.254.169.254` and `metadata.google.internal`)
  from sandboxes. Use Workload Identity, so node credentials are not what the
  metadata server hands out to pods.
- Nested guests share the L1 VM's kernel trust boundary with the agent. Keep
  the KVM pool dedicated (taint) and its service account minimal.

## Verify

```sh
gcloud container node-pools describe kvm --cluster=<cluster> --location=<loc> \
  --format='value(config.advancedMachineFeatures)'          # nested virtualization enabled
NODE=$(kubectl get nodes -l differently.dev/kvm=true -o jsonpath='{.items[0].metadata.name}')
kubectl debug node/$NODE -it --image=busybox:1.37 --profile=sysadmin -- \
  sh -c 'ls -l /host/dev/kvm; grep -c vmx /host/proc/cpuinfo; uname -r'
kubectl -n differently logs ds/dly-differently-agent -c doctor
helm test dly -n differently --logs
```
