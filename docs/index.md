---
page_title: "innfactoryhub Provider"
subcategory: ""
description: |-
  Terraform provider for innFactory Hub and the innFactory AI Hub. Manages API keys, environments and licences and reads tenant identifiers.
---

# innfactoryhub Provider

The `innfactoryhub` provider talks to both hubs — [innFactory Hub](https://hub.innfactory.cloud), the reseller billing portal, and the [innFactory AI Hub](https://hub.innfactory.ai), the licence and contract portal. Both serve the same tenant-lookup and API-key paths, so one provider covers:

- Minting and revoking **API keys** scoped to a tenant (`innfactoryhub_api_key`), e.g. the companyADMIN maintenance key — both hubs.
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
      version = "~> 0.2.0"
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
  # INNFACTORYHUB_AUDIT_CONTEXT (or audit_context) names the run in the
  # Hub's audit log.
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

The provider authenticates with a **system-scope** API key (`ak_<id>.<secret>`), supplied via

- `INNFACTORYHUB_ADMIN_KEY` env var (recommended), or
- the `admin_key` attribute on the provider block (mark sensitive).

Which key:

| Hub | Apply | Plan (e.g. pull requests) |
|---|---|---|
| innFactory AI Hub | `["read", "licensing"]` — environments, licences, key rotation, tenant lookup, companyADMIN keys (`["read_maintenance"]` with `expires_at`); nothing else (`403 forbidden_key_scope`) | `["read_licensing"]` alone — the same routes, read-only; writes fail with `403 forbidden_read_only_key` |
| innfactoryHUB | a key with `write` | a key with `read` |

On the AI Hub, staff mint these system keys in the portal (*Stammdaten → API-Keys*, sign-in with TOTP); no API key can create a system key (`403 forbidden_key_minting`). System keys expire after at most 12 months; keep automation keys short — **at most 90 days** for the apply key. The plaintext is shown exactly once.

A missing key does **not** fail provider configuration. It fails the first resource or data source operation that calls the Hub (`missing admin key`). This lets a configuration declare the provider unconditionally and read the key from a secret store only when the feature using it is enabled (e.g. `count = var.enabled ? 1 : 0`).

### Plans with a read-only key

`terraform plan` only reads (refresh, tenant lookup), so pull-request pipelines can run with a `["read_licensing"]` key and never hold write access. Only the apply job — after review, from the protected branch — gets the `["read", "licensing"]` key:

```yaml
# GitHub Actions
plan:
  env:
    INNFACTORYHUB_ADMIN_KEY: ${{ secrets.AIHUB_PLAN_KEY }}  # ["read_licensing"]
apply:
  environment: production
  env:
    INNFACTORYHUB_ADMIN_KEY: ${{ secrets.AIHUB_APPLY_KEY }} # ["read", "licensing"], ≤ 90 days
```

Applying with the read-only key fails with `403 forbidden_read_only_key` and a hint.

## Audit context

Set `INNFACTORYHUB_AUDIT_CONTEXT` (or the provider attribute `audit_context`) to record who ran Terraform. The provider sends it as `X-Audit-Context` on every request and the AI Hub stores it as `audit_context` with each change in its audit log (for API-key callers; it is never echoed back). It is reduced to printable ASCII on one line (line breaks become spaces, other characters `?`), anything that looks like a key or token is redacted, and it is cut to 256 characters. In CI, put the run, the commit and the customer in it:

```yaml
# GitHub Actions
env:
  INNFACTORYHUB_AUDIT_CONTEXT: "run=${{ github.server_url }}/${{ github.repository }}/actions/runs/${{ github.run_id }} commit=${{ github.sha }} tenant=${{ vars.CUSTOMER }}"
```

```yaml
# GitLab CI
variables:
  INNFACTORYHUB_AUDIT_CONTEXT: "run=$CI_JOB_URL commit=$CI_COMMIT_SHA tenant=$CUSTOMER"
```

## Revocations in the Hub

To take something away from a customer installation, do it in the Hub — Terraform does not undo it:

- **End or suspend a licence** in the portal. A suspended licence stays suspended. An ended licence stays in state with `status = "ended"`, and plans fail until `rotation_trigger` changes (which issues a new licence on purpose).
- **Rotate or revoke a licence key** in the portal. Plans fail with `key_status = "revoked"` until `rotation_trigger` changes (which rotates the key of the same licence).
- **Revoke an API key** in the portal. It stays in state with `status = "revoked"` (or `expired`, `deleted`), and plans fail until `rotation_trigger` changes.

## Endpoint security

The admin key is sent **only** to `https://hub.innfactory.ai` and `https://hub.innfactory.cloud` — exactly these hosts on the default port, as a bare origin (no credentials, path, query or fragment). Other names in these domains (`staging.innfactory.cloud`, `license.innfactory.ai`, …) are refused at provider configuration, as are look-alikes such as `hub.innfactory.ai.evil.com`.

For local development or a tunnel to staging, `INNFACTORYHUB_INSECURE_ENDPOINT=1` lifts the host pin: any `https://` host is then accepted, plain `http://` only on `localhost` or a loopback address. Do not set it in CI.

The client never follows redirects (Go would keep the `Authorization` header on a same-host redirect, even to `http://`); a `3xx` answer is an error. TLS ≥ 1.2 is enforced. Error messages are scrubbed before they reach Terraform output: anything shaped like an API key (`ak_…`), a licence key (`lk_…`) or a JWT (`eyJ….….…`) is replaced by a placeholder.

Creates of environments and licences carry an `Idempotency-Key`; a create whose answer was lost is retried once with the same key, so the Hub never registers an environment or issues a licence twice.

## Schema

### Optional

- `endpoint` (String) `https://hub.innfactory.ai` (innFactory AI Hub) or `https://hub.innfactory.cloud` (innfactoryHUB, the default). Override via `INNFACTORYHUB_ENDPOINT`. Other hosts only with `INNFACTORYHUB_INSECURE_ENDPOINT=1`.
- `admin_key` (String, Sensitive) System-scope admin key — on the AI Hub `["read", "licensing"]` to apply, `["read_licensing"]` to plan. Override via `INNFACTORYHUB_ADMIN_KEY`. Only checked when an operation calls the Hub.
- `audit_context` (String) Free text stored with every change in the Hub's audit log (`X-Audit-Context`), e.g. CI run, commit and customer; sanitised to at most 256 printable ASCII characters. Override via `INNFACTORYHUB_AUDIT_CONTEXT`.
