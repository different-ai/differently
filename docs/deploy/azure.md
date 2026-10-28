# Azure (AKS)

Status: **unverified, experimental**. No step here has been run, and the KVM
path on Azure is not an officially supported configuration.

## Where KVM is available on Azure

| Path | Details |
|---|---|
| **Nested-virtualization-capable VM sizes** | Sizes whose feature table says "Nested Virtualization: Supported", for example **Dsv6** (Intel Emerald Rapids). **Dpsv6** (Arm, Cobalt 100) is not supported. Microsoft documents nested virtualization for Hyper-V. Running KVM inside a Linux VM works but is described as **not officially supported** (Microsoft Q&A answer; **unverified** as policy). There is no AKS documentation on exposing `/dev/kvm` on Ubuntu or Azure Linux nodes (**unverified**) |
| AKS Pod Sandboxing | Kata Containers with Cloud Hypervisor on the Microsoft Hypervisor (MSHV), Azure Linux only. **Not a KVM path**: Firecracker is KVM-only, so the Differently agent cannot use it |

If you need a supported KVM path on Azure, a safer layout is to run the
control plane on AKS and the KVM hosts as plain Linux machines outside AKS
(for example on-premises bare metal) with the systemd agent. See
[bare-metal.md](bare-metal.md#register-plain-linux-hosts-systemd).

## 1. Add a KVM node pool

```sh
az aks nodepool add \
  --resource-group <rg> --cluster-name <cluster> \
  --name kvm \
  --node-vm-size Standard_D16s_v6 \
  --os-sku Ubuntu \
  --node-count 1 \
  --labels differently.dev/kvm=true \
  --node-taints differently.dev/kvm=true:NoSchedule \
  --node-osdisk-size 512
```

- Check that the size's page lists nested virtualization as supported in your
  region.
- Whether the AKS Ubuntu node image loads `kvm_intel` and exposes `/dev/kvm`
  is **unverified**. Check on a node before going further.
- Security type (Trusted Launch versus Standard) may affect nested
  virtualization on some sizes (**unverified**).

## 2. Database

Use Azure Database for PostgreSQL Flexible Server with private access (VNet
integration) and TLS:

```sh
kubectl create namespace differently
kubectl label namespace differently pod-security.kubernetes.io/enforce=privileged
kubectl -n differently create secret generic differently-database \
  --from-literal=database-url='postgres://differently:PASSWORD@<server>.postgres.database.azure.com:5432/differently?sslmode=require'
```

## 3. Install

```yaml
# values-aks.yaml
config:
  store: {driver: postgres}
postgresql:
  external: {existingSecret: differently-database}
server:
  replicas: 3
networkPolicy:
  server:
    egress:
      databaseCIDRs: ["<delegated-subnet-cidr>"]
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
Manager, Calico, or Azure CNI powered by Cilium. Then continue with steps 7
to 9 of [kubernetes.md](kubernetes.md).

## Security notes for Azure

- Block the Instance Metadata Service (`169.254.169.254`) from sandboxes.
- Keep the KVM pool tainted and its managed identity minimal.

## Verify

```sh
NODE=$(kubectl get nodes -l differently.dev/kvm=true -o jsonpath='{.items[0].metadata.name}')
kubectl debug node/$NODE -it --image=busybox:1.37 --profile=sysadmin -- \
  sh -c 'ls -l /host/dev/kvm; grep -c vmx /host/proc/cpuinfo; uname -r'
kubectl -n differently logs ds/dly-differently-agent -c doctor
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/agents" | jq '.items[] | {node, healthy}'
helm test dly -n differently --logs
```

If `/dev/kvm` is missing, the size or node image does not expose nested
virtualization. Use the hybrid layout above instead.
