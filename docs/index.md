---
page_title: "innfactoryhub Provider"
subcategory: ""
description: |-
  Terraform provider for innFactory Hub and the innFactory AI Hub. Manages API keys, environments and licences and reads tenant identifiers.
---

# innfactoryhub Provider

The `innfactoryhub` provider talks to both hubs — [innFactory Hub](https://hub.innfactory.cloud), the reseller billing portal, and the [innFactory AI Hub](https://hub.innfactory.ai), the licence and contract portal. Both serve the same tenant-lookup and API-key paths, so one provider covers:

- Minting and revoking **API keys** scoped to a tenant (`innfactoryhub_api_key`) — both hubs.
- Resolving a tenant from a cloud handle (`data.innfactoryhub_tenant`) — useful when a deploy environment knows only its own GCP project / Azure subscription / AWS account — both hubs.
- Registering **environments** (`innfactoryhub_environment`) — one CompanyGPT / AI Gateway installation per customer — AI Hub only.
- Issuing **licences** (`innfactoryhub_license`) for an environment, with the licence key as a sensitive value — AI Hub only.

Use one provider block per hub; add an `alias` when a configuration talks to both.

The intended workflow is: at deploy-time of a customer's CompanyGPT stack, look up the right Hub tenant from the GCP project the stack runs in, mint an org-scoped read-only API key, store it as a sensitive output, and feed it into the dashboard's runtime.

## Example Usage

```terraform
terraform {
  required_providers {
    innfactoryhub = {
      source  = "innfactory/innfactoryhub"
      version = "~> 0.2"
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

### Talking to the innFactory AI Hub

```terraform
provider "innfactoryhub" {
  alias    = "aihub"
  endpoint = "https://hub.innfactory.ai"
  # admin_key may come from a count-gated secret read; it is only needed
  # once a resource or data source of this provider actually runs.
}

resource "innfactoryhub_environment" "ai_gateway" {
  provider        = innfactoryhub.aihub
  azure_tenant_id = "00000000-0000-0000-0000-000000000000"
  name            = "AI Gateway"
  domain          = "gpt.acme.example"
  cloud           = "azure"
}

resource "innfactoryhub_license" "ai_gateway" {
  provider        = innfactoryhub.aihub
  environment_id  = innfactoryhub_environment.ai_gateway.id
  product         = "ai-gateway"
  domains         = [innfactoryhub_environment.ai_gateway.domain]
  unlimited       = true
  edition         = "all"
  unbilled_reason = "internal"
}
```

## Authentication

The provider authenticates to the Hub admin API with a **system-scope** API key (`ak_<id>.<secret>` with `write` permission). Mint one in the Hub admin UI under *Stammdaten → API-Keys* and supply it via:

- `INNFACTORYHUB_ADMIN_KEY` env var (recommended), or
- the `admin_key` attribute on the provider block (mark sensitive).

The plaintext is shown exactly once in the Hub UI; copy it carefully.

A missing key does **not** fail provider configuration. It fails the first resource or data source operation that calls the Hub (`missing admin key`). This lets a configuration declare the provider unconditionally and read the key from a secret store only when the feature using it is enabled (e.g. `count = var.enabled ? 1 : 0`).

## Endpoint security

The system-scope admin key can mint and revoke API keys for every customer tenant. To prevent credential exfiltration through a typo, supply-chain compromise, or hostile module, the provider refuses to send the bearer to anything but `https://` on

- `innfactory.cloud` or any subdomain (e.g. `hub.innfactory.cloud`), or
- `innfactory.ai` or any subdomain (e.g. `hub.innfactory.ai`).

Matching is per DNS label: look-alikes such as `innfactory.ai.evil.com` or `evilinnfactory.ai` are refused.

For local dev or staging-via-tunnel, set `INNFACTORYHUB_INSECURE_ENDPOINT=1` to opt out of the host pin. Do not set this in CI.

Error messages are scrubbed before they reach Terraform output: anything shaped like an API key (`ak_…`), a licence key (`lk_…`) or a JWT (`eyJ….….…`) is replaced by a placeholder.

## Schema

### Optional

- `endpoint` (String) Base URL of the Hub admin API, e.g. `https://hub.innfactory.ai`. Defaults to `https://hub.innfactory.cloud`. Override via `INNFACTORYHUB_ENDPOINT`. Must be `https://` on `innfactory.cloud`, `innfactory.ai` or a subdomain of either unless `INNFACTORYHUB_INSECURE_ENDPOINT=1`.
- `admin_key` (String, Sensitive) System-scope admin key. Override via `INNFACTORYHUB_ADMIN_KEY`. Only checked when an operation calls the Hub.
