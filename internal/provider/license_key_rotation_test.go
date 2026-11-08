package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// withTrigger is gatewayLicense with rotation_trigger set ("" removes it).
func withTrigger(hub *fakeHub, v string, extra licenseAttrs) string {
	trigger := ""
	if v != "" {
		trigger = fmt.Sprintf("%q", v)
	}
	return licenseConfig(hub, gatewayLicense().with(extra).with(licenseAttrs{"rotation_trigger": trigger}))
}

// expectRotationInPlace asserts an in-place update that keeps the licence
// and plans a new key.
func expectRotationInPlace() []plancheck.PlanCheck {
	return []plancheck.PlanCheck{
		plancheck.ExpectResourceAction(licAddr, plancheck.ResourceActionUpdate),
		plancheck.ExpectKnownValue(licAddr, tfjsonpath.New("id"), knownvalue.NotNull()),
		plancheck.ExpectUnknownValue(licAddr, tfjsonpath.New("license_key")),
		plancheck.ExpectUnknownValue(licAddr, tfjsonpath.New("key_id")),
	}
}

// checkRotatedInPlace asserts the same licence with a new key: the key in
// state is the one the Hub issued last and the only active one, the previous
// key is revoked, and the only writes were the rotation — after one PATCH if
// patched.
func checkRotatedInPlace(hub *fakeHub, id, key *string, patched bool) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		wantWrites := []string{rotateCall(*id)}
		if patched {
			wantWrites = append([]string{"PATCH " + licensePath(*id)}, wantWrites...)
		}
		if got := stateAttr(s, "id"); got != *id {
			return fmt.Errorf("licence id changed from %s to %s — rotation must not replace the licence", *id, got)
		}
		newKey := stateAttr(s, "license_key")
		if newKey == "" || newKey == *key {
			return fmt.Errorf("license_key not rotated (still %q)", newKey)
		}
		issued := hub.issuedKeys(*id)
		if issued[len(issued)-1] != newKey {
			return fmt.Errorf("state key is not the key the Hub issued last")
		}
		active := hub.activeKeyIDs(*id)
		if len(active) != 1 || active[0] != stateAttr(s, "key_id") {
			return fmt.Errorf("Hub active keys %v, key_id in state %q", active, stateAttr(s, "key_id"))
		}
		if got := stateAttr(s, "key_status"); got != "active" {
			return fmt.Errorf("key_status = %q, want active", got)
		}
		if l, _ := hub.license(*id); l.Status != "active" {
			return fmt.Errorf("licence status %q after rotation", l.Status)
		}
		if w := hub.writeCalls(); !slices.Equal(w, wantWrites) {
			return fmt.Errorf("writes\n got: %v\nwant: %v", w, wantWrites)
		}
		*key = newKey
		hub.resetCalls()
		return nil
	}
}

func rotateCall(id string) string { return "POST " + licensePath(id) + "/keys/rotate" }

// expectPriorState checks the refreshed state the plan was made from: the
// given string attributes, plus an optional check on all values.
func expectPriorState(addr string, want map[string]string, more func(map[string]any) error) plancheck.PlanCheck {
	return priorStateCheck{addr: addr, want: want, more: more}
}

type priorStateCheck struct {
	addr string
	want map[string]string
	more func(map[string]any) error
}

func (c priorStateCheck) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	if req.Plan.PriorState == nil || req.Plan.PriorState.Values == nil {
		resp.Error = fmt.Errorf("plan has no prior state")
		return
	}
	for _, r := range req.Plan.PriorState.Values.RootModule.Resources {
		if r.Address != c.addr {
			continue
		}
		for k, v := range c.want {
			if got := r.AttributeValues[k]; got != v {
				resp.Error = fmt.Errorf("prior state %s.%s = %v, want %q", c.addr, k, got, v)
				return
			}
		}
		if c.more != nil {
			resp.Error = c.more(r.AttributeValues)
		}
		return
	}
	resp.Error = fmt.Errorf("%s not in prior state", c.addr)
}

