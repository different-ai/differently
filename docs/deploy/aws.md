# AWS (EKS)

> **Quickest path:** a single EC2 VM with k3s, following
> [One Linux VM on any cloud](single-vm.md). Use this page when you run a
> managed Kubernetes cluster.

Status: **documented**. The Terraform module is **unvalidated**: it has not
been through `terraform validate`, `plan` or `apply`. Nested virtualization
on EKS is **unverified**, and so is OpenSandbox's Fast Sandbox runtime.

Differently's control plane runs on any EKS node. Sandboxes run on an
OpenSandbox server you install in the cluster
([kubernetes.md, step 2](kubernetes.md#2-install-opensandbox-and-choose-its-runtime)).
KVM matters only if that server uses Fast Sandbox (Firecracker microVMs); the
Docker and Kubernetes runtimes run on ordinary nodes.

## Where KVM is available on AWS (Fast Sandbox only)

| Path | Instances | What to know |
|---|---|---|
| **Bare metal (recommended)** | `.metal` sizes of m5n, m6i, m7i, m8i, m6a, m7a, m6g, m7g, m8g, m9g | Firecracker's only tested platforms, and the production path. You get the whole host; boot is slower than virtual instances. 8th-gen Intel needs a 6.1 or 6.18 host kernel |
| **Nested virtualization** | Virtual M7i, M7i-flex, M8i, M8id, M8i-flex, C7i, C7i-flex, C8i, C8id, C8i-flex, R7i, R7iz, R8i, R8id, R8i-flex, X8i, I7i, I7ie | Opt-in CPU option `NestedVirtualization=enabled`, launched in 2026 at no extra cost. AWS steers latency-sensitive work to bare metal. No Graviton (**unverified**, secondary source). Firecracker treats nested virtualization as a development setup |
| EKS Fargate | none | No `/dev/kvm` (**unverified**) |

EKS managed node groups:

- Early 2026 reports said the launch-template nested-virtualization option
  was dropped.
- A July 2026 comment says it is now respected, and the tracking issue
  (aws/containers-roadmap#2784) was closed on 2026-10-12.
- The EKS launch-template docs do not mention it yet. Test it.

Karpenter supports nested virtualization in `EC2NodeClass` from v1.13.0.

Fast Sandbox's other host requirements (cgroup v2, an XFS image volume with
reflink, an enlarged `/dev/shm`) are listed in
[prerequisites.md](prerequisites.md#kvm-hosts-fast-sandbox-only). Whether the
EKS AL2023 root volume meets the XFS-with-reflink requirement is
**unverified**; check it on a node, or add a dedicated volume.

## 1. Create the cluster

With the module in [`deploy/terraform/aws`](../../deploy/terraform/aws/README.md):

```sh
cd deploy/terraform/aws
cp terraform.tfvars.example terraform.tfvars    # region, sizes, CIDRs; kvm_* only for Fast Sandbox
terraform init && terraform validate
terraform apply
$(terraform output -raw configure_kubectl)
```

The module creates:

- a VPC;
- EKS with the core add-ons;
- a `system` node group, which runs Differently's control plane and, with the
  Docker or Kubernetes runtime, can run OpenSandbox too;
- only with `kvm_node_group_enabled = true`: a `kvm` node group labelled
  `differently.dev/kvm=true` and tainted `differently.dev/kvm=true:NoSchedule`,
  using IMDSv2 with hop limit 1, for OpenSandbox's Fast Sandbox components.
  Differently itself does not read the label; OpenSandbox's installation must
  select and tolerate it.

Set `kvm_node_group_enabled = true` only if OpenSandbox will run Fast
Sandbox; the Docker and Kubernetes runtimes do not need KVM nodes, and the
default (`false`) creates none. With the group enabled, `kvm_mode = "metal"`
(the default) uses `m7i.metal-24xl` and `kvm_mode = "nested"` uses
`m8i.4xlarge` and sets the CPU option (**unverified** through the EKS
module).

For an existing cluster that will run Fast Sandbox, add a node group with a
`.metal` type, or a nested-capable type with the CPU option in a launch
template, and the label and taint your OpenSandbox installation expects.

## 2. Database

Use Amazon RDS for PostgreSQL or Aurora PostgreSQL in the same VPC:

- Allow TCP 5432 from the EKS node security group
  (`terraform output node_security_group_id`).
- Require TLS (`sslmode=verify-full` with the RDS CA bundle, or
  `sslmode=require` at minimum).
- Enable automated backups and point-in-time recovery (see [day-2.md](day-2.md)).

```sh
kubectl create namespace differently
kubectl -n differently create secret generic differently-database \
  --from-literal=database-url='postgres://differently:PASSWORD@<rds-endpoint>:5432/differently?sslmode=require'
```

## 3. Install OpenSandbox

Install OpenSandbox with its own chart or manifests, following its
documentation and [kubernetes.md, step 2](kubernetes.md#2-install-opensandbox-and-choose-its-runtime):
choose the runtime, set the server API key, pin versions. For Fast Sandbox,
schedule its node components on the `kvm` group. Then store the API key for
Differently:

```sh
kubectl -n differently create secret generic opensandbox-api-key --from-literal=api-key='<api_key>'
```

## 4. Install Differently

```yaml
# values-aws.yaml
config:
  store: {driver: postgres}
postgresql:
  external:
    existingSecret: differently-database
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
      databaseCIDRs: ["10.42.0.0/16"]   # VPC CIDR or the RDS subnets
ingress:
  enabled: true
  className: alb                       # AWS Load Balancer Controller, installed separately
  annotations:
    alb.ingress.kubernetes.io/scheme: internet-facing
    alb.ingress.kubernetes.io/target-type: ip
    alb.ingress.kubernetes.io/certificate-arn: arn:aws:acm:...:certificate/...
    alb.ingress.kubernetes.io/listen-ports: '[{"HTTPS":443}]'
  hosts: [differently.example.com]
```

```sh
helm install dly deploy/helm/differently -n differently -f values-aws.yaml
```

With an ALB and an ACM certificate, TLS ends at the load balancer, so leave
`ingress.tls.enabled=false` and set `config.publicURL: https://differently.example.com`
explicitly. Then continue with steps 6 to 8 of [kubernetes.md](kubernetes.md).

NetworkPolicy enforcement: the VPC CNI enforces NetworkPolicy only when its
network policy agent is enabled (`enableNetworkPolicy` on the `vpc-cni`
add-on). Without it, the chart's policies have no effect.

## Security notes for AWS

- **IMDS.** Keep IMDSv2 with hop limit 1 on the nodes that run sandboxes, and
  block `169.254.169.254` in sandbox egress (in OpenSandbox's network
  configuration or with NetworkPolicy in its namespace), so no sandbox can
  read a node role's credentials. Differently does not do this for you. The
  Terraform module's `system` group uses hop limit 2, so do not run
  container-runtime sandboxes there without that block, or give them their
  own node group with hop limit 1.
- **IAM.** Differently's control plane needs no IAM permissions. Keep node
  roles at the EKS minimum; check what OpenSandbox's components need in its
  documentation.
- **Encryption.** EBS root volumes are encrypted by the module. Sandbox disks
  live wherever OpenSandbox's runtime puts them; check that those volumes are
  encrypted too.
- **Spot.** A Spot interruption ends every sandbox on the node. Use On-Demand
  for long-lived sandboxes.

## Verify

```sh
kubectl -n differently logs deploy/dly-differently-server -c preflight
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/pools" \
  | jq '.items[] | {name, trust, health: .health.status}'
helm test dly -n differently --logs
# Fast Sandbox only:
kubectl get nodes -l differently.dev/kvm=true -L node.kubernetes.io/instance-type
NODE=$(kubectl get nodes -l differently.dev/kvm=true -o jsonpath='{.items[0].metadata.name}')
kubectl debug node/$NODE -it --image=busybox:1.37 --profile=sysadmin -- ls -l /host/dev/kvm
```

In nested mode, if `/dev/kvm` is missing, the CPU option did not reach the
instance. Check the launch template in the EC2 console, or switch to `metal`.
