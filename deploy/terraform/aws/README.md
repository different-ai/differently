# Differently on AWS: EKS with a KVM node group

> **Unvalidated.** Terraform was not available when this module was written.
> It has not been through `terraform validate`, `plan` or `apply`. Input names
> follow `terraform-aws-modules/eks` v21 and `terraform-aws-modules/vpc` v6.
> Run `terraform init && terraform validate` first and expect small fixes.

This root module creates:

- a VPC with three private and three public subnets and one NAT gateway (or uses yours);
- an EKS cluster with the core add-ons;
- a `system` managed node group for Differently's control plane, ingress and
  monitoring (and OpenSandbox, if it uses the Docker or Kubernetes runtime);
- optionally (`kvm_node_group_enabled = true`), a `kvm` managed node group
  for OpenSandbox's Fast Sandbox runtime (Firecracker microVMs). It is
  labelled `differently.dev/kvm=true` and tainted
  `differently.dev/kvm=true:NoSchedule`.

Differently itself does not read that label or taint. Its chart deploys the
control plane only, and sandboxes run on an OpenSandbox server you install
separately ([ADR 0020](../../../docs/adr/0020-opensandbox-replaces-node-agent.md)).
Configure OpenSandbox's Fast Sandbox components to select and tolerate the
label and taint (the `kvm_node_selector` and `kvm_node_taint` outputs).

The group is off by default. OpenSandbox's Docker and Kubernetes runtimes do
not need KVM. For untrusted code, though, run their sandboxes on nodes
separate from the control plane: add your own node group for them.

It does not create PostgreSQL, install OpenSandbox or the Helm chart, or set
up DNS and TLS. Use RDS or Aurora PostgreSQL (or CloudNativePG) and follow
[docs/deploy/aws.md](../../../docs/deploy/aws.md).

## KVM node modes

| `kvm_mode` | Instances | Status |
|---|---|---|
| `metal` (default) | `.metal` sizes such as `m7i.metal-24xl`, `m6i.metal`, `m7a.metal-48xl`, `m8i.metal-48xl` | Firecracker's tested platform (Intel m5n/m6i/m7i/m8i, AMD m6a/m7a, Graviton m6g/m7g/m8g/m9g). 8th-generation Intel needs a 6.1 or 6.18 host kernel. **Unverified** with this module and with Fast Sandbox. |
| `nested` | Virtual M7i/M8i, C7i/C8i, R7i/R8i and related Intel families | EC2 launched nested virtualization as an opt-in CPU option in 2026. Whether EKS managed node groups honour it from a launch template was reported fixed in July 2026 (aws/containers-roadmap#2784, closed 2026-10-12), but the EKS docs do not mention it yet. Whether the EKS module forwards `cpu_options.nested_virtualization` is **unverified**: an unknown attribute may be dropped silently. Karpenter `EC2NodeClass` supports it from v1.13.0. Firecracker documents nested virtualization as a development setup only. |

The default instance types are a starting point. Check `.metal` capacity in
your Region and Availability Zones (use `kvm_subnet_ids` to pin AZs).
`.metal` instances take noticeably longer to boot than virtual ones.

Host requirements beyond `/dev/kvm`: Fast Sandbox also needs `/dev/net/tun`,
cgroup v2, an XFS filesystem with reflink for images and an enlarged
`/dev/shm` (see
[prerequisites.md](../../../docs/deploy/prerequisites.md#kvm-hosts-fast-sandbox-only)).
Which kernel your EKS AL2023 AMI release ships, and whether its volumes meet
the XFS requirement, is **unverified** for current AMIs; check on a node.

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

Then install OpenSandbox and Differently's chart (see
[docs/deploy/aws.md](../../../docs/deploy/aws.md) and
[docs/deploy/kubernetes.md](../../../docs/deploy/kubernetes.md)).

## Security notes

- KVM nodes use IMDSv2 with a hop limit of 1, so pods on them cannot reach
  the instance metadata service and the node role's credentials. Keep it
  that way, and also block `169.254.169.254` in sandbox egress (in
  OpenSandbox's network configuration or with NetworkPolicy); Differently
  does not.
- The `system` group uses hop limit 2, so pods there can reach the metadata
  service. If OpenSandbox's Docker or Kubernetes runtime puts sandboxes on
  those nodes, block `169.254.169.254` for them, or give sandboxes their own
  node group with hop limit 1.
- Narrow `endpoint_public_access_cidrs`, or set `endpoint_public_access = false`
  and reach the API through a VPN or bastion.
- Spot capacity (`kvm_capacity_type = "SPOT"`) interrupts every sandbox on the
  node without warning to the guest.

## Verify

```sh
# 1. The KVM nodes exist with the label and taint.
kubectl get nodes -l differently.dev/kvm=true \
  -o custom-columns=NAME:.metadata.name,TYPE:.metadata.labels.node\\.kubernetes\\.io/instance-type,TAINTS:.spec.taints

# 2. /dev/kvm exists on a KVM node (debug pod tolerating the taint).
NODE=$(kubectl get nodes -l differently.dev/kvm=true -o jsonpath='{.items[0].metadata.name}')
kubectl debug node/$NODE -it --image=busybox:1.37 --profile=sysadmin -- \
  sh -c 'ls -l /host/dev/kvm /host/dev/net/tun && stat -fc %T /host/sys/fs/cgroup && uname -r'
```

If `/dev/kvm` is missing in `nested` mode, the CPU option did not reach the
instance: check the launch template in the EC2 console, or switch to `metal`.
