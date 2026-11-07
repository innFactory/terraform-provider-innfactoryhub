---
page_title: "innfactoryhub_license Resource - innfactoryhub"
subcategory: ""
description: |-
  Issues a licence for an environment in the innFactory AI Hub and returns its licence key once.
---

# innfactoryhub_license (Resource)

Issues a **licence** for an environment (`innfactoryhub_environment`) in the [innFactory AI Hub](https://hub.innfactory.ai) — for example the AI Gateway licence of a CompanyGPT installation — and returns its **licence key** (`lk_<id>.<secret>`) as a sensitive value, ready to be handed to the gateway (`LICENSE_KEY`).

The Hub returns the licence key **only once**, when it issues the licence. The provider keeps it in state and carries it over unchanged through every in-place update; reads never touch it. Licence files or signed licence tokens are never requested and never stored in state.

| Change | Effect |
|---|---|
| `domains`, `max_users`/`unlimited`, `max_instances`, `edition`, `features`, `entitlements` | Updated in place (`PATCH`, only the changed fields); `id` and `license_key` stay |
| `contract_position_id`, `unbilled_reason`, `unbilled_max_until` | Updated in place (`PATCH`); switching between a contract position and an unbilled reason is in place too |
| `environment_id`, `product` | Forces a new licence (the current one is ended, a new one with a new key is issued) |
| `rotation_trigger` (any change, including setting or removing it) | Forces a new licence — this is how the key is rotated |

Re-running `apply` without changes plans nothing and sends no write request: no new revision, no new key, no touched contract position.

`terraform destroy` **ends** the licence in the Hub (status `ended`; it stays as history) and frees its contract position. A licence the Hub does not know any more (`404`) counts as ended. A licence ended outside Terraform (e.g. in the portal) is dropped from state on refresh and issued anew on the next apply.

The provider must point at the AI Hub (`endpoint = "https://hub.innfactory.ai"`).

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

  # Change this value to rotate the licence key (issues a new licence).
  rotation_trigger = "2026-11"
}

# Hand the key to the gateway, e.g. as the LICENSE_KEY secret.
output "license_key" {
  value     = innfactoryhub_license.ai_gateway.license_key
  sensitive = true
}
```

## Schema

### Required

- `environment_id` (String) Environment the licence is issued for, usually `innfactoryhub_environment.<name>.id`. Forces replacement.
- `product` (String) `ai-gateway`, `ai-prices` or `companygpt-maintenance`. Forces replacement.
- `domains` (Set of String) 1–50 lower-case host names the licence is valid for, e.g. `gpt.example.com`. `*.example.com` covers exactly one label below `example.com` (the wildcard only as the whole leftmost label). No scheme, port or path.
- `edition` (String) Edition of the catalog product, e.g. `all`.

### Optional

- `max_users` (Number) User limit, 1–1000000. Set **either** this **or** `unlimited = true` (checked at plan time).
- `unlimited` (Boolean) `true` issues the licence without a user limit. Defaults to `false`; `unlimited = false` next to `max_users` is allowed, so `unlimited = var.max_users == null` works.
- `max_instances` (Number) Installations that may use the key at the same time, 1–100. Defaults to `5`.
- `features` (Set of String) Subset of the edition's features. Without it the licence gets all features of the edition (also after an edition change). Removing a configured value keeps the current features; list them all to grant all again.
- `entitlements` (Set of String) Additional entitlements, e.g. `ai-prices`. Defaults to none.
- `contract_position_id` (String) Contract position that bills the licence. Set **exactly one** of `contract_position_id` and `unbilled_reason` (checked at plan time).
- `unbilled_reason` (String) Why the licence is not billed: `trial`, `partner` or `internal`.
- `unbilled_max_until` (String) Last day (`YYYY-MM-DD`) of an unbilled licence; only together with `unbilled_reason`. Without it the Hub applies its default per reason — trial 30 days, partner and internal 12 months from the day of issue (limits: trial 60 days, partner and internal 12 months). The applied date is recorded here; re-applying the same reason does not extend it. A later date extends the licence. Also computed.
- `rotation_trigger` (String) Any change replaces the licence and so issues a new licence key. See [Rotating the key](#rotating-the-key).

Plain identifiers (`environment_id`, `contract_position_id`) accept letters, digits, `_` and `-`. Feature and entitlement names are lower-case letters, digits, `_`, `.` and `-`. Invalid values fail at plan time, before any request to the Hub.

### Read-Only

- `id` (String) Hub `license_id`. Use it to import.
- `license_key` (String, Sensitive) Licence key `lk_<id>.<secret>`. Set only when the licence is issued and kept unchanged by updates; empty after import.
- `tenant_id` (String) Hub customer of the environment.
- `status` (String) `active`, `suspended` or `expired`.
- `valid_until` (String) Last day the licence is valid: the end of the contract position or contract, or `unbilled_max_until`.

## Rotating the key

The Hub hands out a licence key only when it issues a licence, so rotation replaces the licence: change `rotation_trigger` (for example to the current month, or to the id of a `time_rotating` resource). Terraform ends the current licence first — its key stops working — and then issues a new licence with a new key for the same environment, parameters and contract position. Deploy the new `license_key` to the gateway.

Do not combine a contract position with `lifecycle { create_before_destroy = true }`: a contract position belongs to one licence at a time, so the new licence can only be issued after the old one has released it (`409 position_already_linked` otherwise).

## Errors

Hub rejections are shown with their code, the Hub's `details` and a hint. Nothing is written to state when the Hub rejects a create.

| Hub answer | Meaning | Fix |
|---|---|---|
| `404 environment_not_found` | No environment with this id | Reference `innfactoryhub_environment.<name>.id` |
| `404 position_not_found` | No such contract position (or removed) | Check `contract_position_id` against the contract in the portal |
| `409 position_already_linked` | Another licence holds the position (`details.license_id`) | Import that licence, or end it first |
| `409 position_tenant_mismatch` | Position of another customer | Use a position of the environment's customer |
| `409 position_product_mismatch` | Position for another product | Use a position of the licence's product |
| `409 position_ended` | The position has ended | Link a running position or issue the licence unbilled |
| `422 edition_unknown` | Edition not in the catalog product (`details.available`) | Use one of the listed editions |
| `422 feature_not_in_edition` | Features outside the edition (`details.features`) | Narrow `features`, or omit it for all features |
| `422 invalid_domain` | Domains the Hub rejects (`details.domains`) | Use host names or `*.` host names |
| `400 unbilled_max_exceeded` | End beyond the limit (`details.max_until_limit`) | Choose an earlier `unbilled_max_until` |
| `400 unbilled_max_in_past` | End in the past | Choose today or later |
| `409 license_ended` | The licence was ended in the Hub | Apply again: refresh drops it and a new licence is issued |

## Import

```shell
terraform import innfactoryhub_license.ai_gateway <license_id>
```

Import restores every attribute except `license_key`: the Hub stores only a hash of the key, so `license_key` is empty after import and the import prints a warning. Keep using the key that is already deployed, or set/change `rotation_trigger` to issue a new licence with a new key. Leave `rotation_trigger` unset in the configuration you import into unless you want that rotation on the next apply.
