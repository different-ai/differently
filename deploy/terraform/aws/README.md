# Differently on AWS: EKS with a KVM node group

> **Unvalidated.** Terraform was not available when this module was written.
> It has not been through `terraform validate`, `plan` or `apply`. Input names
> follow `terraform-aws-modules/eks` v21 and `terraform-aws-modules/vpc` v6.
> Run `terraform init && terraform validate` first and expect small fixes.

This root module creates:

- a VPC with three private and three public subnets and one NAT gateway (or uses yours);
- an EKS cluster with the core add-ons;
- a `system` managed node group for the control plane, ingress and monitoring;
- a `kvm` managed node group for the Differently node agents. It is labelled
  `differently.dev/kvm=true` and tainted `differently.dev/kvm=true:NoSchedule`,
  which the Helm chart's DaemonSet selects and tolerates by default.

It does not create PostgreSQL, install the Helm chart or set up DNS and TLS.
Use RDS or Aurora PostgreSQL (or CloudNativePG) and follow
[docs/deploy/aws.md](../../../docs/deploy/aws.md).

## KVM node modes

| `kvm_mode` | Instances | Status |
|---|---|---|
| `metal` (default) | `.metal` sizes such as `m7i.metal-24xl`, `m6i.metal`, `m7a.metal-48xl`, `m8i.metal-48xl` | Firecracker's tested platform (Intel m5n/m6i/m7i/m8i, AMD m6a/m7a, Graviton m6g/m7g/m8g/m9g). 8th-generation Intel needs a 6.1 or 6.18 host kernel. **Unverified** with this module. |
| `nested` | Virtual M7i/M8i, C7i/C8i, R7i/R8i and related Intel families | EC2 launched nested virtualization as an opt-in CPU option in 2026. Whether EKS managed node groups honour it from a launch template was reported fixed in July 2026 (aws/containers-roadmap#2784, closed 2026-10-12), but the EKS docs do not mention it yet. Whether the EKS module forwards `cpu_options.nested_virtualization` is **unverified**: an unknown attribute may be dropped silently. Karpenter `EC2NodeClass` supports it from v1.13.0. Firecracker documents nested virtualization as a development setup only. |

The default instance types are a starting point. Check `.metal` capacity in
your Region and Availability Zones (use `kvm_subnet_ids` to pin AZs).
`.metal` instances take noticeably longer to boot than virtual ones.

Host kernel: Firecracker supports 5.10, 6.1 and 6.18 host kernels. Check which
kernel your EKS AL2023 AMI release ships (`uname -r` on a node). This is
**unverified** for current AMIs.

## Usage

```sh
cp terraform.tfvars.example terraform.tfvars   # edit region, sizes, CIDRs
terraform init
terraform validate
terraform plan -out tfplan
terraform apply tfplan

$(terraform output -raw configure_kubectl)
kubectl get nodes -L differently.dev/kvm
```

Then install the chart (see [docs/deploy/kubernetes.md](../../../docs/deploy/kubernetes.md)).

## Security notes

- KVM nodes use IMDSv2 with a hop limit of 1, so pods (and microVMs routed
  through agent pods) cannot reach the instance metadata service and the
  node role's credentials. Keep it that way, and also block
  `169.254.169.254` in sandbox egress rules.
- Narrow `endpoint_public_access_cidrs`, or set `endpoint_public_access = false`
  and reach the API through a VPN or bastion.
- Spot capacity (`kvm_capacity_type = "SPOT"`) interrupts every microVM on the
  node without warning to the guest.

## Verify

```sh
# 1. The KVM nodes exist with the label and taint.
kubectl get nodes -l differently.dev/kvm=true \
  -o custom-columns=NAME:.metadata.name,TYPE:.metadata.labels.node\\.kubernetes\\.io/instance-type,TAINTS:.spec.taints

# 2. /dev/kvm exists on a KVM node (debug pod tolerating the taint).
NODE=$(kubectl get nodes -l differently.dev/kvm=true -o jsonpath='{.items[0].metadata.name}')
kubectl debug node/$NODE -it --image=busybox:1.37 --profile=sysadmin -- \
  sh -c 'ls -l /host/dev/kvm && uname -r'
```

If `/dev/kvm` is missing in `nested` mode, the CPU option did not reach the
instance: check the launch template in the EC2 console, or switch to `metal`.
After installing the chart, `kubectl logs <agent-pod> -c doctor` gives the
full KVM, tun, kernel and image report.
