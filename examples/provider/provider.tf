terraform {
  required_providers {
    innfactoryhub = {
      source  = "innfactory/innfactoryhub"
      version = "~> 0.2.0"
    }
  }
}

provider "innfactoryhub" {
  # endpoint defaults to https://hub.innfactory.cloud
  # admin_key is best supplied via INNFACTORYHUB_ADMIN_KEY env var.
}

# Second instance for the innFactory AI Hub (environments, licences,
# companyADMIN keys). INNFACTORYHUB_ADMIN_KEY: a system key with
# ["read", "licensing"] to apply, ["read_licensing"] for plans.
provider "innfactoryhub" {
  alias    = "aihub"
  endpoint = "https://hub.innfactory.ai"

  # Stored with every change in the Hub's audit log. In CI rather set
  # INNFACTORYHUB_AUDIT_CONTEXT, e.g. to the run URL, commit and customer.
  audit_context = "terraform ${terraform.workspace}"
}
