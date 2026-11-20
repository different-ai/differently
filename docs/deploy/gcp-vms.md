# Google Cloud VMs with Fast Sandbox microVMs

The [Azure VMs layout](azure-vms.md) on Compute Engine: two VMs with k3s,
OpenSandbox with both its Kubernetes runtime (containers) and Fast Sandbox
(Firecracker microVMs), Differently in front, and managed Google Cloud
services around it. Use it to run that layout on Google Cloud, or as the
target when moving an installation from Azure to Google Cloud.

> **Status: documented, not run.** Nothing on this page has been run on
> Google Cloud. Every step mirrors one that ran on Azure on 2026-10-29
> ([azure-vms.md](azure-vms.md#verify)). The `gcloud` commands were checked
> against Google Cloud SDK 567.0.0's `--help` for every flag used, not
> executed. Google Cloud facts come from Google's documentation in
> November 2026. Items marked **unverified** are the ones most likely to need
> a fix on a first run.

| Piece | Azure (verified) | Google Cloud (this page) |
|---|---|---|
| Control node | `Standard_D8s_v6` (8 vCPU, 32 GiB) | `n4-standard-8` (8 vCPU, 32 GiB) |
| KVM node | `Standard_D16s_v6` (16 vCPU, 64 GiB), nested KVM | `n4-standard-16` (16 vCPU, 64 GiB), `--enable-nested-virtualization` |
| Fast Sandbox state root | 256 GiB Premium SSD P15, XFS with reflink | 256 GiB Hyperdisk Balanced with provisioned throughput, XFS with reflink |
| Database | PostgreSQL Flexible Server, private access | Cloud SQL for PostgreSQL, private IP (private services access) |
| Snapshot registry | Azure Container Registry, repository-scoped token | Artifact Registry, two Docker repositories, a service account per repository |
| Secrets | Key Vault through the Secrets Store CSI driver, the node's managed identity | Secret Manager through the Secrets Store CSI driver, the node's service account (`auth: provider-adc`) |
| Ingress | Caddy on each node, Let's Encrypt, two host names | The same, on two static external IPs |
| Metadata endpoint to block | `169.254.169.254` and the wire server `168.63.129.16` | `169.254.169.254` (`metadata.google.internal`) |

Nothing in Differently's code or chart is specific to either cloud. The
differences are in the infrastructure and in OpenSandbox's registry settings.

Throughout, `<project>`, `<region>` and `<zone>` are yours, for example
`us-east4` and `us-east4-a`. Keep both VMs and the database in one region.

## 1. Project, quota and organization policy

```sh
gcloud config set project <project>
gcloud services enable compute.googleapis.com sqladmin.googleapis.com \
  servicenetworking.googleapis.com artifactregistry.googleapis.com \
  secretmanager.googleapis.com
```

- **Machine series in the zone.** Check that N4 is offered:
  `gcloud compute machine-types list --filter="name=n4-standard-16 AND zone=<zone>"`.
- **Nested virtualization** is supported on Intel series such as N4, C3, C4
  and N2 (and on AMD N4D only). It is not available on E2, Arm, or most AMD
  series. An organization policy can forbid it
  (`constraints/compute.disableNestedVirtualization`, or the managed
  `constraints/compute.managed.disableNestedVirtualization`); ask for an
  exception for the project if it is enforced. Linux KVM is the only
  supported guest hypervisor, and Google's support does not cover software
  running under nested virtualization. Firecracker documents nested
  virtualization as a development setup. The verified Azure layout runs
  nested KVM as well, so this is the same situation, not a new one.
- **Quota.** This layout needs 24 N4 vCPUs in the region. New projects often
  allow fewer: check **IAM & Admin → Quotas** for `N4 CPUs` and `CPUs` in
  the region, and request an increase before you start.
- **Service account keys.** Step 4 uses one service account key for registry
  access. Organizations created since 2024 enforce
  `constraints/iam.disableServiceAccountKeyCreation` by default; read step 4
  before you start if yours does.

## 2. Network, addresses and firewall

```sh
gcloud compute networks create differently --subnet-mode=custom
gcloud compute networks subnets create differently-nodes \
  --network=differently --region=<region> --range=10.20.0.0/24 \
  --enable-private-ip-google-access

# Two public IPs: sandbox content must not share an origin with the API.
gcloud compute addresses create dly-api dly-sandbox --region=<region>

# Private services access for Cloud SQL. Keep the range inside 10.0.0.0/8,
# so the sandbox NetworkPolicies of step 7 already exclude it.
gcloud compute addresses create differently-psa --global \
  --purpose=VPC_PEERING --addresses=10.30.0.0 --prefix-length=20 \
  --network=differently
gcloud services vpc-peerings connect --service=servicenetworking.googleapis.com \
  --ranges=differently-psa --network=differently

gcloud compute firewall-rules create dly-web --network=differently \
  --direction=INGRESS --allow=tcp:80,tcp:443 --source-ranges=0.0.0.0/0 \
  --target-tags=differently-control,differently-kvm
gcloud compute firewall-rules create dly-ssh-iap --network=differently \
  --direction=INGRESS --allow=tcp:22 --source-ranges=35.235.240.0/20 \
  --target-tags=differently-control,differently-kvm
gcloud compute firewall-rules create dly-nodes --network=differently \
  --direction=INGRESS --allow=tcp,udp,icmp --source-ranges=10.20.0.0/24 \
  --target-tags=differently-control,differently-kvm
```

- 443 is public for the same reason as on Azure: people's browsers open port
  URLs, and clients call the API. Every API call still needs a key.
- SSH is allowed only from Identity-Aware Proxy's range
  (`gcloud compute ssh --tunnel-through-iap`); no SSH port faces the
  internet.
- `dly-nodes` lets the two nodes reach each other (k3s API 6443, flannel
  VXLAN 8472/udp, kubelet 10250, the KVM node's private registry). Pod
  traffic between nodes travels inside VXLAN on node addresses.
- Point two DNS names at the two addresses: the API host at `dly-api`, the
  sandbox host at `dly-sandbox`. Use names in a domain you control, so a
  later move is a DNS change rather than a client change.

## 3. VMs

Give the control node its own service account with no roles yet (step 5
grants it access to its secrets, and nothing else). Do not use the Compute
Engine default service account, which often holds the project-wide Editor
role. The KVM node gets no service account at all: nothing on it needs
Google APIs, and a sandbox that escaped to the host would find no
credentials on the metadata server.

```sh
gcloud iam service-accounts create dly-control --display-name="Differently control node"

gcloud compute instances create dly-control --zone=<zone> \
  --machine-type=n4-standard-8 \
  --image-family=ubuntu-2404-lts-amd64 --image-project=ubuntu-os-cloud \
  --boot-disk-type=hyperdisk-balanced --boot-disk-size=128GB \
  --subnet=differently-nodes --private-network-ip=10.20.0.10 \
  --address=dly-api --tags=differently-control \
  --service-account=dly-control@<project>.iam.gserviceaccount.com \
  --scopes=cloud-platform --shielded-secure-boot

gcloud compute instances create dly-kvm --zone=<zone> \
  --machine-type=n4-standard-16 --enable-nested-virtualization \
  --image-family=ubuntu-2404-lts-amd64 --image-project=ubuntu-os-cloud \
  --boot-disk-type=hyperdisk-balanced --boot-disk-size=128GB \
  --create-disk=name=dly-kvm-state,device-name=fsb-state,type=hyperdisk-balanced,size=256GB,provisioned-throughput=400,auto-delete=no \
  --subnet=differently-nodes --private-network-ip=10.20.0.11 \
  --address=dly-sandbox --tags=differently-kvm \
  --no-service-account --no-scopes
```

- N4 takes only Hyperdisk, so both boot disks are `hyperdisk-balanced`.
- **Pause time is disk throughput.** Every Fast Sandbox pause writes, reads
  back and uploads the whole checkpoint (14 GB for a Workbot desktop) through
  the state-root disk. On Azure that disk served about 175 MB/s and a pause
  took 174–186 s
  ([measurements](../research/fast-sandbox-pause-time.md)). Hyperdisk
  Balanced lets you provision throughput separately from size, up to the
  VM's own limit (check
  [Hyperdisk performance limits](https://cloud.google.com/compute/docs/disks/hyperdisk-perf-limits)
  for `n4-standard-16`). 400 MiB/s is a starting point. That pauses get
  shorter in proportion is an **estimate**, not a measurement.
- Whether Shielded VM's secure boot interferes with loading `kvm_intel` is
  **unverified**, so the KVM node above leaves it off. Ubuntu's KVM modules
  are signed, so turning it on probably works.

On the KVM node, check KVM and prepare the state root:

```sh
gcloud compute ssh dly-kvm --zone=<zone> --tunnel-through-iap
ls -l /dev/kvm && grep -c vmx /proc/cpuinfo          # the device exists; VT-x is exposed
sudo mkfs.xfs -m reflink=1 /dev/disk/by-id/google-fsb-state
sudo mkdir -p /var/lib/fast-sandbox
echo '/dev/disk/by-id/google-fsb-state /var/lib/fast-sandbox xfs defaults,nofail 0 2' | sudo tee -a /etc/fstab
sudo mount /var/lib/fast-sandbox
```

Check the rest of Fast Sandbox's host requirements there (kernel, cgroup v2,
`/dev/net/tun`, `/dev/shm` size):
[prerequisites.md](prerequisites.md#kvm-hosts-fast-sandbox-only).

## 4. Database

```sh
gcloud sql instances create differently-pg --region=<region> \
  --database-version=POSTGRES_17 --edition=enterprise --tier=db-custom-2-8192 \
  --network=differently --no-assign-ip --ssl-mode=ENCRYPTED_ONLY \
  --availability-type=zonal --enable-point-in-time-recovery \
  --backup-start-time=03:00 --storage-auto-increase --deletion-protection
gcloud sql databases create differently --instance=differently-pg
gcloud sql users create differently --instance=differently-pg --password='<password>'
gcloud sql instances describe differently-pg --format='value(ipAddresses[0].ipAddress)'
```

- When moving an installation, use the same major version as the source
  database (`SHOW server_version;` there). Differently's migrations need no
  extension and no superuser.
- `--availability-type=regional` adds a standby in another zone. The Azure
  layout's database was not highly available either.
- The database URL is
  `postgres://differently:<password>@<private IP>:5432/differently?sslmode=require`.
  For `verify-full`, download the instance's server CA
  (`gcloud sql instances describe differently-pg --format='value(serverCaCert.cert)'`)
  and mount it ([certificates](day-2.md#certificates)).
- The chart has no Cloud SQL Auth Proxy sidecar, and needs none: the server
  connects to the private IP directly.

## 5. k3s and registry access

Install k3s with `--disable traefik --disable servicelb` on the control node
and join the KVM node as an agent over the private network
(`K3S_URL=https://10.20.0.10:6443`). Label the nodes as on Azure
(`differently.dev/role=control`, `sandbox-runtime=fast-sandbox`).

Create two Docker repositories:

```sh
gcloud artifacts repositories create opensandbox-snapshots \
  --repository-format=docker --location=<region>
gcloud artifacts repositories create images \
  --repository-format=docker --location=<region>
```

The image committer pushes each sandbox's snapshot to its own image under
`<region>-docker.pkg.dev/<project>/opensandbox-snapshots/`. Artifact
Registry grants access per repository, and images nested under it inherit
it, so the wildcard scope Azure needed (`opensandbox-snapshots/*`) is
implicit here.

Give the cluster one service account that can manage snapshots and read
images. Deleting a snapshot needs more than `artifactregistry.writer`:

```sh
gcloud iam service-accounts create dly-registry --display-name="Differently cluster registry"
gcloud artifacts repositories add-iam-policy-binding opensandbox-snapshots --location=<region> \
  --member=serviceAccount:dly-registry@<project>.iam.gserviceaccount.com \
  --role=roles/artifactregistry.repoAdmin
gcloud artifacts repositories add-iam-policy-binding images --location=<region> \
  --member=serviceAccount:dly-registry@<project>.iam.gserviceaccount.com \
  --role=roles/artifactregistry.reader
gcloud iam service-accounts keys create dly-registry.json \
  --iam-account=dly-registry@<project>.iam.gserviceaccount.com
```

Use the key as a password with the user name `_json_key_base64`, both in
each node's `/etc/rancher/k3s/registries.yaml`:

```yaml
configs:
  "<region>-docker.pkg.dev":
    auth:
      username: _json_key_base64
      password: "<base64 -w0 dly-registry.json>"
```

and in the docker-registry Secret that OpenSandbox's controller uses to push
and pull snapshots (in the `opensandbox` namespace):

```sh
kubectl -n opensandbox create secret docker-registry ar-snapshots \
  --docker-server=<region>-docker.pkg.dev \
  --docker-username=_json_key_base64 \
  --docker-password="$(base64 -w0 dly-registry.json)"
shred -u dly-registry.json
```

If your organization forbids service account keys, there is no static
credential for Artifact Registry. The alternative is a short-lived access
token (user name `oauth2accesstoken`, valid for about an hour) that a
CronJob on the control node refreshes from the metadata server into the
Secret, plus a kubelet image credential provider for node pulls. That path
is **unverified** with OpenSandbox. Whether its controller picks up a
replaced Secret without a restart is also unknown.

## 6. OpenSandbox and Fast Sandbox

Exactly as in [azure-vms.md, steps 3 and 4](azure-vms.md#3-opensandbox-kubernetes-runtime),
with these substitutions:

- controller: `snapshot.registry: <region>-docker.pkg.dev/<project>/opensandbox-snapshots`,
  and `snapshotPushSecret` and `resumePullSecret` naming `ar-snapshots`;
- `gateway.host` for both servers is the sandbox host name of step 2;
- the template builder's anonymous source registry (`registry:2`) binds to
  the KVM node's private address `10.20.0.11`, which no firewall rule
  exposes. Copy template images into it from the `images` repository. Use
  the `stage` containerd namespace as on Azure, because k3s discards layers
  it has unpacked;
- the Garage artifact store, the guest kernel with nftables, the idle
  sweeper, the template store on a volume, the warm SandboxPool and the
  one-desktop-template-per-node rule carry over unchanged. On
  `n4-standard-16` the same three fastlets of two 2 vCPU / 4 GiB sandboxes
  fit as on `Standard_D16s_v6`: both have 16 vCPUs and 64 GiB.

## 7. Secret Manager through the CSI driver

Store each value without a trailing newline, in the region:

```sh
for name in differently-database-url differently-bootstrap-token \
            differently-secrets-key osb-api-key osb-fsb-api-key; do
  printf '%s' "<value of $name>" | gcloud secrets create "$name" \
    --replication-policy=user-managed --locations=<region> --data-file=-
  gcloud secrets add-iam-policy-binding "$name" \
    --member=serviceAccount:dly-control@<project>.iam.gserviceaccount.com \
    --role=roles/secretmanager.secretAccessor
done
```

When moving an installation, `differently-secrets-key` and
`differently-bootstrap-token` must hold the old installation's exact values:
without the same secrets key, the provider credentials stored in the database
cannot be opened.

Install the Secrets Store CSI driver with Secret syncing
(`helm install csi-secrets-store secrets-store-csi-driver/secrets-store-csi-driver -n kube-system --set syncSecret.enabled=true`
from `https://kubernetes-sigs.github.io/secrets-store-csi-driver/charts`)
and Google's provider, from
[its repository](https://github.com/GoogleCloudPlatform/secrets-store-csi-driver-provider-gcp)
(`deploy/provider-gcp-plugin.yaml`, or the chart in its `charts/` directory).
Pin the provider to the control node, then:

```yaml
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata: {name: differently-kv, namespace: differently}
spec:
  provider: gcp
  parameters:
    auth: provider-adc            # the control node's service account, from the metadata server
    secrets: |
      - resourceName: projects/<project>/secrets/differently-database-url/versions/latest
        path: database-url
      - resourceName: projects/<project>/secrets/differently-bootstrap-token/versions/latest
        path: bootstrap-token
      - resourceName: projects/<project>/secrets/differently-secrets-key/versions/1   # pinned
        path: secrets-key
      - resourceName: projects/<project>/secrets/osb-api-key/versions/latest
        path: api-key
      - resourceName: projects/<project>/secrets/osb-fsb-api-key/versions/latest
        path: osb-fsb-api-key
  secretObjects:
    - secretName: differently-kv
      type: Opaque
      data:
        - {objectName: database-url, key: database-url}
        - {objectName: bootstrap-token, key: bootstrap-token}
        - {objectName: secrets-key, key: secrets-key}
        - {objectName: api-key, key: api-key}
```

- `auth: provider-adc` makes the provider call Secret Manager as itself,
  which on a Compute Engine VM is the node's service account. It is the
  counterpart of Azure's `useVMManagedIdentity`, and has the same trade-off:
  no per-namespace identity. Google's provider documentation recommends
  per-pod Workload Identity instead, which exists only on GKE. That this
  mode works on k3s on Compute Engine is **unverified**. If it does not, create the
  `differently-kv` Secret by hand from `gcloud secrets versions access` and
  keep Secret Manager as the record and the backup.
- **Pin the secrets key's version** (`versions/1` above), as you pin
  `objectVersion` on Key Vault. A new version reaching the server at its next
  restart makes every stored credential unreadable
  ([back up the secrets key](../guides/keys-and-providers.md#back-up-the-secrets-key)).
- As on Azure, the driver syncs `differently-kv` only while a pod mounts the
  volume, and the chart's pre-install migration Job needs it first: run the
  same small `pause` Deployment that mounts it.

Sandboxes must not reach the metadata server, or a container on the control
node could read these secrets through the node's service account. The
NetworkPolicies of step 9 cover that.

## 8. Differently

The values are those of [azure-vms.md, step 6](azure-vms.md#6-differently),
with the Google Cloud addresses:

```yaml
postgresql:
  external: {existingSecret: differently-kv}
auth:
  existingSecret: differently-kv
secrets:
  existingSecret: differently-kv
opensandbox:
  existingSecret: differently-kv
networkPolicy:
  server:
    egress:
      databaseCIDRs: [10.30.0.0/20]          # the private services access range
      extra:                                 # Fast Sandbox execd through the public sandbox host
        - to: [{ipBlock: {cidr: <dly-sandbox address>/32}}]
          ports: [{port: 443, protocol: TCP}]
```

Keep `server.extraVolumes` for the CSI volume, and the microVM pool's
`credentials: {file: /mnt/kv/osb-fsb-api-key}`. Pools on Freestyle and
Daytona need nothing new: their keys are stored in Differently (`secret:`)
and move with the database.

## 9. Ingress and sandbox network policy

Caddy, as on Azure: a host-network Deployment on each node, Let's Encrypt
for both names, `flush_interval -1`. The API host goes to
`dly-differently-server:8080`. On the sandbox host, `/fsb/*` (prefix
stripped) goes to the Fast Sandbox gateway and everything else to the
Kubernetes runtime's gateway.

NetworkPolicies, enforced by k3s. They are Azure's without the wire server:

- namespace `opensandbox` (container sandboxes): egress to kube-dns and to
  `0.0.0.0/0` except `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`,
  `169.254.0.0/16` and `100.64.0.0/10`. `10.0.0.0/8` covers the nodes'
  subnet and the Cloud SQL range, and `169.254.0.0/16` covers the metadata
  server;
- namespace `opensandbox-dataplane` (fastlets): no `169.254.0.0/16`,
  `10.20.0.0/24` or `10.30.0.0/20`;
- template build pods (label `sandbox.fast.io/sandboxtemplate`) also need
  the API server (`10.20.0.10:6443`) and the source registry
  (`10.20.0.11:5000`).

If you turn on Private Google Access's restricted or private VIPs
(`199.36.153.4/30`, `199.36.153.8/30`) with DNS pointing Google APIs at
them, add those ranges to the exclusions as well.

## Verify

None of these has run on Google Cloud. They are the checks that passed on
Azure ([azure-vms.md](azure-vms.md#verify)):

| Check | How |
|---|---|
| KVM on the node | `ls -l /dev/kvm`, `grep -c vmx /proc/cpuinfo` on `dly-kvm` |
| Pools healthy | `curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/pools" \| jq '.items[] \| {name, health: .health.status}'` |
| Container sandbox egress | from a sandbox: the metadata server, Cloud SQL, the Kubernetes API and Redis refused; the internet reachable |
| Container pool stop/start through Artifact Registry | a stop pushes a snapshot to `opensandbox-snapshots`, and a start pulls it back with the disk kept |
| Fast Sandbox create, pause/resume and fork | as on Azure; record the pause time against the provisioned disk throughput |
| Adapter live scenario | `go test -tags live ./internal/adapters/opensandbox/` against the Fast Sandbox server |
| End-to-end suite on the microVM pool | `make e2e` with `DIFFERENTLY_E2E_POOL`, `DIFFERENTLY_E2E_TEMPLATE` and `DIFFERENTLY_E2E_METRICS_URL` set as on Azure |
| Audit chain | `curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/audit/verify" \| jq .` |

Update this page's status, and the support matrix in
[README.md](README.md#support-matrix), with what ran and when.
