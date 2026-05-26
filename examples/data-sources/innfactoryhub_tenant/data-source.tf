# Resolve the Hub tenant from a GCP project ID. Use any one of the three
# identifier attributes — exactly one must be set.

data "innfactoryhub_tenant" "by_gcp" {
  gcp_project_id = "companygpt-acme-prod"
}

data "innfactoryhub_tenant" "by_azure" {
  azure_subscription_id = "00000000-0000-0000-0000-000000000000"
}

data "innfactoryhub_tenant" "by_aws" {
  aws_account_id = "123456789012"
}

output "tenant_id" {
  value = data.innfactoryhub_tenant.by_gcp.tenant_id
}

output "tenant_other_handles" {
  value = {
    gcp   = data.innfactoryhub_tenant.by_gcp.gcp_project_ids
    azure = data.innfactoryhub_tenant.by_gcp.azure_subscription_ids
    aws   = data.innfactoryhub_tenant.by_gcp.aws_account_ids
  }
}
