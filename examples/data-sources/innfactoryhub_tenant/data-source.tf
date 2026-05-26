# Resolve the Hub tenant by ONE identifier. Five inputs are supported;
# pass exactly one. Whichever you pass, the response carries ALL of the
# tenant's identifier arrays so you can enumerate the other handles in
# one call.

data "innfactoryhub_tenant" "by_gcp" {
  gcp_project_id = "companygpt-acme-prod"
}

data "innfactoryhub_tenant" "by_azure_subscription" {
  azure_subscription_id = "00000000-0000-0000-0000-000000000000"
}

# Azure tenant ID = Entra ID directory. Different from a subscription —
# useful when an SSO-integrated install only knows its AAD tenant.
data "innfactoryhub_tenant" "by_azure_tenant" {
  azure_tenant_id = "11111111-2222-3333-4444-555555555555"
}

data "innfactoryhub_tenant" "by_aws" {
  aws_account_id = "123456789012"
}

# Direct fetch when the Hub tenant_id is already known. No scan.
data "innfactoryhub_tenant" "by_hub_id" {
  tenant_id = "tnt_abc12345"
}

output "tenant_id" {
  value = data.innfactoryhub_tenant.by_gcp.tenant_id
}

output "tenant_other_handles" {
  value = {
    gcp           = data.innfactoryhub_tenant.by_gcp.gcp_project_ids
    azure_subs    = data.innfactoryhub_tenant.by_gcp.azure_subscription_ids
    azure_tenants = data.innfactoryhub_tenant.by_gcp.azure_tenant_ids
    aws           = data.innfactoryhub_tenant.by_gcp.aws_account_ids
  }
}
