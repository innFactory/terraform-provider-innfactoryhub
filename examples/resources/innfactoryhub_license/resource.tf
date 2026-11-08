# Issue the AI Gateway licence for a customer's installation in the
# innFactory AI Hub and hand the licence key to the gateway as a secret.

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

variable "max_users" {
  type        = number
  default     = 50
  description = "User limit of the gateway licence; null for unlimited."
}

variable "contract_position_id" {
  type        = string
  default     = null
  description = "Contract position that bills the licence; null issues a trial."
}

resource "innfactoryhub_license" "ai_gateway" {
  environment_id = innfactoryhub_environment.ai_gateway.id
  product        = "ai-gateway"
  domains        = [innfactoryhub_environment.ai_gateway.domain]
  edition        = "all"

  # Either a user limit or unlimited = true.
  max_users = var.max_users
  unlimited = var.max_users == null

  # Exactly one: the contract position that bills the licence, or why it is
  # not billed (trial | partner | internal; end defaults per reason).
  contract_position_id = var.contract_position_id
  unbilled_reason      = var.contract_position_id == null ? "trial" : null

  # Change this value to rotate the licence key (same licence, new key; the
  # previous key stops working at once).
  rotation_trigger = "2026-11"
}

# Hand the key to the gateway, e.g. as the LICENSE_KEY secret.
output "license_key" {
  value     = innfactoryhub_license.ai_gateway.license_key
  sensitive = true
}

output "license_valid_until" {
  value = innfactoryhub_license.ai_gateway.valid_until
}
