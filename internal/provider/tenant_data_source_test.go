package provider

import (
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const tenantAddr = "data.innfactoryhub_tenant.self"

// tenantConfig renders the data source with the raw HCL line(s) of its
// inputs, e.g. `companygpt_handle = "alpha"`.
func tenantConfig(providerBlock, inputs string) string {
	return providerBlock + fmt.Sprintf(`
data "innfactoryhub_tenant" "self" {
  %s
}
`, inputs)
}

// The data source keeps working unchanged against either Hub: same path,
// same query keys, same envelope. Each input is sent as the one query key
// of the same name.
func TestTenantDataSourceLookup(t *testing.T) {
	cases := []struct {
		name  string
		attr  string
		value string
	}{
		{name: "tenant_id", attr: "tenant_id", value: alphaTenantID},
		{name: "companygpt_handle", attr: "companygpt_handle", value: alphaHandle},
		{name: "second companygpt_handle", attr: "companygpt_handle", value: alphaStagingHandle},
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
					Config: tenantConfig(hub.providerBlock(), fmt.Sprintf("%s = %q", tc.attr, tc.value)),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(tenantAddr, "tenant_id", alphaTenantID),
						resource.TestCheckResourceAttr(tenantAddr, "legal_name", "Alpha Demo GmbH"),
						resource.TestCheckResourceAttr(tenantAddr, "azure_tenant_ids.0", alphaAzureTenantID),
						resource.TestCheckResourceAttr(tenantAddr, "gcp_project_ids.0", alphaGcpProject),
						func(*terraform.State) error {
							want := url.Values{tc.attr: {tc.value}}
							if got := hub.lastLookupQuery(); !reflect.DeepEqual(got, want) {
								return fmt.Errorf("lookup query %v, want %v", got, want)
							}
							return nil
						},
					),
					ConfigStateChecks: []statecheck.StateCheck{
						statecheck.ExpectKnownValue(tenantAddr, tfjsonpath.New("companygpt_handles"), knownvalue.ListExact([]knownvalue.Check{
							knownvalue.StringExact(alphaHandle), knownvalue.StringExact(alphaStagingHandle),
						})),
					},
				}},
			})
		})
	}
}

// companygpt_handles is an empty list, never null, for a tenant without
// handles and for a Hub that does not know them (innfactoryHUB) — so
// length() and contains() work on it.
func TestTenantDataSourceWithoutHandles(t *testing.T) {
	cases := []struct {
		name            string
		tenantID        string
		hubOmitsHandles bool
	}{
		{name: "tenant without handles", tenantID: "tnt_gamma"},
		{name: "hub without handles", tenantID: alphaTenantID, hubOmitsHandles: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeHub(t)
			useFakeHubEnv(t)
			if tc.hubOmitsHandles {
				hub.omitHandles()
			}

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				Steps: []resource.TestStep{{
					Config: tenantConfig(hub.providerBlock(), fmt.Sprintf("tenant_id = %q", tc.tenantID)),
					ConfigStateChecks: []statecheck.StateCheck{
						statecheck.ExpectKnownValue(tenantAddr, tfjsonpath.New("companygpt_handles"), knownvalue.ListSizeExact(0)),
					},
				}},
			})
		})
	}
}

// A handle no customer carries is the Hub's 404 tenant_not_found.
func TestTenantDataSourceUnknownHandle(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      tenantConfig(hub.providerBlock(), handleLine("nobody")),
			ExpectError: regexp.MustCompile(`(?s)Tenant lookup failed.*404 tenant_not_found`),
		}},
	})
}

// A malformed handle or a second input fails at plan time, before any Hub
// call. TestCompanygptHandleFormat covers the format itself.
func TestTenantDataSourceConfigValidation(t *testing.T) {
	cases := []struct {
		name    string
		inputs  string
		wantErr string
	}{
		{name: "upper case handle", inputs: handleLine("Alpha"), wantErr: `(?s)companygpt_handle.*must be a CompanyGPT handle`},
		// "" is not "unset" as on the other inputs: it is no handle.
		{name: "empty handle", inputs: handleLine(""), wantErr: `(?s)companygpt_handle.*must be a CompanyGPT handle`},
		{
			name:    "handle and cloud identifier",
			inputs:  handleLine(alphaHandle) + "\n  " + azureTenantLine(),
			wantErr: `(?s)Exactly one identifier required.*companygpt_handle.*got 2`,
		},
		{
			name:    "handle and tenant_id",
			inputs:  handleLine(alphaHandle) + fmt.Sprintf("\n  tenant_id = %q", alphaTenantID),
			wantErr: `(?s)Exactly one identifier required.*got 2`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeHub(t)
			useFakeHubEnv(t)

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				Steps: []resource.TestStep{{
					Config:      tenantConfig(hub.providerBlock(), tc.inputs),
					PlanOnly:    true,
					ExpectError: regexp.MustCompile(tc.wantErr),
				}},
			})
			if n := hub.callCount(); n != 0 {
				t.Fatalf("invalid config reached the Hub (%d request(s))", n)
			}
		})
	}
}
