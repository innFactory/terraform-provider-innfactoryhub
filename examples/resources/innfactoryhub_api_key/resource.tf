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

# innFactory AI Hub: the companyADMIN key of a CompanyGPT installation reads
# only the maintenance status (read_maintenance alone — read next to it would
# lift that restriction). The provider's admin key needs ["read", "licensing"].
provider "innfactoryhub" {
  alias    = "aihub"
  endpoint = "https://hub.innfactory.ai"
}

# A new key every 335 days; each key is valid for 365 days.
resource "time_rotating" "maintenance_key" {
  rotation_days = 335
}

resource "innfactoryhub_api_key" "maintenance" {
  provider         = innfactoryhub.aihub
  name             = "companyADMIN ${terraform.workspace}"
  scope            = "organization"
  tenant_id        = "tnt_abc12345" # e.g. innfactoryhub_environment.<name>.tenant_id
  permissions      = ["read_maintenance"]
  expires_at       = timeadd(time_rotating.maintenance_key.rfc3339, "8760h")
  rotation_trigger = time_rotating.maintenance_key.id
}

output "companyadmin_maintenance_key" {
  value     = innfactoryhub_api_key.maintenance.plaintext
  sensitive = true
}
