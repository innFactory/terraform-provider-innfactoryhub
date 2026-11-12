---
page_title: "innfactoryhub_tenant Data Source - innfactoryhub"
subcategory: ""
description: |-
  Resolves a Hub tenant by one of its identifiers (tenant_id, CompanyGPT handle, GCP project, Azure subscription, Azure tenant, or AWS account).
---

# innfactoryhub_tenant (Data Source)

Resolves a Hub tenant from one of its identifiers. **Exactly one** of `tenant_id`, `companygpt_handle`, `gcp_project_id`, `azure_subscription_id`, `azure_tenant_id`, or `aws_account_id` must be set. The returned attributes include ALL identifier arrays and the tenant's CompanyGPT handles, so a single lookup gives the caller the tenant's full cross-cloud footprint.

For CompanyGPT rollouts on the innFactory AI Hub, look the customer up by its **CompanyGPT handle** — the deployment name such as `innfactory`, `lmu` or `ksob`. The Hub keeps each handle unique across customers, and a CompanyGPT stack always knows its own name, whereas a cloud identifier (Azure tenant, subscription, project) can be shared or change when the stack moves. Handles exist on the AI Hub only; the innfactoryHUB on `innfactory.cloud` does not know them.

## Example Usage

```terraform
# By CompanyGPT handle — recommended for CompanyGPT rollouts. Handles live
# in the innFactory AI Hub only.
provider "innfactoryhub" {
  alias    = "aihub"
  endpoint = "https://hub.innfactory.ai"
}

data "innfactoryhub_tenant" "by_handle" {
  provider          = innfactoryhub.aihub
  companygpt_handle = "innfactory"
}

# By GCP project.
data "innfactoryhub_tenant" "by_gcp" {
  gcp_project_id = "companygpt-acme-prod"
}

# By Azure Entra ID directory (different from a subscription).
data "innfactoryhub_tenant" "by_azure_tenant" {
  azure_tenant_id = "00000000-0000-0000-0000-000000000000"
}

# By known Hub tenant_id — direct fetch, no scan.
data "innfactoryhub_tenant" "by_hub_id" {
  tenant_id = "tnt_abc12345"
}

output "tenant_id" {
  value = data.innfactoryhub_tenant.by_handle.tenant_id
}

output "all_gcp_projects_in_tenant" {
  value = data.innfactoryhub_tenant.by_gcp.gcp_project_ids
}
```

## Schema

### Optional (set exactly one)

- `tenant_id` (String) Direct fetch by Hub `tnt_…` id.
- `companygpt_handle` (String) CompanyGPT handle (deployment name), e.g. `innfactory` — AI Hub only. 1–40 lower-case letters, digits and hyphens, starting and ending with a letter or digit (`^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$`); anything else — upper case, surrounding spaces, underscores — fails at plan time.
- `gcp_project_id` (String) GCP project ID.
- `azure_subscription_id` (String) Azure subscription ID (billing scope).
- `azure_tenant_id` (String) Azure (Entra ID) tenant / directory ID.
- `aws_account_id` (String) AWS account ID (12-digit).

### Read-Only

- `id` (String) Mirrors `tenant_id`.
- `tenant_id` (String) Also writable as an input; on read it reflects the resolved tenant.
- `legal_name` (String)
- `companygpt_handles` (List of String) All CompanyGPT handles of the tenant. An empty list when it has none, and always empty on the innfactoryHUB.
- `gcp_project_ids` (List of String)
- `azure_subscription_ids` (List of String)
- `azure_tenant_ids` (List of String)
- `aws_account_ids` (List of String)

## Errors

| Hub answer | Meaning | Fix |
|---|---|---|
| `404 tenant_not_found` | No customer carries the identifier or handle | Add it to the customer in the Hub, or look up by `tenant_id` |
| `409 ambiguous_lookup` | Several customers carry the cloud identifier (never a handle — handles are unique) | Clean up the identifiers in the Hub, or look up by `companygpt_handle` or `tenant_id` |

Setting none or more than one input fails before the Hub is called (`Exactly one identifier required`).