// Changing rotation_trigger rotates the key of the same licence in place
// (SEC-011): no ended licence, no lost instances or position link, and the
// new key lands in state. Setting, changing and removing the trigger all
// rotate; keeping it unchanged keeps the key.
func TestLicenseRotationTriggerRotatesKeyInPlace(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)

	var id, key string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{
				Config: licenseConfig(hub, gatewayLicense()),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(licAddr, "id", &id),
					captureAttr(licAddr, "license_key", &key),
					resource.TestCheckResourceAttr(licAddr, "key_status", "active"),
					func(s *terraform.State) error {
						if active := hub.activeKeyIDs(id); len(active) != 1 || active[0] != stateAttr(s, "key_id") {
							return fmt.Errorf("key_id %q, Hub active keys %v", stateAttr(s, "key_id"), active)
						}
						hub.resetCalls()
						return nil
					},
				),
			},
			{
				Config:           withTrigger(hub, "2026-11", nil),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: expectRotationInPlace()},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkRotatedInPlace(hub, &id, &key, false),
					func(*terraform.State) error {
						if p := hub.positionLicense(alphaPositionID); p != id {
							return fmt.Errorf("position linked to %q after rotation, want %s", p, id)
						}
						return nil
					},
				),
			},
			{
				Config:           withTrigger(hub, "2026-11", nil),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
			{
				// Rotation and a parameter change in one apply: one PATCH, one rotation.
				Config:           withTrigger(hub, "2026-12", licenseAttrs{"max_users": "150"}),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: expectRotationInPlace()},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(licAddr, "max_users", "150"),
					resource.TestCheckResourceAttr(licAddr, "rotation_trigger", "2026-12"),
					checkRotatedInPlace(hub, &id, &key, true),
				),
			},
			{
				Config:           licenseConfig(hub, gatewayLicense().with(licenseAttrs{"max_users": "150"})),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: expectRotationInPlace()},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(licAddr, "rotation_trigger"),
					checkRotatedInPlace(hub, &id, &key, false),
				),
			},
		},
	})
}

// A failed rotation keeps the previous key and trigger in state, so the
// next apply retries it; a parameter change sent before it is kept.
func TestLicenseRotationFailureIsRetried(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	rotated := withTrigger(hub, "2026-11", licenseAttrs{"max_users": "70"})

	var id, key string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{
				Config: licenseConfig(hub, gatewayLicense()),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(licAddr, "id", &id),
					captureAttr(licAddr, "license_key", &key),
					func(*terraform.State) error {
						hub.failPath(http.MethodPost, "/keys/rotate", http.StatusServiceUnavailable,
							`{"error":{"code":"signing_unavailable","message":"try again"}}`)
						return nil
					},
				),
			},
			{
				Config:      rotated,
				ExpectError: regexp.MustCompile(`(?s)Rotate licence key failed.*503 signing_unavailable`),
			},
			{
				PreConfig: func() {
					hub.failPath("", "", 0, "")
					if l, _ := hub.license(id); l.Params.MaxUsers == nil || *l.Params.MaxUsers != 70 {
						t.Errorf("PATCH before the failed rotation was lost: %+v", l.Params)
					}
					hub.resetCalls()
				},
				Config:           rotated,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: expectRotationInPlace()},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(licAddr, "max_users", "70"),
					checkRotatedInPlace(hub, &id, &key, false),
				),
			},
		},
	})
}

