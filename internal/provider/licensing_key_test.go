package provider

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// companyGPTConfig is the CompanyGPT rollout against the AI Hub: environment,
// gateway licence and the companyADMIN maintenance key, applied with a
// `licensing` key.
func companyGPTConfig(hub *fakeHub, key string, maxUsers int, expiresAt string) string {
	return hub.providerBlockWithKey(key) + fmt.Sprintf(`
resource "innfactoryhub_environment" "gw" {
  azure_tenant_id = %q
  name            = "Alpha AI Gateway"
  domain          = "gpt.alpha.example"
  cloud           = "azure"
}

resource "innfactoryhub_license" "gw" {
  environment_id  = innfactoryhub_environment.gw.id
  product         = "ai-gateway"
  domains         = [innfactoryhub_environment.gw.domain]
  max_users       = %d
  edition         = "all"
  unbilled_reason = "trial"
}

resource "innfactoryhub_api_key" "k" {
  name        = "companyADMIN Alpha"
  scope       = "organization"
  tenant_id   = innfactoryhub_environment.gw.tenant_id
  permissions = ["read_maintenance"]
  expires_at  = %q
}
`, alphaAzureTenantID, maxUsers, expiresAt)
}

// A licensing key ["read", "licensing"] applies the whole CompanyGPT rollout;
// a read_licensing key plans it (e.g. in pull requests) but cannot apply.
func TestLicensingKeyAppliesAndReadLicensingKeyPlans(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)
	expiresAt := time.Now().AddDate(1, 0, 0).UTC().Truncate(time.Second).Format(time.RFC3339)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{
				Config: companyGPTConfig(hub, testLicensingKey, 50, expiresAt),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(licAddr, "license_key"),
					resource.TestCheckResourceAttrSet(keyAddr, "plaintext"),
					resource.TestCheckResourceAttr(keyAddr, "status", "active"),
					func(*terraform.State) error { hub.resetCalls(); return nil },
				),
			},
			{
				Config:   companyGPTConfig(hub, testReadLicensingKey, 50, expiresAt),
				PlanOnly: true,
				Check: func(*terraform.State) error {
					if w := hub.writeCalls(); len(w) != 0 {
						return fmt.Errorf("plan wrote: %v", w)
					}
					return nil
				},
			},
			{
				Config:      companyGPTConfig(hub, testReadLicensingKey, 80, expiresAt),
				ExpectError: regexp.MustCompile(`(?s)403 forbidden_read_only_key.*\["read",\s*"licensing"\]`),
			},
			{
				Config: companyGPTConfig(hub, testLicensingKey, 80, expiresAt),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(licAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.TestCheckResourceAttr(licAddr, "max_users", "80"),
			},
		},
	})
}

// The Hub's refusals for API keys come with a hint on what to do.
func TestAPIKeyRefusalsCarryHints(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		body    string
		wantErr string
	}{
		{name: "no API key mints a system key", key: testAdminKey, body: `scope = "system"
  permissions = ["read", "licensing"]`, wantErr: `(?s)403 forbidden_key_minting.*portal`},
		{name: "licensing key mints only maintenance keys", key: testLicensingKey, body: `scope = "organization"
  tenant_id = "tnt_alpha"
  permissions = ["read"]`, wantErr: `(?s)403 forbidden_key_scope.*read_maintenance`},
		{name: "licensing key mints maintenance keys only with expiry", key: testLicensingKey, body: `scope = "organization"
  tenant_id = "tnt_alpha"
  permissions = ["read_maintenance"]`, wantErr: `(?s)403 forbidden_key_scope.*with\s+an\s+expires_at`},
		{name: "read_licensing key cannot write", key: testReadLicensingKey, body: `scope = "organization"
  tenant_id = "tnt_alpha"
  permissions = ["read_maintenance"]`, wantErr: `(?s)403 forbidden_read_only_key.*plans`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeHub(t)
			useFakeHubEnv(t)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				Steps: []resource.TestStep{{
					Config: hub.providerBlockWithKey(tc.key) + `
resource "innfactoryhub_api_key" "k" {
  name = "refused"
  ` + tc.body + `
}
`,
					ExpectError: regexp.MustCompile(tc.wantErr),
				}},
			})
			if ids := hub.apiKeyIDs(); len(ids) != 0 {
				t.Fatalf("a refused key was created: %v", ids)
			}
		})
	}
}

// Anything outside the licensing scope fails with a hint, not a bare 403.
func TestLicensingKeyOutOfScopeHint(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)
	hub.failPath("", "/api/v1/admin/tenants/lookup", 403, `{"error":{"code":"forbidden_key_scope","message":"API key scope does not cover this request"}}`)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{{
			Config: hub.providerBlockWithKey(testLicensingKey) + `
data "innfactoryhub_tenant" "self" {
  tenant_id = "tnt_alpha"
}
`,
			ExpectError: regexp.MustCompile(`(?s)403 forbidden_key_scope.*licensing`),
		}},
	})
	if strings.Contains(fmt.Sprint(hub.allCalls()), "api-keys") {
		t.Fatal("unexpected API key calls")
	}
}
