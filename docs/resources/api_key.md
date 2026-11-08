---
page_title: "innfactoryhub_api_key Resource - innfactoryhub"
subcategory: ""
description: |-
  Mints an API key in innFactory Hub. Plaintext is returned exactly once at create time.
---

# innfactoryhub_api_key (Resource)

Mints an API key in innFactory Hub. The plaintext token is returned exactly once at create time; it can be read from Terraform state thereafter (always treat as sensitive).

`terraform destroy` revokes the key (soft delete — the Hub keeps the metadata + revocation timestamp for audit). A key revoked outside Terraform is dropped on refresh and minted anew on the next apply.

Works against both hubs. On the innFactory AI Hub (`endpoint = "https://hub.innfactory.ai"`) use it for **organization** keys, e.g. the read-only key a customer installation uses for `GET /api/v1/customer/maintenance`. The AI Hub gives every system key an expiry of at most 12 months and this resource sends none, so a system key minted here expires after 12 months; mint system keys in the portal instead.

## Example Usage

### innFactory Hub

```terraform
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
```

### innFactory AI Hub (organization key of an environment's customer)

```terraform
provider "innfactoryhub" {
  alias    = "aihub"
  endpoint = "https://hub.innfactory.ai"
}

resource "innfactoryhub_api_key" "maintenance" {
  provider    = innfactoryhub.aihub
  name        = "companyGPT-acme-companyadmin"
  scope       = "organization"
  tenant_id   = innfactoryhub_environment.ai_gateway.tenant_id
  permissions = ["read"]
}
```

## Schema

### Required

- `name` (String) Human-readable name. Multiple keys with the same name per tenant are allowed. Forces replace.
- `scope` (String) `system` (no tenant binding, read/write allowed) or `organization` (bound to a `tenant_id`, currently read-only). Forces replace.
- `permissions` (Set of String) Subset of `["read", "write"]` (the AI Hub also knows `read_accounting` for system keys). Organization-scoped keys must request only `["read"]`. Forces replace (the Hub cannot edit permissions in place).

### Optional

- `tenant_id` (String) Required for `scope = organization`; must be omitted for `scope = system`. Forces replace.

### Read-Only

- `id` (String) Stable Hub-side key identifier.
- `plaintext` (String, Sensitive) The full `ak_<id>.<secret>` token.
- `display_prefix` (String) First 8 characters of the plaintext.
- `created_at` (String) RFC-3339 timestamp the key was minted.

## Import

```shell
terraform import innfactoryhub_api_key.example <key_id>
```

> **Note:** `plaintext` is **NOT** recoverable after import — the Hub does not store it. If a downstream module needs the plaintext, mint a fresh key instead of importing.