// A key rotated outside Terraform (portal, customer self-service) leaves the
// key in state revoked. Terraform flags it instead of shipping a dead key:
// the plan fails until rotation_trigger changes, which rotates in place.
func TestLicenseKeyRotatedOutsideTerraform(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, gatewayLicense())

	var id, key string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(licAddr, "id", &id),
					captureAttr(licAddr, "license_key", &key),
				),
			},
			{
				PreConfig: func() {
					hub.rotateKeyOutOfBand(id)
					hub.resetCalls()
				},
				Config:      cfg,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)Licence key revoked in the AI Hub.*rotation_trigger`),
			},
			{
				Config:      licenseConfig(hub, gatewayLicense().with(licenseAttrs{"max_users": "80"})),
				ExpectError: regexp.MustCompile(`(?s)Licence key revoked in the AI Hub.*rotation_trigger`),
			},
			{
				PreConfig: func() {
					if w := hub.writeCalls(); len(w) != 0 {
						t.Errorf("a flagged plan wrote to the Hub: %v", w)
					}
				},
				Config: withTrigger(hub, "after-portal-rotation", nil),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: append(expectRotationInPlace(),
					expectPriorState(licAddr, map[string]string{"key_status": "revoked"}, func(a map[string]any) error {
						if a["license_key"] != key {
							return fmt.Errorf("refresh changed license_key")
						}
						return nil
					}),
				)},
				Check: checkRotatedInPlace(hub, &id, &key, false),
			},
			{
				Config:           withTrigger(hub, "after-portal-rotation", nil),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
		},
	})
}

// A licence ended in the Hub stays ended (SEC-006): it is kept in state with
// status "ended", every plan that would touch it fails until rotation_trigger
// changes, and only then a new licence (with a new key) is issued.
func TestLicenseEndedInHubIsNotReissued(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, gatewayLicense())
	ended := regexp.MustCompile(`(?s)Licence ended in the AI Hub.*change rotation_trigger to issue a new one`)

	var id, key string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(licAddr, "id", &id),
					captureAttr(licAddr, "license_key", &key),
				),
			},
			{
				PreConfig: func() {
					hub.endLicenseOutOfBand(id)
					hub.resetCalls()
				},
				Config:      cfg,
				PlanOnly:    true,
				ExpectError: ended,
			},
			{Config: licenseConfig(hub, gatewayLicense().with(licenseAttrs{"max_users": "80"})), ExpectError: ended},
			{
				PreConfig: func() {
					if w := hub.writeCalls(); len(w) != 0 {
						t.Errorf("a refused plan wrote to the Hub: %v", w)
					}
					if ids := hub.licenseIDsByStatus(""); len(ids) != 1 {
						t.Errorf("licence re-issued without rotation_trigger: %v", ids)
					}
				},
				Config: withTrigger(hub, "reissue", nil),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(licAddr, plancheck.ResourceActionDestroyBeforeCreate),
					// Kept in state with its Hub status and key until now.
					expectPriorState(licAddr, map[string]string{"status": "ended"}, func(a map[string]any) error {
						if a["id"] != id || a["license_key"] != key {
							return fmt.Errorf("the ended licence was not kept in state: %v", a["id"])
						}
						return nil
					}),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(licAddr, "status", "active"),
					resource.TestCheckResourceAttr(licAddr, "key_status", "active"),
					checkReplaced(hub, &id, &key),
				),
			},
			{
				Config:           withTrigger(hub, "reissue", nil),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
		},
	})
}

// Removing an ended licence from the configuration (or destroying it) is
// fine: nothing is re-issued, the repeated end is a no-op in the Hub.
func TestLicenseEndedInHubCanBeDestroyed(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, gatewayLicense())
	var id string

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy: func(*terraform.State) error {
			if ids := hub.licenseIDsByStatus(""); len(ids) != 1 {
				return fmt.Errorf("expected only the original licence, got %v", ids)
			}
			return checkLicensesEnded(hub)(nil)
		},
		Steps: []resource.TestStep{
			{Config: cfg, Check: captureAttr(licAddr, "id", &id)},
			{
				PreConfig: func() { hub.endLicenseOutOfBand(id) },
				Config:    hub.providerBlock(),
			},
		},
	})
}

// A licence suspended in the Hub stays suspended: Terraform shows the status
// and plans nothing, so it never resumes the licence.
func TestLicenseSuspendedInHubStaysSuspended(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, gatewayLicense())
	var id string

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{Config: cfg, Check: captureAttr(licAddr, "id", &id)},
			{
				PreConfig: func() {
					hub.suspendLicenseOutOfBand(id)
					hub.resetCalls()
				},
				Config:           cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(licAddr, "status", "suspended"),
					func(*terraform.State) error {
						for _, c := range hub.writeCalls() {
							if strings.Contains(c, "resume") || strings.HasPrefix(c, "PATCH") {
								return fmt.Errorf("Terraform touched the suspended licence: %v", hub.writeCalls())
							}
						}
						return nil
					},
				),
			},
		},
	})
}

// A licence the Hub does not know at all (404 — the Hub never deletes
// licences, so this is another Hub or lost data) is dropped from state and
// issued anew, as for any resource that disappeared.
func TestLicenseUnknownToHubIsReissued(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, gatewayLicense())

	var id, key string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(licAddr, "id", &id),
					captureAttr(licAddr, "license_key", &key),
					func(*terraform.State) error { hub.forgetLicense(id); return nil },
				),
				ExpectNonEmptyPlan: true,
			},
			{
				Config: cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(licAddr, plancheck.ResourceActionCreate),
				}},
				Check: func(s *terraform.State) error {
					if stateAttr(s, "id") == id || stateAttr(s, "license_key") == key {
						return fmt.Errorf("expected a newly issued licence and key")
					}
					return nil
				},
			},
		},
	})
}
