package provider

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
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
						// lookup → POST (answer lost) → lookup → POST (replay).
						calls := hub.allCalls()
						first := slices.Index(calls, "POST /api/v1/admin/licenses")
						if first < 0 || slices.Index(calls[:first], "GET /api/v1/admin/licenses") < 0 {
							return fmt.Errorf("want a lookup by environment before the first POST, calls: %v", calls)
						}
						lookup := slices.Index(calls[first+1:], "GET /api/v1/admin/licenses")
						if lookup < 0 || slices.Index(calls[first+1+lookup:], "POST /api/v1/admin/licenses") < 0 {
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
	if got := hub.allCalls(); !slices.Equal(got, []string{"GET /api/v1/admin/licenses", "POST /api/v1/admin/licenses"}) {
		t.Fatalf("calls %v, want the lookup and exactly one POST", got)
	}
}

// Two attempts that both lose their answer end in an error that says the
// outcome is open and names what to check — never in a blind third POST.
func TestLicenseCreateOutcomeUnknownIsReported(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	hub.failTimes(2, http.MethodPost, "/api/v1/admin/licenses", http.StatusGatewayTimeout, "gateway timeout")
	cfg := hub.providerBlock() + fmt.Sprintf(trialLicense, alphaEnvID)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{
				Config:      cfg,
				ExpectError: regexp.MustCompile(`(?s)Create licence failed.*504.*outcome.*terraform\s+import`),
			},
			{
				// Nothing partial was stored: the next apply plans a plain create.
				PreConfig: func() {
					if n := len(hub.idempotencyKeysOf("POST /api/v1/admin/licenses")); n != 2 {
						t.Errorf("%d POST(s), want 2", n)
					}
				},
				Config: cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(licAddr, plancheck.ResourceActionCreate),
				}},
			},
		},
	})
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

// A replay is only possible after a retry with the same key. A 409
// idempotent_replay to the first attempt is not trusted: nothing is adopted,
// no key of the named licence is rotated (P-1).
func TestLicenseCreateIgnoresReplayWithoutRetry(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	foreign := seedBetaLicense(hub)
	hub.failTimes(1, http.MethodPost, "/api/v1/admin/licenses", http.StatusConflict,
		`{"error":{"code":"idempotent_replay","message":"replay","details":{"license_id":"`+foreign+`"}}}`)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      hub.providerBlock() + fmt.Sprintf(trialLicense, alphaEnvID),
			ExpectError: regexp.MustCompile(`(?s)Create licence failed.*409 idempotent_replay.*first\s+attempt`),
		}},
	})
	checkForeignUntouched(t, hub, foreign)
}

// A replay that names a licence of another environment or product, or an
// ended one, is refused before any rotation (P-1): a broken or hostile Hub
// must not make Terraform revoke someone else's key.
func TestLicenseCreateRefusesForeignReplay(t *testing.T) {
	cases := []struct {
		name    string
		replay  func(hub *fakeHub) string
		wantErr string
	}{
		{name: "other environment", replay: seedBetaLicense, wantErr: `env_beta_gw`},
		{name: "other product", replay: func(h *fakeHub) string {
			return seedLicenseIn(h, "lic_prices000001", alphaEnvID, "ai-prices", "active")
		}, wantErr: `ai-prices`},
		{name: "ended licence", replay: func(h *fakeHub) string {
			return seedLicenseIn(h, "lic_ended0000001", alphaEnvID, "ai-gateway", "ended")
		}, wantErr: `ended`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newLicenseHub(t)
			useFakeHubEnv(t)
			foreign := tc.replay(hub)
			hub.tamperReplay(foreign)
			hub.dropResponses(1, "/api/v1/admin/licenses")

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				Steps: []resource.TestStep{{
					Config:      hub.providerBlock() + fmt.Sprintf(trialLicense, alphaEnvID),
					ExpectError: regexp.MustCompile(`(?s)Create licence failed.*` + foreign + `.*` + tc.wantErr + `.*not\s+adopt`),
				}},
			})
			checkForeignUntouched(t, hub, foreign)
		})
	}
}

// A failed rotation while adopting is an error, and nothing lands in state;
// the next apply refuses to issue a second licence while the adopted one
// runs and says how to take it over (P-4).
func TestLicenseAdoptionWithoutKeyIsAnError(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	hub.dropResponses(1, "/api/v1/admin/licenses")
	hub.failPath(http.MethodPost, "/keys/rotate", http.StatusServiceUnavailable, `{"error":{"code":"signing_unavailable","message":"later"}}`)
	cfg := hub.providerBlock() + fmt.Sprintf(trialLicense, alphaEnvID)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config:      cfg,
				ExpectError: regexp.MustCompile(`(?s)Create licence failed.*rotating.*503\s+signing_unavailable.*terraform\s+import`),
			},
			{
				PreConfig:   func() { hub.failPath("", "", 0, "") },
				Config:      cfg,
				ExpectError: regexp.MustCompile(`(?s)Licence already running.*lic_\d+.*terraform\s+import`),
			},
		},
	})
	if ids := hub.licenseIDsByStatus(""); len(ids) != 1 {
		t.Fatalf("licences %v, want exactly one", ids)
	}
}

// Across applies, a running licence for the environment and product stops
// the create before the Hub: a create whose answer was lost in an earlier
// apply is never issued a second time (P-4).
func TestLicenseCreateRefusesWhenOneIsRunning(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	running := seedLicenseIn(hub, "lic_running00001", alphaEnvID, "ai-gateway", "active")
	seedLicenseIn(hub, "lic_otherprod001", alphaEnvID, "ai-prices", "active")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      hub.providerBlock() + fmt.Sprintf(trialLicense, alphaEnvID),
			ExpectError: regexp.MustCompile(`(?s)Licence already running.*` + running + `.*terraform\s+import.*rotation_trigger`),
		}},
	})
	if n := len(hub.idempotencyKeysOf("POST /api/v1/admin/licenses")); n != 0 {
		t.Fatalf("%d POST(s), want none", n)
	}
}

// seedBetaLicense seeds a running licence of another customer.
func seedBetaLicense(h *fakeHub) string {
	return seedLicenseIn(h, "lic_betaforeign1", betaEnvID, "ai-gateway", "active")
}

// seedLicenseIn seeds a licence with one key, as if issued in the portal.
func seedLicenseIn(h *fakeHub, id, envID, product, status string) string {
	tenant := alphaTenantID
	if envID == betaEnvID {
		tenant = betaTenantID
	}
	five := int64(5)
	h.seedLicense(fakeLicense{
		LicenseID: id, TenantID: tenant, EnvironmentID: envID, Product: product, Status: status, Revision: 1,
		Unbilled: &fakeUnbilled{Reason: "trial", MaxUntil: fakeToday().AddDate(0, 0, 30).Format(isoDay)},
		Params:   fakeLicenseParams{Domains: []string{"gpt.example.com"}, MaxUsers: &five, MaxInstances: 5, EditionID: "all"},
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mintLicenseKeyLocked(id)
	return id
}

// checkForeignUntouched asserts no key of the licence was rotated.
func checkForeignUntouched(t *testing.T, hub *fakeHub, id string) {
	t.Helper()
	if keys := hub.issuedKeys(id); len(keys) != 1 {
		t.Fatalf("licence %s has %d keys — its key was rotated", id, len(keys))
	}
	for _, c := range hub.writeCalls() {
		if strings.Contains(c, "/keys/rotate") {
			t.Fatalf("a rotation was sent: %v", hub.writeCalls())
		}
	}
}
