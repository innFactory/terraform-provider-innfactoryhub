# Changelog

## Unreleased

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
