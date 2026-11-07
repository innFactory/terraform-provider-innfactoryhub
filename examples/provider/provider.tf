terraform {
  required_providers {
    innfactoryhub = {
      source  = "innfactory/innfactoryhub"
      version = "~> 0.1"
    }
  }
}

provider "innfactoryhub" {
  # endpoint defaults to https://hub.innfactory.cloud
  # admin_key is best supplied via INNFACTORYHUB_ADMIN_KEY env var.
}

# Second instance for the innFactory AI Hub (environments, licences).
provider "innfactoryhub" {
  alias    = "aihub"
  endpoint = "https://hub.innfactory.ai"
}
