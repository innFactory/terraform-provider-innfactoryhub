package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// The data source keeps working unchanged against either Hub: same path,
// same query keys, same envelope.
func TestTenantDataSourceLookup(t *testing.T) {
	cases := []struct {
		name  string
		attr  string
		value string
	}{
		{name: "tenant_id", attr: "tenant_id", value: alphaTenantID},
		{name: "gcp_project_id", attr: "gcp_project_id", value: alphaGcpProject},
		{name: "azure_subscription_id", attr: "azure_subscription_id", value: alphaSubscription},
		{name: "azure_tenant_id", attr: "azure_tenant_id", value: alphaAzureTenantID},
		{name: "aws_account_id", attr: "aws_account_id", value: alphaAwsAccount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeHub(t)
			useFakeHubEnv(t)

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				Steps: []resource.TestStep{{
					Config: hub.providerBlock() + fmt.Sprintf(`
data "innfactoryhub_tenant" "self" {
  %s = %q
}
`, tc.attr, tc.value),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.innfactoryhub_tenant.self", "tenant_id", alphaTenantID),
						resource.TestCheckResourceAttr("data.innfactoryhub_tenant.self", "legal_name", "Alpha Demo GmbH"),
						resource.TestCheckResourceAttr("data.innfactoryhub_tenant.self", "azure_tenant_ids.0", alphaAzureTenantID),
						resource.TestCheckResourceAttr("data.innfactoryhub_tenant.self", "gcp_project_ids.0", alphaGcpProject),
					),
				}},
			})
		})
	}
}
