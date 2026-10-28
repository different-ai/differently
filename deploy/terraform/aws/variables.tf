variable "region" {
  description = "AWS region."
  type        = string
}

variable "cluster_name" {
  description = "EKS cluster name; also names the VPC."
  type        = string
  default     = "differently"
}

variable "kubernetes_version" {
  description = "EKS Kubernetes version. Pick one in EKS standard support; the chart needs 1.29 or newer."
  type        = string
  default     = "1.35"
}

variable "tags" {
  description = "Tags applied to every resource."
  type        = map(string)
  default     = {}
}

# --- Networking -------------------------------------------------------------

variable "create_vpc" {
  description = "Create a VPC. Set false and pass vpc_id and subnet_ids to use an existing one."
  type        = bool
  default     = true
}

variable "vpc_cidr" {
  description = "CIDR of the created VPC."
  type        = string
  default     = "10.42.0.0/16"
}

variable "vpc_id" {
  description = "Existing VPC ID (create_vpc = false)."
  type        = string
  default     = null
}

variable "subnet_ids" {
  description = "Existing private subnet IDs for the cluster and node groups (create_vpc = false)."
  type        = list(string)
  default     = []
}

variable "kvm_subnet_ids" {
  description = "Subnets for the KVM node group only. .metal capacity differs per Availability Zone, so you may need to pin these. Defaults to the cluster subnets."
  type        = list(string)
  default     = []
}

variable "endpoint_public_access" {
  description = "Expose the EKS API endpoint publicly."
  type        = bool
  default     = true
}

variable "endpoint_public_access_cidrs" {
  description = "CIDRs allowed to reach a public EKS API endpoint. Narrow this."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

# --- System node group ------------------------------------------------------

variable "system_instance_types" {
  description = "Instance types for the system node group (control plane, ingress, monitoring)."
  type        = list(string)
  default     = ["m7i.large"]
}

variable "system_node_count" {
  description = "Min/desired/max size of the system node group."
  type        = object({ min = number, desired = number, max = number })
  default     = { min = 2, desired = 2, max = 4 }
}

# --- KVM node group ---------------------------------------------------------

variable "kvm_mode" {
  description = <<-EOT
    How KVM nodes get /dev/kvm:
      metal  - bare-metal instances (Firecracker's tested platform; recommended).
      nested - virtual Intel 7th/8th-generation instances with the EC2
               nested-virtualization CPU option. UNVERIFIED with EKS managed
               node groups; confirm /dev/kvm on a node before relying on it.
  EOT
  type        = string
  default     = "metal"

  validation {
    condition     = contains(["metal", "nested"], var.kvm_mode)
    error_message = "kvm_mode must be \"metal\" or \"nested\"."
  }
}

variable "kvm_instance_types" {
  description = "Instance types for the KVM node group. Empty picks a default for kvm_mode (m7i.metal-24xl for metal, m8i.4xlarge for nested)."
  type        = list(string)
  default     = []
}

variable "kvm_node_count" {
  description = "Min/desired/max size of the KVM node group. The EKS module ignores later changes to desired; scale with min/max or the console."
  type        = object({ min = number, desired = number, max = number })
  default     = { min = 1, desired = 1, max = 3 }
}

variable "kvm_root_volume_gib" {
  description = "Root EBS volume size for KVM nodes; holds guest images and per-sandbox disks under /var/lib/differently."
  type        = number
  default     = 500
}

variable "kvm_capacity_type" {
  description = "ON_DEMAND or SPOT. Spot interruptions terminate every microVM on the node."
  type        = string
  default     = "ON_DEMAND"
}
