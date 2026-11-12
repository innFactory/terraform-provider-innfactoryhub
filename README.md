<a href="https://www.innfactory.de"><img src="https://innfactory.de/images/innfactory-logo.png" alt="innFactory" height="64"></a>

# Terraform Provider for innFactory Hub

Manages API keys, environments and licences and reads tenant identifiers in [innFactory Hub](https://hub.innfactory.cloud) and the [innFactory AI Hub](https://hub.innfactory.ai). Both hubs serve the same tenant-lookup and API-key paths, so one provider talks to either.

Intended primarily for CompanyGPT customer deployments: at deploy-time of a customer's stack, resolve the right Hub tenant from the GCP project ID (or Azure subscription / Azure tenant / AWS account / Hub tenant_id), register the customer's AI Gateway installation as an environment in the AI Hub, issue its licence, and mint the companyADMIN maintenance key — handed to the runtime as sensitive outputs.

## Quick start

```hcl
terraform {
  required_providers {
    innfactoryhub = {
      source  = "innfactory/innfactoryhub"
      version = "~> 0.2.0"
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

### CompanyGPT on the innFactory AI Hub

| What | Key / attribute |
|---|---|
| Apply (CI, protected branch) | `INNFACTORYHUB_ADMIN_KEY` = system key `["read", "licensing"]`, at most **90 days** |
| Plan (pull requests) | `INNFACTORYHUB_ADMIN_KEY` = system key `["read_licensing"]` (read-only; writes fail with `403 forbidden_read_only_key`) |
| companyADMIN | `innfactoryhub_api_key`: `scope = "organization"`, `permissions = ["read_maintenance"]`, `expires_at` (required, ≤ 24 months), `rotation_trigger` — see [docs/resources/api_key.md](docs/resources/api_key.md) |
| Gateway licence | `innfactoryhub_license.license_key`; rotate with `rotation_trigger` (same licence, new key) |
| Gateway without internet | Staff switch air-gap operation on in the portal (licence files only then); `innfactoryhub_license.air_gapped` unset or `true` keeps it, `false` switches it off |
| Audit | `INNFACTORYHUB_AUDIT_CONTEXT` = CI run URL, commit, customer |
| Take something away | End or suspend the licence, revoke the key — **in the Hub**. Terraform does not undo it; plans fail until `rotation_trigger` changes. |

System keys are minted by staff in the portal (TOTP); an API key never creates one (`403 forbidden_key_minting`).

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

### Issuing a licence in the innFactory AI Hub

```hcl
resource "innfactoryhub_license" "ai_gateway" {
  provider             = innfactoryhub.aihub
  environment_id       = innfactoryhub_environment.ai_gateway.id
  product              = "ai-gateway"
  domains              = ["gpt.acme.example"]
  max_users            = 50 # or: unlimited = true
  edition              = "all"
  contract_position_id = "pos_0123456789ab" # or: unbilled_reason = "trial"
}

output "license_key" {
  value     = innfactoryhub_license.ai_gateway.license_key
  sensitive = true
}
```

The licence key (`lk_…`) is returned by the Hub only when the licence is issued or its key rotated; it stays in state and survives every in-place update (`max_users`, domains, edition, billing link …). Changing `rotation_trigger` rotates the key of the same licence in place (`key_id` changes, the old key is revoked at once); changing `environment_id` or `product` issues a new licence; destroy ends the licence. A licence ended in the Hub, or a key rotated there, is flagged and never silently re-issued. After `terraform import` the key is empty — rotate to get one. See [docs/resources/license.md](docs/resources/license.md).

A gateway without internet access runs on a licence file, which the Hub issues only in air-gap operation (`air_gapped`, AI Gateway only); an admin or the customer downloads it in the portal — the provider never fetches it. Only AI Hub staff switch air-gap operation on, in the portal: from Terraform (an API key) the Hub refuses it with `403 air_gapped_staff_only`, and a new licence with `air_gapped = true` already fails at plan time. Unset in the configuration, `air_gapped` stays as the Hub has it and is only read; `true` keeps it on; `false` switches it off in place, which revokes the licence files issued before — the plan warns about it.

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

This provider holds a **system-scope admin key**. On the AI Hub, use the narrow `["read", "licensing"]` key for applies and `["read_licensing"]` for plans; still treat it like a production credential.

- The bearer is sent ONLY over HTTPS to exactly `hub.innfactory.ai` or `hub.innfactory.cloud` (default port, bare origin). Other names in these domains, look-alikes such as `hub.innfactory.ai.evil.com`, credentials, paths and queries are refused at provider configure time. `INNFACTORYHUB_INSECURE_ENDPOINT=1` together with `INNFACTORYHUB_INSECURE_ENDPOINT_HOST=<host>` (local dev / staging-via-tunnel only) opens exactly that one host — over https, or plain http if it is localhost/loopback — and every configuration warns while it is set.
- Redirects are never followed — Go would keep `Authorization` on a same-host redirect, even to http — and a 3xx is an error.
- 4xx/5xx response bodies are truncated to 512 bytes, and every error text is scrubbed for API keys (`ak_…`), licence keys (`lk_…`) and JWTs (`eyJ….….…`) before reaching Terraform diagnostics — so a misbehaving proxy that echoes the Authorization header into an error page can't leak a key into CI logs.
- The HTTP transport pins TLS ≥ 1.2.
- Every request can carry `X-Audit-Context` (`INNFACTORYHUB_AUDIT_CONTEXT` / `audit_context`): who ran Terraform ends up in the Hub's audit log. Sanitised to 256 printable ASCII characters, secrets redacted.
- Creates carry an `Idempotency-Key` and are retried once with the same key after a lost answer — no duplicate within one apply. Across applies, a licence create first looks up the running licences of the environment and product and stops if one runs (adopt it with `terraform import`), so a lost answer is never issued twice.
- Revocations in the Hub stick: ended licences, keys rotated in the portal, revoked or expired API keys stay in state with their status and are never re-issued without a `rotation_trigger` change.
- The plaintext API key and the licence key live in Terraform state (as sensitive attributes). Use a backend with encryption-at-rest (GCS+CMEK, S3+KMS, Terraform Cloud). Licence files and signed licence tokens are never stored in state.

## Development

```bash
go vet ./...
go test -race ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
go build .
```

The resource tests run real `terraform` plans against an in-process fake Hub (`httptest`) via `terraform-plugin-testing`'s `resource.UnitTest`. The fake mirrors the AI Hub's contract: key permissions (`licensing`, `read_licensing`, `read_maintenance`), key rotation, idempotent creates (and lost answers), out-of-band revocations. They need a Terraform CLI: either on `PATH`, at `TF_ACC_TERRAFORM_PATH`, or `TestMain` downloads the latest release (or `TF_ACC_TERRAFORM_VERSION`) once per test run and shares it with every test. No real Hub is ever called; the tests set `INNFACTORYHUB_INSECURE_ENDPOINT=1` with `INNFACTORYHUB_INSECURE_ENDPOINT_HOST=127.0.0.1` only for the loopback fake.

For local debugging, run `./terraform-provider-innfactoryhub --debug` and follow the printed `TF_REATTACH_PROVIDERS` instructions.

## Releases

Tagged via `git tag v0.2.0 && git push origin v0.2.0` — a manual step after approval, on a commit that is already on `main`. The `Release` workflow first runs a gate (`verify`): the tagged commit must be on `main`, the Go toolchain in use must be the one `go.mod` pins, and `go mod verify`, `go vet`, `go test -race` and govulncheck must pass. Only then the release job runs in the protected `release` environment: GoReleaser v2.19.0 (pinned) cross-compiles for darwin/linux/windows/freebsd, signs the checksum file with GPG and publishes to GitHub Releases; `actions/attest-build-provenance` attests the zips and the checksum file. The public Terraform Registry polls the releases page. Every action is pinned to a commit SHA; Dependabot proposes updates weekly.

Verify a release binary's provenance:

```bash
gh attestation verify terraform-provider-innfactoryhub_0.2.0_linux_amd64.zip --repo innFactory/terraform-provider-innfactoryhub
```

### Repository settings (manual, once)

These protect the release and cannot be set from the repository's files:

1. **Environment `release`** (*Settings → Environments → New environment*):
   - *Required reviewers*: at least one maintainer other than the person pushing the tag; enable *Prevent self-review*.
   - *Deployment branches and tags*: *Selected branches and tags* → add the tag pattern `v*` only.
   - Move the secrets `GPG_PRIVATE_KEY` and `GPG_PASSPHRASE` from repository secrets into this environment's secrets (and delete the repository-level copies), so only an approved release job can read them.
2. **Tag ruleset** (*Settings → Rules → Rulesets → New tag ruleset*): target `v*`, enforcement *Active*, rules *Restrict creations*, *Restrict updates* and *Restrict deletions*; bypass list: the release maintainers only.
3. **Branch protection for `main`**: pull request with at least one approval, required status checks `test`, `govulncheck`, `actionlint`.
4. **Actions** (*Settings → Actions → General*): *Workflow permissions* = *Read repository contents*; do not allow Actions to create or approve pull requests; under *Actions permissions* enable *Require actions to be pinned to a full-length commit SHA*.
5. **Security** (*Settings → Code security*): Dependabot alerts and Dependabot security updates on; private vulnerability reporting on.

## Keeping the provider in sync with the Hub API

Both hub repos run `pnpm openapi:check` in CI, which fails when `docs/api/openapi.json` drifts from `openapi-spec.ts`: [innfactoryHUB](https://github.com/innFactory/innfactoryHUB/blob/main/docs/api/openapi.json) (tenants, API keys) and innfactoryAIHUB (`docs/api/openapi.json`: tenants, API keys, `/api/v1/admin/environments`, `/api/v1/admin/licenses`, `/api/v1/admin/licenses/{id}/keys/rotate`). When either spec changes in a way that affects this provider:

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
