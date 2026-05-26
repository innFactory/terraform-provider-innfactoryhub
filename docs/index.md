---
page_title: "innfactoryhub Provider"
subcategory: ""
description: |-
  Terraform provider for innFactory Hub. Manages reseller API keys and reads tenant identifiers.
---

# innfactoryhub Provider

The `innfactoryhub` provider manages resources in [innFactory Hub](https://hub.innfactory.cloud), the reseller billing portal. Today it covers:

- Minting and revoking **API keys** scoped to a tenant (`innfactoryhub_api_key`).
- Resolving a tenant from a cloud handle (`data.innfactoryhub_tenant`) — useful when a deploy environment knows only its own GCP project / Azure subscription / AWS account.

The intended workflow is: at deploy-time of a customer's CompanyGPT stack, look up the right Hub tenant from the GCP project the stack runs in, mint an org-scoped read-only API key, store it as a sensitive output, and feed it into the dashboard's runtime.

## Example Usage

```terraform
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
  # admin_key is best supplied via INNFACTORYHUB_ADMIN_KEY
}

data "innfactoryhub_tenant" "self" {
  gcp_project_id = "companygpt-acme-prod"
}

resource "innfactoryhub_api_key" "companygpt" {
  name        = "companyGPT-${terraform.workspace}"
  scope       = "organization"
  tenant_id   = data.innfactoryhub_tenant.self.tenant_id
  permissions = ["read"]
}
```

## Authentication

The provider authenticates to the Hub admin API with a **system-scope** API key (`ak_<id>.<secret>` with `write` permission). Mint one in the Hub admin UI under *Stammdaten → API-Keys* and supply it via:

- `INNFACTORYHUB_ADMIN_KEY` env var (recommended), or
- the `admin_key` attribute on the provider block (mark sensitive).

The plaintext is shown exactly once in the Hub UI; copy it carefully.

## Schema

### Optional

- `endpoint` (String) Base URL of the Hub admin API. Defaults to `https://hub.innfactory.cloud`. Override via `INNFACTORYHUB_ENDPOINT`.
- `admin_key` (String, Sensitive) System-scope admin key. Override via `INNFACTORYHUB_ADMIN_KEY`.
