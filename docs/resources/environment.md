---
page_title: "innfactoryhub_environment Resource - innfactoryhub"
subcategory: ""
description: |-
  Registers an environment (one CompanyGPT / AI Gateway installation of a customer) in the innFactory AI Hub.
---

# innfactoryhub_environment (Resource)

Registers an **environment** — one CompanyGPT / AI Gateway installation of a customer — in the [innFactory AI Hub](https://hub.innfactory.ai). Licences ([`innfactoryhub_license`](license.md)) hang off the environment's `id`; the Hub deletes an environment only after its licences have ended, and Terraform destroys the licences first when both are in one configuration.

The customer is named by **exactly one** of `tenant_id`, `azure_tenant_id`, `azure_tenant_domain`, `azure_subscription_id`, `gcp_project_id` or `aws_account_id`. For a cloud identifier the Hub resolves the customer at create time (same rules as `data "innfactoryhub_tenant"`) and returns it as `tenant_id`.

| Change | Effect |
|---|---|
| `name`, `domain` | Updated in place (`PATCH`), `id` stays the same |
| `cloud` | Forces a new environment |
| Customer binding (any identifier or a configured `tenant_id`, including switching between them) | Forces a new environment |

`terraform destroy` deletes the environment in the Hub. Re-running `apply` without changes sends no write request.

The provider must point at the AI Hub (`endpoint = "https://hub.innfactory.ai"`); the innfactoryHUB on `innfactory.cloud` does not serve environments.

## Example Usage

```terraform
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
```

## Schema

### Required

- `name` (String) Display name in the Hub. Must not be blank. Updated in place.
- `domain` (String) Lower-case host name the installation is served under, e.g. `gpt.example.com` — no scheme, port, path or wildcard. Updated in place.
- `cloud` (String) `azure`, `gcp`, `aws`, `stackit` or `other`. Forces replacement.

### Optional (set exactly one)

- `tenant_id` (String) Hub customer (`tnt_…`) to bind to directly. Forces replacement when a configured value changes. Also computed (see below).
- `azure_tenant_id` (String) Azure (Entra ID) tenant / directory ID of the customer. Forces replacement.
- `azure_tenant_domain` (String) Azure (Entra ID) tenant domain, e.g. `contoso.onmicrosoft.com`. Forces replacement.
- `azure_subscription_id` (String) Azure subscription ID of the customer. Forces replacement.
- `gcp_project_id` (String) GCP project ID of the customer. Forces replacement.
- `aws_account_id` (String) AWS account ID (12-digit) of the customer. Forces replacement.

Blank (whitespace-only) values are rejected at plan time.

### Read-Only

- `id` (String) Hub `environment_id`.
- `tenant_id` (String) The resolved Hub customer when a cloud identifier is used.

## Errors

| Hub answer | Meaning | Fix |
|---|---|---|
| `404 tenant_not_found` | No customer carries the identifier | Add the identifier to the customer in the Hub, or use `tenant_id` |
| `409 ambiguous_lookup` | Several customers carry the identifier | Clean up the identifiers in the Hub, or use `tenant_id` |
| `409 environment_exists` | The environment is already registered | `terraform import` it instead of creating it again |

Nothing is written to state when the Hub rejects a create.

## Import

```shell
terraform import innfactoryhub_environment.ai_gateway <environment_id>
```

After import, `tenant_id` and — when the Hub reports how the environment was registered (`identifier_used`) — the matching lookup attribute are filled in. Environments created in the portal carry no lookup identifier: import them into a configuration that binds them with `tenant_id`, otherwise the next plan replaces them.
