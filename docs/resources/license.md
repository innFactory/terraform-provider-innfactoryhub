---
page_title: "innfactoryhub_license Resource - innfactoryhub"
subcategory: ""
description: |-
  Issues a licence for an environment in the innFactory AI Hub and returns its licence key once.
---

# innfactoryhub_license (Resource)

Issues a **licence** for an environment (`innfactoryhub_environment`) in the [innFactory AI Hub](https://hub.innfactory.ai) — for example the AI Gateway licence of a CompanyGPT installation — and returns its **licence key** (`lk_<id>.<secret>`) as a sensitive value, ready to be handed to the gateway (`LICENSE_KEY`).

The Hub returns a licence key **only** when it issues the licence or rotates its key. The provider keeps it in state and carries it over unchanged through every in-place update; reads never touch it, but they check that it is still the licence's key (`key_id`, `key_status`). Licence files or signed licence tokens are never requested and never stored in state.

| Change | Effect |
|---|---|
| `domains`, `max_users`/`unlimited`, `max_instances`, `edition`, `features`, `entitlements` | Updated in place (`PATCH`, only the changed fields); `id` and `license_key` stay |
| `contract_position_id`, `unbilled_reason`, `unbilled_max_until` | Updated in place (`PATCH`); switching between a contract position and an unbilled reason is in place too |
| `environment_id`, `product` | Forces a new licence (the current one is ended, a new one with a new key is issued) |
| `rotation_trigger` (any change, including setting or removing it) | Rotates the key **in place**: same licence, instances and contract position; the old key is revoked at once, the new one lands in `license_key` |

Re-running `apply` without changes plans nothing and sends no write request: no new revision, no new key, no touched contract position.

`terraform destroy` **ends** the licence in the Hub (status `ended`; it stays as history) and frees its contract position. A licence the Hub does not know (`404` — the Hub never deletes licences, so this is another Hub or lost data) is dropped from state and issued anew.

### Ending, suspending and revoking in the Hub

To take a licence away, **end or suspend it in the Hub** (portal). Terraform does not undo that:

- A **suspended** licence stays suspended: `status = "suspended"`, the plan stays empty, Terraform never resumes it.
- An **ended** licence stays in state with `status = "ended"` and is **not** re-issued. Every plan that keeps the resource fails with `Licence ended in the AI Hub – change rotation_trigger to issue a new one`. Change `rotation_trigger` to issue a new licence (with a new key) on purpose, or remove the resource from the configuration.
- A **key rotated or revoked in the Hub** (portal, customer self-service) leaves the key in state dead: `key_status = "revoked"`, and every plan fails with `Licence key revoked in the AI Hub – change rotation_trigger to issue a new key` until `rotation_trigger` changes, which rotates the key of the same licence in place. Terraform never ships the revoked key silently, and never rotates on its own.

### Lost answers

Every create carries an `Idempotency-Key`. When the Hub's answer is lost (connection drop, timeout, `5xx`), the provider looks up the running licences of the environment and product and sends the same request once more with the same key. The Hub answers `409 idempotent_replay` with the licence it already issued; the provider adopts that licence and rotates its key — the key of the lost answer is revoked, the new one is stored — and warns about it. If the retry is unanswered as well, the create fails with the running licences of the environment listed, instead of issuing a second (unbilled) licence.

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

  # Change this value to rotate the licence key (same licence, new key;
  # the previous key stops working at once).
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
- `rotation_trigger` (String) Any change rotates the licence key in place; for a licence ended in the Hub it issues a new licence. See [Rotating the key](#rotating-the-key).

Plain identifiers (`environment_id`, `contract_position_id`) accept letters, digits, `_` and `-`. Feature and entitlement names are lower-case letters, digits, `_`, `.` and `-`. Invalid values fail at plan time, before any request to the Hub.

### Read-Only

- `id` (String) Hub `license_id`. Use it to import.
- `license_key` (String, Sensitive) Licence key `lk_<id>.<secret>`. Set when the licence is issued or its key rotated and kept unchanged by updates; empty after import.
- `key_id` (String) Id of the key in `license_key` (the `<id>` of `lk_<id>.<secret>`); empty when state holds no key.
- `key_status` (String) `active` while `license_key` is the licence's current key, `revoked` once the Hub revoked it (e.g. rotated in the portal); empty when state holds no key.
- `tenant_id` (String) Hub customer of the environment.
- `status` (String) `active`, `suspended`, `expired` or `ended`. Never changed by Terraform.
- `valid_until` (String) Last day the licence is valid: the end of the contract position or contract, or `unbilled_max_until`.

## Rotating the key

Change `rotation_trigger` (for example to the current month, or to the id of a `time_rotating` resource). Terraform calls `POST /api/v1/admin/licenses/<id>/keys/rotate`: the **same licence** gets a new key, every earlier key is revoked at once, and the new `lk_…` lands in `license_key` (`key_id` changes). The licence id, its instances and the contract position stay — the gateway keeps its installation history and just needs the new key. Deploy the new `license_key` to the gateway in the same apply.

A rotation together with parameter changes sends the `PATCH` first, then rotates. If the rotation fails, the parameter change is kept and the previous key and `rotation_trigger` stay in state, so the next apply retries the rotation. Rotation also works for suspended and expired licences.

Do not combine a contract position with `lifecycle { create_before_destroy = true }`: when the licence is replaced (new `environment_id` or `product`), a contract position belongs to one licence at a time, so the new licence can only be issued after the old one has released it (`409 position_already_linked` otherwise).

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
| `409 license_ended` | The licence was ended in the Hub | Change `rotation_trigger` to issue a new licence, or remove the resource |
| `409 idempotent_replay` | The Hub already issued the licence for this create (answer was lost) | Handled by the provider: it adopts the licence and rotates its key |
| `403 forbidden_read_only_key` | The admin key is read-only (`read_licensing`) | Apply with a `["read", "licensing"]` key |
| `403 forbidden_key_scope` | The admin key does not cover the request | Use a `["read", "licensing"]` key |

## Import

```shell
terraform import innfactoryhub_license.ai_gateway <license_id>
```

Import restores every attribute except `license_key` (and so `key_id`, `key_status`): the Hub stores only a hash of the key, so `license_key` is empty after import and the import prints a warning. Keep using the key that is already deployed, or set/change `rotation_trigger` to rotate the key of the imported licence in place and get the new key into state. Leave `rotation_trigger` unset in the configuration you import into unless you want that rotation on the next apply.
