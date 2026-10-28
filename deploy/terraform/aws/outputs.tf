output "region" {
  description = "AWS region."
  value       = var.region
}

output "cluster_name" {
  description = "EKS cluster name."
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  description = "EKS API endpoint."
  value       = module.eks.cluster_endpoint
}

output "cluster_certificate_authority_data" {
  description = "Base64 cluster CA certificate."
  value       = module.eks.cluster_certificate_authority_data
}

output "oidc_provider_arn" {
  description = "IRSA OIDC provider ARN."
  value       = module.eks.oidc_provider_arn
}

output "node_security_group_id" {
  description = "Shared node security group (allow your PostgreSQL to accept 5432 from it)."
  value       = module.eks.node_security_group_id
}

output "vpc_id" {
  description = "VPC ID."
  value       = local.vpc_id
}

output "kvm_node_selector" {
  description = "Label on KVM nodes (when kvm_node_group_enabled); select it for OpenSandbox's Fast Sandbox components."
  value       = local.kvm_label
}

output "kvm_node_taint" {
  description = "Taint on KVM nodes (when kvm_node_group_enabled); tolerate it for OpenSandbox's Fast Sandbox components."
  value       = "differently.dev/kvm=true:NoSchedule"
}

output "kvm_mode" {
  description = "metal or nested."
  value       = var.kvm_mode
}

output "configure_kubectl" {
  description = "Command that writes a kubeconfig entry for the cluster."
  value       = "aws eks update-kubeconfig --region ${var.region} --name ${module.eks.cluster_name}"
}
