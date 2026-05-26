---
page_title: "innfactoryhub_tenant Data Source - innfactoryhub"
subcategory: ""
description: |-
  Resolves a Hub tenant from a cloud handle (GCP project, Azure subscription, or AWS account).
---

# innfactoryhub_tenant (Data Source)

Resolves a Hub tenant from one of its cloud-handle identifiers. Exactly one of `gcp_project_id`, `azure_subscription_id`, or `aws_account_id` must be set. The returned attributes include ALL identifier arrays so a single lookup gives the caller the tenant's full cross-cloud footprint.

## Example Usage

```terraform
data "innfactoryhub_tenant" "self" {
  gcp_project_id = "companygpt-acme-prod"
}

output "tenant_id" {
  value = data.innfactoryhub_tenant.self.tenant_id
}

output "all_gcp_projects_in_tenant" {
  value = data.innfactoryhub_tenant.self.gcp_project_ids
}
```

## Schema

### Optional

- `gcp_project_id` (String)
- `azure_subscription_id` (String)
- `aws_account_id` (String) (12-digit AWS account ID)

### Read-Only

- `id` (String) Mirrors `tenant_id`.
- `tenant_id` (String)
- `legal_name` (String)
- `gcp_project_ids` (List of String)
- `azure_subscription_ids` (List of String)
- `aws_account_ids` (List of String)
