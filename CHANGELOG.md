# Changelog

## v0.2.0 — unreleased

Prepared for release; the tag `v0.2.0` is pushed manually after approval (GoReleaser publishes to the Registry).

**Added**

- Resource `innfactoryhub_license` (innFactory AI Hub): issues a licence for an environment via `POST /api/v1/admin/licenses` with `environment_id`, `product`, `domains`, `max_users` or `unlimited`, `max_instances` (default 5), `edition`, `features` (default: all of the edition), `entitlements`, and exactly one of `contract_position_id` or `unbilled_reason` (+ optional `unbilled_max_until`; without it the Hub's default per reason applies) — both "exactly one" rules are checked at plan time.
  - `license_key` (sensitive) is set only from the create response and kept unchanged through updates; Read never touches it. After import it is empty, with a warning.
  - Parameter and billing-link changes update in place with one `PATCH` carrying only the changed fields; re-applying unchanged config plans nothing. `environment_id` and `product` force a new licence.
  - Key rotation via `rotation_trigger`: any change replaces the licence (the Hub has no rotation endpoint and returns keys only at issue).
  - Destroy ends the licence (`DELETE`, status `ended`, contract position freed); `404` is tolerated. A licence ended outside Terraform is dropped on refresh and re-issued.
  - Hub rejections (`position_already_linked`, `edition_unknown`, `unbilled_max_exceeded`, …) come with a remediation hint. No licence file or token is ever stored in state.
- Hub error `details` (e.g. available editions, the licence holding a position) are shown in diagnostics, capped and redacted like the message.

- Resource `innfactoryhub_environment` (innFactory AI Hub): registers a customer installation via `POST /api/v1/admin/environments`, bound by exactly one of `tenant_id`, `azure_tenant_id`, `azure_tenant_domain`, `azure_subscription_id`, `gcp_project_id`, `aws_account_id`. `name` and `domain` update in place (`PATCH`); `cloud` and the customer binding force replacement; import by `environment_id`. Hub errors `tenant_not_found`, `ambiguous_lookup` and `environment_exists` come with a remediation hint.
- Endpoint allowlist accepts `innfactory.ai` and its subdomains (e.g. `https://hub.innfactory.ai`) besides `innfactory.cloud`; both apexes are accepted too.
- Test suite: `resource.UnitTest` against an `httptest` fake Hub (create, in-place update, replacement, import, validation, errors, redaction). New dependencies `terraform-plugin-framework-validators` v0.15.0 and `terraform-plugin-testing` v1.11.0.

- Data source `innfactoryhub_tenant` accepts two more inputs: `tenant_id` (direct fetch, no scan) and `azure_tenant_id` (Entra ID directory — distinct from a subscription).
- `azure_tenant_ids` returned as a computed list attribute.

**Changed**

- A missing admin key no longer fails provider configuration; it fails the first operation that calls the Hub (`missing admin key`). Allows count-gated secret reads in callers such as CompanyGPT.
- A trailing slash on `endpoint` is tolerated.

**Fixed**

- `innfactoryhub_api_key`: changing `permissions` now forces replacement instead of failing with "Update not supported".

**Security**

- Error redaction also covers licence keys (`lk_…`) and JWTs, applies to the error code and to transport/decode errors, and no longer leaves the tail of secrets longer than 64 characters behind.
- Look-alike hosts such as `innfactory.ai.evil.com` and `evilinnfactory.ai` are refused (label-boundary match).

- Endpoint host pin: refuses to send the admin key to anything that isn't `https://hub.innfactory.cloud` or `https://*.innfactory.cloud`. Opt-out via `INNFACTORYHUB_INSECURE_ENDPOINT=1` for local dev.
- 4xx/5xx response bodies are truncated to 512 bytes and stripped of `ak_…` shapes before reaching Terraform diagnostics.
- TLS ≥ 1.2 pinned on the HTTP transport.
- Whitespace-only identifiers are rejected at the data-source level instead of producing a confusing 404.
- `terraform import` now sets `plaintext` to null explicitly and emits a warning.

## v0.1.0 — 2026-05-26

Initial release.

**Added**

- Provider `innfactoryhub` with `endpoint` + `admin_key` configuration (env: `INNFACTORYHUB_ENDPOINT`, `INNFACTORYHUB_ADMIN_KEY`).
- Resource `innfactoryhub_api_key` — mint/revoke API keys in innFactory Hub. Plaintext returned exactly once at create time; subsequent reads expose metadata only.
- Data source `innfactoryhub_tenant` — resolve a Hub tenant by GCP project ID, Azure subscription ID, or AWS account ID. Returns full identifier arrays for one-call enumeration.

**Known limitations**

- `terraform import` cannot recover the plaintext (the Hub doesn't store it). Mint a fresh key instead.
- All mutable attributes force replace — the Hub has no in-place permission/name edit endpoint.
- v0.1 has no acceptance test suite against a live Hub. CI runs `go vet` + unit tests only.
