# AWS (EKS)

Status: **documented**. The Terraform module is **unvalidated**: it has not
been through `terraform validate`, `plan` or `apply`. Nested virtualization
on EKS is **unverified**.

## Where KVM is available on AWS

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

## 1. Create the cluster

With the module in [`deploy/terraform/aws`](../../deploy/terraform/aws/README.md):

```sh
cd deploy/terraform/aws
cp terraform.tfvars.example terraform.tfvars    # region, kvm_mode, sizes, CIDRs
terraform init && terraform validate
terraform apply
$(terraform output -raw configure_kubectl)
```

The module creates:

- a VPC;
- EKS with the core add-ons;
- a `system` node group;
- a `kvm` node group labelled `differently.dev/kvm=true` and tainted
  `differently.dev/kvm=true:NoSchedule`, using IMDSv2 with hop limit 1.

`kvm_mode = "metal"` (the default) uses `m7i.metal-24xl`. `kvm_mode = "nested"`
uses `m8i.4xlarge` and sets the CPU option (**unverified** through the EKS
module).

For an existing cluster, add a node group with the same label and taint. Use
a `.metal` type, or a nested-capable type with the CPU option in a launch
template.

## 2. Database

Use Amazon RDS for PostgreSQL or Aurora PostgreSQL in the same VPC:

- Allow TCP 5432 from the EKS node security group
  (`terraform output node_security_group_id`).
- Require TLS (`sslmode=verify-full` with the RDS CA bundle, or
  `sslmode=require` at minimum).
- Enable automated backups and point-in-time recovery (see [day-2.md](day-2.md)).

```sh
kubectl create namespace differently
kubectl label namespace differently pod-security.kubernetes.io/enforce=privileged
kubectl -n differently create secret generic differently-database \
  --from-literal=database-url='postgres://differently:PASSWORD@<rds-endpoint>:5432/differently?sslmode=require'
```

## 3. Guest images

Build the kernel and root filesystem ([`deploy/images/`](../../deploy/images/README.md)).
Then do one of these:

- push a guest-images OCI image to ECR and enable `agent.guestImages`;
- bake the files into a custom AMI or user data under
  `/var/lib/differently/images/`.

## 4. Install

```yaml
# values-aws.yaml
config:
  store: {driver: postgres}
postgresql:
  external:
    existingSecret: differently-database
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
agent:
  jailer: {enabled: true}
```

```sh
helm install dly deploy/helm/differently -n differently -f values-aws.yaml
```

With an ALB and an ACM certificate, TLS ends at the load balancer, so leave
`ingress.tls.enabled=false` and set `config.publicURL: https://differently.example.com`
explicitly. Then continue with steps 7 to 9 of [kubernetes.md](kubernetes.md).

NetworkPolicy enforcement: the VPC CNI enforces NetworkPolicy only when its
network policy agent is enabled (`enableNetworkPolicy` on the `vpc-cni`
add-on). Without it, the chart's policies have no effect.

## Security notes for AWS

- **IMDS.** Keep IMDSv2 with hop limit 1 on KVM nodes. Also drop
  `169.254.169.254` in sandbox egress, so no sandbox can read the node role's
  credentials.
- **IAM.** The agent needs no IAM permissions. Keep the KVM node role at the
  EKS minimum.
- **Encryption.** EBS root volumes are encrypted by the module. Guest disks
  live on them under `/var/lib/differently`.
- **Spot.** A Spot interruption ends every microVM on the node. Use On-Demand
  for long-lived sandboxes.

## Verify

```sh
kubectl get nodes -l differently.dev/kvm=true -L node.kubernetes.io/instance-type
NODE=$(kubectl get nodes -l differently.dev/kvm=true -o jsonpath='{.items[0].metadata.name}')
kubectl debug node/$NODE -it --image=busybox:1.37 --profile=sysadmin -- ls -l /host/dev/kvm
kubectl -n differently logs ds/dly-differently-agent -c doctor
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/agents" | jq '.items[] | {node, healthy}'
helm test dly -n differently --logs
```

In nested mode, if `/dev/kvm` is missing, the CPU option did not reach the
instance. Check the launch template in the EC2 console, or switch to `metal`.
