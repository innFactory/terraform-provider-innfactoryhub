# Changelog

## Unreleased

**Added**

- Data source `innfactoryhub_tenant` accepts two more inputs: `tenant_id` (direct fetch, no scan) and `azure_tenant_id` (Entra ID directory — distinct from a subscription).
- `azure_tenant_ids` returned as a computed list attribute.

**Security**

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
