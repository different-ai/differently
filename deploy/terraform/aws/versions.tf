terraform {
  # UNVALIDATED: this module has not been run through `terraform validate`,
  # `plan` or `apply`. See README.md.
  required_version = ">= 1.5.7"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.0"
    }
  }
}
