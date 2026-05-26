# Terraform Provider for innFactory Hub

Manages reseller API keys and reads tenant identifiers in [innFactory Hub](https://hub.innfactory.cloud).

Intended primarily for CompanyGPT customer deployments: at deploy-time of a customer's stack, resolve the right Hub tenant from the GCP project ID, mint a read-only org-scoped API key, and feed it to the dashboard's runtime as a sensitive output.

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

Export `INNFACTORYHUB_ADMIN_KEY` with a system-scope admin key (mint one in *Hub → Stammdaten → API-Keys*).

## Development

```bash
go test ./...
go build .
```

For local debugging, run `./terraform-provider-innfactoryhub --debug` and follow the printed `TF_REATTACH_PROVIDERS` instructions.

## Releases

Tagged via `git tag v0.1.0 && git push --tags`. GoReleaser cross-compiles for darwin/linux/windows × amd64/arm64, signs with GPG, and publishes to the GitHub Releases page; the public Terraform Registry pulls from there.

## Keeping the provider in sync with the Hub API

The Hub repo runs `pnpm openapi:check` in CI which fails when [`docs/api/openapi.json`](https://github.com/innFactory/innfactoryHUB/blob/main/docs/api/openapi.json) drifts from `openapi-spec.ts`. When the spec changes in a way that affects this provider:

1. Re-read the diff in `docs/api/openapi.json`.
2. Update the relevant resource/data-source shape here.
3. Cut a new minor version of this provider.

There's no automated sync — the contract is "spec changes → provider PR within the same week."

## License

MPL-2.0 (matches HashiCorp ecosystem default).
