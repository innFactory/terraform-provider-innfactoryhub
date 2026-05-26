# Provision a read-only org-scoped API key for a CompanyGPT customer.
# The provider data-source resolves the tenant from the GCP project the
# CompanyGPT stack is deployed into, so the operator never has to type a
# tenant_id by hand.

data "innfactoryhub_tenant" "self" {
  gcp_project_id = "companygpt-acme-prod"
}

resource "innfactoryhub_api_key" "companygpt" {
  name        = "companyGPT-${terraform.workspace}"
  scope       = "organization"
  tenant_id   = data.innfactoryhub_tenant.self.tenant_id
  permissions = ["read"]
}

output "innfactoryhub_api_key" {
  value     = innfactoryhub_api_key.companygpt.plaintext
  sensitive = true
}
