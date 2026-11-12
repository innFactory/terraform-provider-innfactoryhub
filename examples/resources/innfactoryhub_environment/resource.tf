# Register a customer's AI Gateway installation in the innFactory AI Hub.
# The Hub resolves the customer from the Azure (Entra ID) tenant the
# CompanyGPT stack is deployed into, so nobody has to type a tenant_id.

provider "innfactoryhub" {
  endpoint = "https://hub.innfactory.ai"
  # admin_key is best supplied via INNFACTORYHUB_ADMIN_KEY.
}

data "azurerm_client_config" "current" {}

resource "innfactoryhub_environment" "ai_gateway" {
  azure_tenant_id = data.azurerm_client_config.current.tenant_id
  name            = "AI Gateway ${terraform.workspace}"
  domain          = "gpt.acme.example"
  cloud           = "azure"
}

# Alternative: bind directly to a known Hub customer.
resource "innfactoryhub_environment" "by_hub_id" {
  tenant_id = "tnt_abc12345"
  name      = "Staging"
  domain    = "gpt-staging.acme.example"
  cloud     = "gcp"
}

output "environment_id" {
  value = innfactoryhub_environment.ai_gateway.id
}

output "customer" {
  value = innfactoryhub_environment.ai_gateway.tenant_id
}
