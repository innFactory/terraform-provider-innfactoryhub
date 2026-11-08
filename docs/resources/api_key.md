---
page_title: "innfactoryhub_api_key Resource - innfactoryhub"
subcategory: ""
description: |-
  Mints an API key in innFactory Hub or the innFactory AI Hub. Plaintext is returned exactly once at create time.
---

# innfactoryhub_api_key (Resource)

Mints an API key in innFactory Hub or the [innFactory AI Hub](https://hub.innfactory.ai). The plaintext token is returned exactly once at create time; it can be read from Terraform state thereafter (always treat as sensitive).

`terraform destroy` revokes the key (soft delete — the Hub keeps the metadata and the revocation time for audit).

**Revocations in the Hub stay effective.** A key revoked in the Hub, deleted there, or past its `expires_at` is **not** minted again on the next apply. It stays in state with `status` `revoked`, `deleted` or `expired`, and every plan that keeps it fails:

```
Error: API key revoked in the Hub – change rotation_trigger to mint a new one
```

Change `rotation_trigger` to mint a new key on purpose, or remove the resource from the configuration to retire it. To take a key away from a customer installation, revoke it in the Hub — Terraform does not undo that.

On the AI Hub, Terraform manages **organization** keys: an API key can never create a system key there (`403 forbidden_key_minting`); staff mint system keys in the portal (with TOTP). With a `["read", "licensing"]` admin key the provider may create exactly the companyADMIN key: `scope = "organization"`, `permissions = ["read_maintenance"]`, `expires_at` at most 24 months ahead.

## Example Usage

### companyADMIN maintenance key (innFactory AI Hub)

companyADMIN reads only the maintenance status of its environment (`GET /api/v1/customer/maintenance`). Give it an organization key with `read_maintenance` alone and an expiry, and rotate it before it expires with a [`time_rotating`](https://registry.terraform.io/providers/hashicorp/time/latest/docs/resources/rotating) resource:

```terraform
provider "innfactoryhub" {
  alias    = "aihub"
  endpoint = "https://hub.innfactory.ai"
  # INNFACTORYHUB_ADMIN_KEY: a system key with ["read", "licensing"].
}

# A new key every 335 days; each key is valid for 365, so the old one is
# replaced a month before it expires.
resource "time_rotating" "maintenance_key" {
  rotation_days = 335
}

resource "innfactoryhub_api_key" "maintenance" {
  provider         = innfactoryhub.aihub
  name             = "companyADMIN ${terraform.workspace}"
  scope            = "organization"
  tenant_id        = innfactoryhub_environment.ai_gateway.tenant_id
  permissions      = ["read_maintenance"]
  expires_at       = timeadd(time_rotating.maintenance_key.rfc3339, "8760h")
  rotation_trigger = time_rotating.maintenance_key.id
}

output "companyadmin_maintenance_key" {
  value     = innfactoryhub_api_key.maintenance.plaintext
  sensitive = true
}
```

`read` next to `read_maintenance` lifts the restriction — the key then reads everything a customer user of the tenant sees — so the provider warns about that combination.

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

## Schema

### Required

- `name` (String) Human-readable name. Multiple keys with the same name per tenant are allowed. Forces replace.
- `scope` (String) `system` (no tenant binding) or `organization` (bound to `tenant_id`, read-only). Forces replace.
- `permissions` (Set of String) Forces replace (the Hub cannot edit permissions in place). Checked at plan time:

  | Permission | Scope | Meaning |
  |---|---|---|
  | `read` | both | Read like an admin (system) or like a customer user of the tenant (organization) |
  | `read_maintenance` | organization | AI Hub: only `GET /customer/maintenance` (companyADMIN) — unless `read` is set too |
  | `write` | system | Everything |
  | `read_accounting` | system | Read like accounting |
  | `licensing` | system | AI Hub, together with `read`: environments, licences, key rotation, tenant lookup, companyADMIN keys — for Terraform applies |
  | `read_licensing` | system | AI Hub: the same routes, read-only — for Terraform plans |

### Optional

- `tenant_id` (String) Required for `scope = organization`; must be omitted for `scope = system`. Forces replace.
- `expires_at` (String) Expiry, RFC 3339 (e.g. `2027-06-30T00:00:00Z`); sent in UTC. AI Hub limits: organization keys at most 24 months ahead (optional), system keys at most 12 months (default 12 months). Another notation of the same instant is no change. Changing it mints a new key; removing it keeps the key. Also computed: without it, the Hub's value is recorded.
- `rotation_trigger` (String) Any change (including setting or removing it) revokes this key and mints a new one. The only way to replace a key whose `status` is not `active`.

### Read-Only

- `id` (String) Stable Hub-side key identifier.
- `plaintext` (String, Sensitive) The full `ak_<id>.<secret>` token.
- `display_prefix` (String) First 8 characters of the plaintext.
- `created_at` (String) RFC-3339 timestamp the key was minted.
- `status` (String) `active`, `revoked`, `expired` or `deleted`. `deleted` means the Hub no longer lists the key — revoked and deleted, or invisible to the admin key (a `licensing` key lists organization keys only).

## Errors

| Hub answer | Meaning | Fix |
|---|---|---|
| `403 forbidden_key_minting` | API keys cannot create system keys (AI Hub) | Mint system keys in the portal; manage organization keys with Terraform |
| `403 forbidden_key_scope` | The admin key's permissions do not cover the request (e.g. a `licensing` key creating anything but a `read_maintenance` key) | Use the companyADMIN shape above, or a broader admin key |
| `403 forbidden_read_only_key` | The admin key is read-only (`read_licensing`) | Apply with `["read", "licensing"]` |
| `400 bad_request` with `details.max_expires_at` | `expires_at` beyond the limit | Choose an earlier `expires_at` |

## Import

```shell
terraform import innfactoryhub_api_key.example <key_id>
```

> **Note:** `plaintext` is **NOT** recoverable after import — the Hub does not store it. If a downstream module needs the plaintext, change `rotation_trigger` to mint a fresh key.
