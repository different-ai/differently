# EKS cluster for Differently: a system node group for the control plane and
# a tainted KVM node group for the node agents.
#
# UNVALIDATED: written against terraform-aws-modules/eks v21 and
# terraform-aws-modules/vpc v6 input names, but never run. Expect to adjust.

provider "aws" {
  region = var.region

  default_tags {
    tags = merge({ "differently.dev/cluster" = var.cluster_name }, var.tags)
  }
}

data "aws_availability_zones" "available" {
  filter {
    name   = "opt-in-status"
    values = ["opt-in-not-required"]
  }
}

locals {
  azs = slice(data.aws_availability_zones.available.names, 0, 3)

  vpc_id     = var.create_vpc ? module.vpc[0].vpc_id : var.vpc_id
  subnet_ids = var.create_vpc ? module.vpc[0].private_subnets : var.subnet_ids

  kvm_instance_types = length(var.kvm_instance_types) > 0 ? var.kvm_instance_types : (
    var.kvm_mode == "metal" ? ["m7i.metal-24xl"] : ["m8i.4xlarge"]
  )

  # Node label and taint the Helm chart's agent DaemonSet selects and tolerates.
  kvm_label = { "differently.dev/kvm" = "true" }
  kvm_taint = {
    kvm = {
      key    = "differently.dev/kvm"
      value  = "true"
      effect = "NO_SCHEDULE"
    }
  }

  # EC2 nested virtualization is an opt-in CPU option on supported Intel
  # families. UNVERIFIED: whether this module version passes the attribute to
  # the launch template (an unknown attribute may be dropped silently) and
  # whether EKS managed node groups honour it (reported fixed in 2026, see
  # aws/containers-roadmap#2784). Check /dev/kvm on a node after apply.
  # A filtered for expression (not a conditional) so both cases have one type.
  kvm_nested_settings = {
    for k, v in { cpu_options = { nested_virtualization = "enabled" } } : k => v
    if var.kvm_mode == "nested"
  }

  kvm_subnet_ids = length(var.kvm_subnet_ids) > 0 ? var.kvm_subnet_ids : local.subnet_ids

  kvm_node_group = merge({
    ami_type       = "AL2023_x86_64_STANDARD"
    instance_types = local.kvm_instance_types
    capacity_type  = var.kvm_capacity_type
    min_size       = var.kvm_node_count.min
    desired_size   = var.kvm_node_count.desired
    max_size       = var.kvm_node_count.max
    subnet_ids     = local.kvm_subnet_ids

    labels = local.kvm_label
    taints = local.kvm_taint

    # Hop limit 1 keeps pods, and microVMs routed through agent pods, away
    # from the instance metadata service and the node's IAM credentials.
    metadata_options = {
      http_endpoint               = "enabled"
      http_tokens                 = "required"
      http_put_response_hop_limit = 1
    }

    block_device_mappings = {
      xvda = {
        device_name = "/dev/xvda"
        ebs = {
          volume_size           = var.kvm_root_volume_gib
          volume_type           = "gp3"
          encrypted             = true
          delete_on_termination = true
        }
      }
    }
  }, local.kvm_nested_settings)
}

module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 6.0"
  count   = var.create_vpc ? 1 : 0

  name = var.cluster_name
  cidr = var.vpc_cidr
  azs  = local.azs

  private_subnets = [for i, _ in local.azs : cidrsubnet(var.vpc_cidr, 4, i)]
  public_subnets  = [for i, _ in local.azs : cidrsubnet(var.vpc_cidr, 8, i + 48)]

  enable_nat_gateway = true
  single_nat_gateway = true

  public_subnet_tags  = { "kubernetes.io/role/elb" = 1 }
  private_subnet_tags = { "kubernetes.io/role/internal-elb" = 1 }
}

module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 21.0"

  name               = var.cluster_name
  kubernetes_version = var.kubernetes_version

  endpoint_public_access       = var.endpoint_public_access
  endpoint_public_access_cidrs = var.endpoint_public_access_cidrs

  enable_cluster_creator_admin_permissions = true

  addons = {
    coredns                = {}
    eks-pod-identity-agent = { before_compute = true }
    kube-proxy             = {}
    vpc-cni                = { before_compute = true }
  }

  vpc_id     = local.vpc_id
  subnet_ids = local.subnet_ids

  eks_managed_node_groups = {
    system = {
      ami_type       = "AL2023_x86_64_STANDARD"
      instance_types = var.system_instance_types
      min_size       = var.system_node_count.min
      desired_size   = var.system_node_count.desired
      max_size       = var.system_node_count.max

      labels = { "differently.dev/role" = "system" }

      metadata_options = {
        http_endpoint               = "enabled"
        http_tokens                 = "required"
        http_put_response_hop_limit = 2
      }
    }

    kvm = local.kvm_node_group
  }

  tags = var.tags
}
