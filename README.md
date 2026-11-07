<a href="https://www.innfactory.de"><img src="https://innfactory.de/images/innfactory-logo.png" alt="innFactory" height="64"></a>

# Terraform Provider for innFactory Hub

Manages API keys and environments and reads tenant identifiers in [innFactory Hub](https://hub.innfactory.cloud) and the [innFactory AI Hub](https://hub.innfactory.ai). Both hubs serve the same tenant-lookup and API-key paths, so one provider talks to either.

Intended primarily for CompanyGPT customer deployments: at deploy-time of a customer's stack, resolve the right Hub tenant from the GCP project ID (or Azure subscription / Azure tenant / AWS account / Hub tenant_id), mint a read-only org-scoped API key, and feed it to the dashboard's runtime as a sensitive output — and register the customer's AI Gateway installation as an environment in the AI Hub.

## Quick start

```hcl
terraform {
  required_providers {
    innfactoryhub = {
      source  = "innfactory/innfactoryhub"
      version = "~> 0.1"
    }
  }
}

provider "innfactoryhub" {}

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

Export `INNFACTORYHUB_ADMIN_KEY` with a system-scope admin key (mint one in *Hub → Stammdaten → API-Keys*). A missing key only fails the first operation that calls the Hub, not the provider configuration — so a `count`-gated secret read is enough.

### Registering an environment in the innFactory AI Hub

```hcl
provider "innfactoryhub" {
  alias    = "aihub"
  endpoint = "https://hub.innfactory.ai"
}

resource "innfactoryhub_environment" "ai_gateway" {
  provider        = innfactoryhub.aihub
  azure_tenant_id = data.azurerm_client_config.current.tenant_id
  name            = "AI Gateway"
  domain          = "gpt.acme.example"
  cloud           = "azure"
}
```

Bind the environment with exactly one of `tenant_id`, `azure_tenant_id`, `azure_tenant_domain`, `azure_subscription_id`, `gcp_project_id`, `aws_account_id`. `name` and `domain` update in place; `cloud` and the customer binding force a new environment. Import with `terraform import innfactoryhub_environment.<name> <environment_id>`. See [docs/resources/environment.md](docs/resources/environment.md).

### Resolving a tenant — five ways

Pass exactly one of these to `data "innfactoryhub_tenant"`:

| Input | Use when |
|---|---|
| `tenant_id` | You already know the Hub `tnt_…` id (direct fetch, no scan) |
| `gcp_project_id` | The deploy env runs on GCP and knows its project ID |
| `azure_subscription_id` | The deploy env knows its Azure subscription |
| `azure_tenant_id` | You only know the Entra ID directory (Azure tenant) |
| `aws_account_id` | The deploy env knows its 12-digit AWS account |

## Security model

This provider holds a **system-scope admin key** that can mint and revoke keys for every customer tenant. Treat it like root.

- The bearer is sent ONLY over HTTPS to `innfactory.cloud`, `innfactory.ai` or a subdomain of either (e.g. `hub.innfactory.cloud`, `hub.innfactory.ai`). Matching is per DNS label, so `innfactory.ai.evil.com` is refused. Any other endpoint is refused at provider configure time unless you opt out explicitly with `INNFACTORYHUB_INSECURE_ENDPOINT=1` (intended for local dev / staging-via-tunnel).
- 4xx/5xx response bodies are truncated to 512 bytes, and every error text is scrubbed for API keys (`ak_…`), licence keys (`lk_…`) and JWTs (`eyJ….….…`) before reaching Terraform diagnostics — so a misbehaving proxy that echoes the Authorization header into an error page can't leak a key into CI logs.
- The HTTP transport pins TLS ≥ 1.2.
- The plaintext token lives in Terraform state. Use a backend with encryption-at-rest (GCS+CMEK, S3+KMS, Terraform Cloud).

## Development

```bash
go test ./...
go build .
```

The resource tests run real `terraform` plans against an in-process fake Hub (`httptest`) via `terraform-plugin-testing`'s `resource.UnitTest`. They need a Terraform CLI: either on `PATH`, at `TF_ACC_TERRAFORM_PATH`, or the harness downloads the latest release. No real Hub is ever called; the tests set `INNFACTORYHUB_INSECURE_ENDPOINT=1` only for the loopback fake.

For local debugging, run `./terraform-provider-innfactoryhub --debug` and follow the printed `TF_REATTACH_PROVIDERS` instructions.

## Releases

Tagged via `git tag v0.1.0 && git push --tags`. GoReleaser cross-compiles for darwin/linux/windows × amd64/arm64, signs the checksum file with GPG, and publishes to GitHub Releases. The public Terraform Registry polls that page automatically.

## Keeping the provider in sync with the Hub API

The Hub repo runs `pnpm openapi:check` in CI which fails when [`docs/api/openapi.json`](https://github.com/innFactory/innfactoryHUB/blob/main/docs/api/openapi.json) drifts from `openapi-spec.ts`. When the spec changes in a way that affects this provider:

1. Re-read the diff in `docs/api/openapi.json`.
2. Update the relevant resource/data-source shape here.
3. Cut a new minor version of this provider.

There's no automated sync — the contract is "spec changes → provider PR within the same week."

---

## Credits

Built and maintained by [innFactory GmbH](https://www.innfactory.de) — cloud, software, and AI consulting.

- 🌐 Web · [innfactory.de](https://www.innfactory.de)
- 🤖 AI offering · [innfactory.ai](https://www.innfactory.ai)

## License

MPL-2.0 (matches HashiCorp ecosystem default).
