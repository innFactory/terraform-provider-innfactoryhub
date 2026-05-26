---
page_title: "innfactoryhub_tenant Data Source - innfactoryhub"
subcategory: ""
description: |-
  Resolves a Hub tenant by one of its identifiers (tenant_id, GCP project, Azure subscription, Azure tenant, or AWS account).
---

# innfactoryhub_tenant (Data Source)

Resolves a Hub tenant from one of its identifiers. **Exactly one** of `tenant_id`, `gcp_project_id`, `azure_subscription_id`, `azure_tenant_id`, or `aws_account_id` must be set. The returned attributes include ALL identifier arrays so a single lookup gives the caller the tenant's full cross-cloud footprint.

## Example Usage

```terraform
# By GCP project — typical for a GCP-hosted CompanyGPT deploy.
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
  value = data.innfactoryhub_tenant.by_gcp.tenant_id
}

output "all_gcp_projects_in_tenant" {
  value = data.innfactoryhub_tenant.by_gcp.gcp_project_ids
}
```

## Schema

### Optional (set exactly one)

- `tenant_id` (String) Direct fetch by Hub `tnt_…` id.
- `gcp_project_id` (String) GCP project ID.
- `azure_subscription_id` (String) Azure subscription ID (billing scope).
- `azure_tenant_id` (String) Azure (Entra ID) tenant / directory ID.
- `aws_account_id` (String) AWS account ID (12-digit).

### Read-Only

- `id` (String) Mirrors `tenant_id`.
- `tenant_id` (String) Also writable as an input; on read it reflects the resolved tenant.
- `legal_name` (String)
- `gcp_project_ids` (List of String)
- `azure_subscription_ids` (List of String)
- `azure_tenant_ids` (List of String)
- `aws_account_ids` (List of String)
