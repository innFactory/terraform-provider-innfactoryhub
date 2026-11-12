package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func apiKeyConfig(hub *fakeHub, permissions string) string {
	return hub.providerBlock() + fmt.Sprintf(`
resource "innfactoryhub_api_key" "k" {
  name        = "Gateway Rollout"
  scope       = "system"
  permissions = %s
}
`, permissions)
}

// The Hub has no endpoint to edit permissions, so a change must replace the
// key instead of ending in the provider's "Update not supported" error.
func TestAPIKeyPermissionChangeForcesReplacement(t *testing.T) {
	hub := newFakeHub(t)
	hub.allowSystemKeyMinting() // innfactoryHUB; the AI Hub refuses system keys from API keys
	useFakeHubEnv(t)

	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: apiKeyConfig(hub, `["read"]`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("innfactoryhub_api_key.k", "permissions.#", "1"),
					resource.TestCheckResourceAttrSet("innfactoryhub_api_key.k", "plaintext"),
					captureAttr("innfactoryhub_api_key.k", "id", &firstID),
				),
			},
			{
				Config: apiKeyConfig(hub, `["read", "write"]`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("innfactoryhub_api_key.k", plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("innfactoryhub_api_key.k", "permissions.#", "2"),
					func(s *terraform.State) error {
						ids := hub.apiKeyIDs()
						if len(ids) != 1 || ids[0] == firstID {
							return fmt.Errorf("expected exactly one new active key replacing %s, got %v", firstID, ids)
						}
						return nil
					},
				),
			},
		},
	})
}

// captureAttr stores a state attribute for assertions in later steps.
func captureAttr(name, attr string, dst *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[name]
		if !ok {
			return fmt.Errorf("resource %s not in state", name)
		}
		*dst = rs.Primary.Attributes[attr]
		if *dst == "" {
			return fmt.Errorf("%s.%s is empty", name, attr)
		}
		return nil
	}
}

// Organization keys keep their create/read/import/revoke behaviour.
func TestAPIKeyOrganizationLifecycle(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)
	cfg := hub.providerBlock() + fmt.Sprintf(`
resource "innfactoryhub_api_key" "k" {
  name        = "CompanyGPT Alpha"
  scope       = "organization"
  tenant_id   = %q
  permissions = ["read"]
}
`, alphaTenantID)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy: func(*terraform.State) error {
			if ids := hub.apiKeyIDs(); len(ids) != 0 {
				return fmt.Errorf("keys still active after destroy: %v", ids)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("innfactoryhub_api_key.k", "tenant_id", alphaTenantID),
					resource.TestCheckResourceAttrSet("innfactoryhub_api_key.k", "plaintext"),
				),
			},
			{
				Config:                  cfg,
				ResourceName:            "innfactoryhub_api_key.k",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"plaintext"},
			},
		},
	})
}
