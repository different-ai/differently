# Azure (AKS)

> **Quickest path:** a single Azure VM with k3s, following
> [One Linux VM on any cloud](single-vm.md). Use this page when you run a
> managed Kubernetes cluster.

Status: **unverified, experimental**. No step here has been run.

Differently's control plane runs on any AKS node pool. Sandboxes run on an
OpenSandbox server you install in the cluster
([kubernetes.md, step 2](kubernetes.md#2-install-opensandbox-and-choose-its-runtime)).
The Docker and Kubernetes runtimes need nothing special from AKS. The KVM
notes below apply only if OpenSandbox uses Fast Sandbox (Firecracker
microVMs), and the KVM path on Azure is not an officially supported
configuration.

## Where KVM is available on Azure (Fast Sandbox only)

| Path | Details |
|---|---|
| **Nested-virtualization-capable VM sizes** | Sizes whose feature table says "Nested Virtualization: Supported", for example **Dsv6** (Intel Emerald Rapids). **Dpsv6** (Arm, Cobalt 100) is not supported. Microsoft documents nested virtualization for Hyper-V. Running KVM inside a Linux VM works but is described as **not officially supported** (Microsoft Q&A answer; **unverified** as policy). There is no AKS documentation on exposing `/dev/kvm` on Ubuntu or Azure Linux nodes (**unverified**) |
| AKS Pod Sandboxing | Kata Containers with Cloud Hypervisor on the Microsoft Hypervisor (MSHV), Azure Linux only. **Not a KVM path**: Firecracker is KVM-only, so Fast Sandbox cannot use it. OpenSandbox's Kubernetes runtime can use it instead: its AKS example sets `[secure_runtime] type = "kata"` with RuntimeClass `kata-vm-isolation`, so each sandbox gets its own VM. Not yet run with Differently |

If you need microVM isolation with a supported KVM path, a safer layout is to
run the control plane on AKS and the OpenSandbox server with Fast Sandbox on
KVM hosts outside AKS (for example on-premises bare metal), registered as a
pool by URL. See
[bare-metal.md](bare-metal.md#register-an-opensandbox-server-on-a-plain-linux-host).

## 1. Add a KVM node pool (Fast Sandbox only)

```sh
az aks nodepool add \
  --resource-group <rg> --cluster-name <cluster> \
  --name kvm \
  --node-vm-size Standard_D16s_v6 \
  --os-sku Ubuntu \
  --node-count 1 \
  --labels sandbox-runtime=fast-sandbox \
  --node-taints sandbox-runtime=fast-sandbox:NoSchedule \
  --node-osdisk-size 512
```

- The label and taint are examples. Differently does not read them; make
  OpenSandbox's installation select and tolerate whatever you choose.
- Check that the size's page lists nested virtualization as supported in your
  region.
- Whether the AKS Ubuntu node image loads `kvm_intel` and exposes `/dev/kvm`
  is **unverified**. Check on a node before going further, together with the
  rest of Fast Sandbox's host requirements
  ([prerequisites.md](prerequisites.md#kvm-hosts-fast-sandbox-only)).
- Security type (Trusted Launch versus Standard) may affect nested
  virtualization on some sizes (**unverified**).

## 2. Database

Use Azure Database for PostgreSQL Flexible Server with private access (VNet
integration) and TLS:

```sh
kubectl create namespace differently
kubectl -n differently create secret generic differently-database \
  --from-literal=database-url='postgres://differently:PASSWORD@<server>.postgres.database.azure.com:5432/differently?sslmode=require'
```

## 3. Install OpenSandbox

Follow OpenSandbox's documentation and
[kubernetes.md, step 2](kubernetes.md#2-install-opensandbox-and-choose-its-runtime),
then store its API key for Differently:

```sh
kubectl -n differently create secret generic opensandbox-api-key --from-literal=api-key='<api_key>'
```

## 4. Install Differently

```yaml
# values-aks.yaml
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
      databaseCIDRs: ["<delegated-subnet-cidr>"]
      # runtimeProviders: true            # Daytona or Freestyle added from the dashboard or API
ingress:
  enabled: true
  className: <your-ingress-class>       # e.g. the application routing add-on or AGIC
  hosts: [differently.example.com]
  tls:
    enabled: true
    certManager: {clusterIssuer: letsencrypt-prod}
```

```sh
helm install dly deploy/helm/differently -n differently -f values-aks.yaml
```

The chart's NetworkPolicy needs enforcement turned on: Azure Network Policy
Manager, Calico, or Azure CNI powered by Cilium. Then continue with steps 6
to 8 of [kubernetes.md](kubernetes.md).

The chart also generates the secrets key, which seals provider credentials
stored in Differently, in `dly-differently-secrets-key`. Back it up, for
example to Key Vault as below, or provide your own from Key Vault with
`secrets.existingSecret`.

## Provider credentials with Azure Key Vault

Unverified: none of this has been run on AKS. The Key Vault parts follow
Azure's documentation for the Secrets Store CSI Driver (the AKS add-on
`azure-keyvault-secrets-provider`).

Differently can take credentials from Key Vault in three ways. You can mix
them across pools.

| Way | What reaches the server | When a new Key Vault version takes effect |
|---|---|---|
| A. CSI driver syncs a Kubernetes Secret, which the chart reads | Environment variables (`opensandbox.existingSecret`, `providers.*.existingSecret`, `secrets.existingSecret`) | After the synced Secret updates **and** the server restarts |
| B. CSI driver mounts files, which pools reference as `file:` | Files under the mount path | Within about 5 seconds of the file changing, no restart |
| C. Keep only the secrets key in Key Vault, and store provider keys in Differently (`secret:`) | The secrets key (by A); provider keys are in Differently's database, sealed | Provider keys: within seconds of `differently admin secret put` |

The CSI driver runs on the node and fetches from Key Vault itself, so the
server needs no egress to Key Vault. Give the driver an identity with read
access to the vault's secrets; Azure recommends workload identity. The
chart's `serviceAccount.annotations` can carry the workload identity
annotation.

A `SecretProviderClass` for all three:

```yaml
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: differently-kv
  namespace: differently
spec:
  provider: azure
  parameters:
    clientID: <workload-identity-client-id>
    keyvaultName: <vault>
    tenantId: <tenant-id>
    objects: |
      array:
        - |
          objectName: differently-secrets-key
          objectType: secret
          objectVersion: <pinned version>   # never let this one change
        - |
          objectName: opensandbox-api-key
          objectType: secret
        - |
          objectName: daytona-api-key
          objectType: secret
  secretObjects:                            # way A: a Kubernetes Secret the chart reads
    - secretName: differently-kv
      type: Opaque
      data:
        - {objectName: differently-secrets-key, key: secrets-key}
        - {objectName: opensandbox-api-key, key: api-key}
```

The driver creates and updates the synced Secret only while a pod mounts
the volume, so mount it into the server in every case:

```yaml
# values-aks.yaml additions; opensandbox.existingSecret replaces opensandbox-api-key from step 4
server:
  extraVolumes:
    - name: kv
      csi:
        driver: secrets-store.csi.k8s.io
        readOnly: true
        volumeAttributes: {secretProviderClass: differently-kv}
  extraVolumeMounts:
    - {name: kv, mountPath: /mnt/kv, readOnly: true}
secrets:
  existingSecret: differently-kv              # way A for the secrets key
  existingSecretKey: secrets-key
opensandbox:
  existingSecret: differently-kv              # way A for the OpenSandbox key
  existingSecretKey: api-key
```

**Way B, live rotation from Key Vault.** Reference the mounted file from a
pool: `credentials: {file: /mnt/kv/daytona-api-key}` in `config.pools`, or
`"credentials":{"file":"/mnt/kv/daytona-api-key"}` with
`POST /v1/admin/pools`. The driver updates mounted files only with secret
rotation turned on:

```sh
az aks addon update --resource-group <rg> --name <cluster> \
  --addon azure-keyvault-secrets-provider --enable-secret-rotation --rotation-poll-interval 2m
```

A new Key Vault version then reaches the file within the poll interval, and
Differently picks it up within about 5 seconds after that. No dedicated test
covers `file:` pickup; it uses the same code path as stored secrets.

**The secrets key.** Keep it in Key Vault as the backup even if the chart
generated it
([back up the secrets key](../guides/keys-and-providers.md#back-up-the-secrets-key)).
If the server reads it from Key Vault, pin `objectVersion` as above and never
add a version to that secret. The server reads the key once, at start-up.
A changed key makes every stored credential unreadable after the next
restart.

**Egress for providers added at runtime.** A Daytona or Freestyle pool added
from the dashboard or API needs
`networkPolicy.server.egress.runtimeProviders: true`. That lets the server
reach `providerCIDRs` on 443. Providers enabled with the chart's
`providers.*` values already have this rule.

## Security notes for Azure

- Block the Instance Metadata Service (`169.254.169.254`) from sandboxes, in
  OpenSandbox's network configuration or with NetworkPolicy in its namespace.
- Keep any KVM pool tainted and its managed identity minimal.
- Scope the CSI driver's identity to the one vault, with read access to
  secrets only. Anyone who can read the secrets key and the database can
  read every stored provider credential.

## Verify

```sh
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/pools" \
  | jq '.items[] | {name, trust, health: .health.status}'
helm test dly -n differently --logs
# Fast Sandbox only:
NODE=$(kubectl get nodes -l sandbox-runtime=fast-sandbox -o jsonpath='{.items[0].metadata.name}')
kubectl debug node/$NODE -it --image=busybox:1.37 --profile=sysadmin -- \
  sh -c 'ls -l /host/dev/kvm; grep -c vmx /host/proc/cpuinfo; uname -r'
```

If `/dev/kvm` is missing, the size or node image does not expose nested
virtualization. Use the container runtimes, or the hybrid layout above.
