package provider

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// sameIdempotencyKey asserts n calls to methodPath that all carried the same
// UUID v4 Idempotency-Key.
func sameIdempotencyKey(hub *fakeHub, methodPath string, n int) error {
	keys := hub.idempotencyKeysOf(methodPath)
	if len(keys) != n {
		return fmt.Errorf("%d call(s) to %s, want %d", len(keys), methodPath, n)
	}
	for _, k := range keys {
		if k != keys[0] || !uuidV4.MatchString(k) {
			return fmt.Errorf("Idempotency-Key per attempt %q, want one UUID v4", keys)
		}
	}
	return nil
}

const trialLicense = `
resource "innfactoryhub_license" "gw" {
  environment_id  = %q
  product         = "ai-gateway"
  domains         = ["gpt.alpha.example"]
  max_users       = 50
  edition         = "all"
  unbilled_reason = "trial"
}
`

// A lost create response must not duplicate an unbilled licence (SEC-014):
// the provider looks the licence up, retries with the same Idempotency-Key,
// adopts the licence the Hub reports as replayed and rotates its key — the
// key of the lost response is revoked, the new one lands in state.
func TestLicenseCreateLostResponseAdoptsLicense(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	hub.dropResponses(1, "/api/v1/admin/licenses")
	cfg := hub.providerBlock() + fmt.Sprintf(trialLicense, alphaEnvID)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(licAddr, "key_status", "active"),
					func(s *terraform.State) error {
						ids := hub.licenseIDsByStatus("")
						if len(ids) != 1 || ids[0] != stateAttr(s, "id") {
							return fmt.Errorf("licences in the Hub %v, state %s — want exactly the adopted one", ids, stateAttr(s, "id"))
						}
						issued := hub.issuedKeys(ids[0])
						if len(issued) != 2 || stateAttr(s, "license_key") != issued[1] {
							return fmt.Errorf("want the lost key rotated away and the new key in state, issued %d key(s)", len(issued))
						}
						if active := hub.activeKeyIDs(ids[0]); len(active) != 1 || active[0] != stateAttr(s, "key_id") {
							return fmt.Errorf("active keys %v, key_id %s", active, stateAttr(s, "key_id"))
						}
						if err := sameIdempotencyKey(hub, "POST /api/v1/admin/licenses", 2); err != nil {
							return err
						}
						calls := hub.allCalls()
						lookup := slices.Index(calls, "GET /api/v1/admin/licenses")
						retry := slices.Index(calls[lookup+1:], "POST /api/v1/admin/licenses")
						if lookup < 0 || retry < 0 {
							return fmt.Errorf("want a lookup by environment before the retry, calls: %v", calls)
						}
						return nil
					},
				),
			},
			{
				Config:           cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
		},
	})
}

// A 5xx (e.g. the hosting layer timing out) leaves the outcome open too; if
// the Hub did not create the licence, the retry does — exactly once.
func TestLicenseCreateRetriesAfterServerError(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	hub.failTimes(1, http.MethodPost, "/api/v1/admin/licenses", http.StatusBadGateway, "<html>upstream timeout</html>")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{{
			Config: hub.providerBlock() + fmt.Sprintf(trialLicense, alphaEnvID),
			Check: func(s *terraform.State) error {
				if ids := hub.licenseIDsByStatus(""); len(ids) != 1 || ids[0] != stateAttr(s, "id") {
					return fmt.Errorf("licences %v, state %s", ids, stateAttr(s, "id"))
				}
				if keys := hub.issuedKeys(stateAttr(s, "id")); len(keys) != 1 || keys[0] != stateAttr(s, "license_key") {
					return fmt.Errorf("want the key of the create response in state")
				}
				return sameIdempotencyKey(hub, "POST /api/v1/admin/licenses", 2)
			},
		}},
	})
}

// A Hub refusal (4xx) is final: no retry, no lookup.
func TestLicenseCreateDoesNotRetryRefusals(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      hub.providerBlock() + fmt.Sprintf(trialLicense, "env_missing"),
			ExpectError: regexp.MustCompile(`404 environment_not_found`),
		}},
	})
	if got := hub.allCalls(); !slices.Equal(got, []string{"POST /api/v1/admin/licenses"}) {
		t.Fatalf("calls %v, want exactly one POST", got)
	}
}

// Two attempts that both lose their answer end in an error that says the
// outcome is open and names what to check — never in a blind third POST.
func TestLicenseCreateOutcomeUnknownIsReported(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	hub.failTimes(2, http.MethodPost, "/api/v1/admin/licenses", http.StatusGatewayTimeout, "gateway timeout")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      hub.providerBlock() + fmt.Sprintf(trialLicense, alphaEnvID),
			ExpectError: regexp.MustCompile(`(?s)Create licence failed.*504.*outcome.*terraform\s+import`),
		}},
	})
	if n := len(hub.idempotencyKeysOf("POST /api/v1/admin/licenses")); n != 2 {
		t.Fatalf("%d POST(s), want 2", n)
	}
}

// Environments: a lost response is retried with the same Idempotency-Key and
// the Hub's replay is adopted — one environment, no environment_exists.
func TestEnvironmentCreateLostResponseIsReplayed(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)
	hub.dropResponses(1, "/api/v1/admin/environments")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkEnvironmentGone(hub),
		Steps: []resource.TestStep{{
			Config: envConfig(hub, azureTenantLine(), "Alpha AI Gateway", "gpt.alpha.example", "azure"),
			Check: func(s *terraform.State) error {
				ids := hub.environmentIDs()
				if len(ids) != 1 || ids[0] != s.RootModule().Resources[envAddr].Primary.ID {
					return fmt.Errorf("environments %v, want exactly the one in state", ids)
				}
				return sameIdempotencyKey(hub, "POST /api/v1/admin/environments", 2)
			},
		}},
	})
}

// Every create carries its own key.
func TestIdempotencyKeysDifferPerCreate(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	cfg := hub.providerBlock() + fmt.Sprintf(trialLicense, alphaEnvID) +
		fmt.Sprintf(`
resource "innfactoryhub_license" "stg" {
  environment_id  = %q
  product         = "ai-gateway"
  domains         = ["stg.alpha.example"]
  max_users       = 5
  edition         = "all"
  unbilled_reason = "trial"
}
`, alphaStagingEnvID)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{{
			Config: cfg,
			Check: func(*terraform.State) error {
				keys := hub.idempotencyKeysOf("POST /api/v1/admin/licenses")
				if len(keys) != 2 || keys[0] == keys[1] || !uuidV4.MatchString(keys[0]) || !uuidV4.MatchString(keys[1]) {
					return fmt.Errorf("Idempotency-Keys %q, want two different UUID v4", keys)
				}
				return nil
			},
		}},
	})
}
