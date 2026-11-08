# Changelog

## v0.2.0 — unreleased

Prepared for release; the tag `v0.2.0` is pushed manually after approval (GoReleaser publishes to the Registry). Needs the AI Hub with key permissions `licensing` / `read_licensing` / `read_maintenance`, `Idempotency-Key` and `X-Audit-Context`.

**Upgrade notes**

- The endpoint must be exactly `https://hub.innfactory.ai` or `https://hub.innfactory.cloud`. Other names in these domains need `INNFACTORYHUB_INSECURE_ENDPOINT=1`, and plain http now only works to localhost/loopback even then.
- On the AI Hub, run applies with a system key `["read", "licensing"]` (at most 90 days) and plans with `["read_licensing"]`. API keys can no longer create system keys there (`403 forbidden_key_minting`).
- A licence ended in the Hub, a licence key rotated there and an API key revoked, deleted or expired there now make plans fail until `rotation_trigger` changes — they were silently re-issued before.
- `innfactoryhub_license.rotation_trigger` rotates the key in place instead of replacing the licence.

**Added**

- Resource `innfactoryhub_license` (innFactory AI Hub): issues a licence for an environment via `POST /api/v1/admin/licenses` with `environment_id`, `product`, `domains`, `max_users` or `unlimited`, `max_instances` (default 5), `edition`, `features` (default: all of the edition), `entitlements`, and exactly one of `contract_position_id` or `unbilled_reason` (+ optional `unbilled_max_until`; without it the Hub's default per reason applies) — both "exactly one" rules are checked at plan time.
  - `license_key` (sensitive) is set from the create or rotation response and kept unchanged through updates; Read never touches it. After import it is empty, with a warning.
  - Parameter and billing-link changes update in place with one `PATCH` carrying only the changed fields; re-applying unchanged config plans nothing. `environment_id` and `product` force a new licence.
  - Key rotation via `rotation_trigger` **in place** (SEC-011): any change calls `POST /admin/licenses/:id/keys/rotate`; same licence, instances and contract position; the previous key is revoked at once. A failed rotation keeps the previous key and trigger, so the next apply retries it.
  - Computed `key_id` and `key_status`: Read checks the key in state against the licence's keys. A key revoked or replaced in the Hub (e.g. rotated in the portal) shows `key_status = "revoked"`, and plans fail until `rotation_trigger` changes.
  - `status` includes `ended`. A licence ended in the Hub stays in state and is not re-issued (SEC-006): plans fail with "Licence ended in the AI Hub – change rotation_trigger to issue a new one"; a `rotation_trigger` change issues a new licence. Suspended licences are never resumed. Destroy ends the licence (`DELETE`, status `ended`, contract position freed); `404` is tolerated.
  - Hub rejections (`position_already_linked`, `edition_unknown`, `unbilled_max_exceeded`, …) come with a remediation hint. No licence file or token is ever stored in state.
- Resource `innfactoryhub_environment` (innFactory AI Hub): registers a customer installation via `POST /api/v1/admin/environments`, bound by exactly one of `tenant_id`, `azure_tenant_id`, `azure_tenant_domain`, `azure_subscription_id`, `gcp_project_id`, `aws_account_id`. `name` and `domain` update in place (`PATCH`); `cloud` and the customer binding force replacement; import by `environment_id`. Hub errors `tenant_not_found`, `ambiguous_lookup` and `environment_exists` come with a remediation hint.
- Idempotent creates (SEC-014): licence and environment creates carry a UUID v4 `Idempotency-Key`. After a lost answer (no or unreadable response, `5xx`) the provider looks up the running licences of the environment and product and retries once with the same key; a replayed environment is adopted, a replayed licence (`409 idempotent_replay`) is adopted and its key rotated so the key of the lost answer is revoked. Two unanswered attempts fail with the running licences listed — never a duplicate unbilled licence.
- `innfactoryhub_api_key`:
  - `expires_at` (optional, RFC 3339, forces a new key): sent in UTC, read back without a diff for the same instant; computed from the Hub when not set.
  - `rotation_trigger`: any change mints a new key.
  - `status` (`active`, `revoked`, `expired`, `deleted`). A key revoked, deleted or expired in the Hub stays in state and is not minted again (SEC-006): plans fail with "API key revoked in the Hub – change rotation_trigger to mint a new one".
  - Permissions of the AI Hub, validated at plan time with the scope and `tenant_id` rules: `licensing` (system keys, only as `["read", "licensing"]`, never with `write`), `read_licensing` (system keys, alone) and `read_maintenance` (organization keys); a warning when `read` lifts the `read_maintenance` restriction. A `licensing` admin key creates only the companyADMIN key (`["read_maintenance"]` with `expires_at`).
- Provider attribute `audit_context` / env `INNFACTORYHUB_AUDIT_CONTEXT` (SEC-013): sent as `X-Audit-Context` on every request; printable ASCII on one line, secrets redacted, at most 256 characters.
- Hints for the AI Hub's key refusals `forbidden_key_scope`, `forbidden_key_minting`, `forbidden_read_only_key` and for `invalid_api_key`, on every resource and the data source. The `admin_key` description and the missing-key error recommend `["read", "licensing"]` to apply and `["read_licensing"]` for plans.
- Hub error `details` (e.g. available editions, the licence holding a position) are shown in diagnostics, capped and redacted like the message.
- Data source `innfactoryhub_tenant` accepts two more inputs: `tenant_id` (direct fetch, no scan) and `azure_tenant_id` (Entra ID directory — distinct from a subscription); `azure_tenant_ids` returned as a computed list.
- Test suite: `resource.UnitTest` against an `httptest` fake Hub that mirrors the AI Hub (key permissions, rotation, idempotency and lost answers, out-of-band revocations).
- CI: `go mod verify`, gofmt, `go test -race`, govulncheck v1.9.0, actionlint. Dependabot for Go modules and GitHub Actions.

**Changed**

- A missing admin key no longer fails provider configuration; it fails the first operation that calls the Hub (`missing admin key`). Allows count-gated secret reads in callers such as CompanyGPT.
- A trailing slash on `endpoint` is tolerated.
- Go 1.26.10 toolchain (`go 1.26.0`); terraform-plugin-framework v1.19.0, terraform-plugin-go v0.31.0, terraform-plugin-testing v1.16.0, terraform-plugin-framework-validators v0.19.0, terraform-plugin-log v0.11.0, golang.org/x/net v0.62.0, google.golang.org/grpc v1.85.0. Same 13 platforms as v0.1.0; `windows/arm` is excluded explicitly.
- Docs: CompanyGPT usage on the AI Hub (apply/plan keys, companyADMIN key with `time_rotating`, audit context in CI, revoking in the Hub), import examples; examples pin `~> 0.2.0`.

**Fixed**

- `innfactoryhub_api_key`: changing `permissions` now forces replacement instead of failing with "Update not supported".
- Data source: an unexpected provider data type is a diagnostic instead of a panic.

**Security**

- SEC-005 — release pipeline: every action pinned to a commit SHA, GoReleaser pinned to v2.19.0, release job in the protected `release` environment with minimal permissions, no Go cache, no persisted checkout credentials, `go mod verify` + `git diff --exit-code go.mod go.sum` instead of `go mod tidy`, build provenance attested (`actions/attest-build-provenance`). Required reviewers and the `v*` tag ruleset are repository settings (README, "Releases").
- SEC-006 — revocations in the Hub are no longer undone by the next apply (licences ended, licence keys rotated, API keys revoked/deleted/expired).
- SEC-009 — the admin key goes only to exactly `hub.innfactory.ai` and `hub.innfactory.cloud` (default port, bare origin); with `INNFACTORYHUB_INSECURE_ENDPOINT=1` other https hosts, plain http only to loopback.
- SEC-010 — the HTTP client follows no redirects; a 3xx is an error.
- SEC-011 — key rotation no longer ends the licence (no gap without licence, no lost instance history or position link); keys rotated outside Terraform are detected.
- SEC-012 — Go 1.26.10 (GO-2026-6809, crypto/x509) and dependencies without known vulnerabilities (govulncheck).
- SEC-013 — `X-Audit-Context` attributes changes in the Hub's audit log to the CI run.
- SEC-014 — a lost create answer no longer duplicates an unbilled licence.
- Error redaction covers licence keys (`lk_…`) and JWTs, applies to the error code and to transport/decode errors, and leaves no tail of secrets longer than 64 characters behind.
- 4xx/5xx response bodies are truncated to 512 bytes; TLS ≥ 1.2.
- Whitespace-only identifiers are rejected at the data-source level instead of producing a confusing 404.
- `terraform import` of an API key sets `plaintext` to null explicitly and emits a warning.

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
