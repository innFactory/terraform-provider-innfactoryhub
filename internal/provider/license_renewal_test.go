package provider

import (
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func day(offset int) string { return fakeToday().AddDate(0, 0, offset).Format(isoDay) }

func unbilledGateway(reason string) licenseAttrs {
	return gatewayLicense().with(licenseAttrs{"contract_position_id": "", "unbilled_reason": fmt.Sprintf("%q", reason)})
}

func TestAddMonthsClamped(t *testing.T) {
	d := func(s string) time.Time { v, _ := time.Parse(isoDay, s); return v }
	cases := []struct {
		from   string
		months int
		want   string
	}{
		{"2026-11-15", 12, "2027-11-15"},
		{"2028-02-29", 12, "2029-02-28"}, // AddDate would give 2029-03-01, beyond the Hub's limit
		{"2026-08-31", 6, "2027-02-28"},
		{"2027-01-31", 1, "2027-02-28"},
	}
	for _, tc := range cases {
		if got := addMonthsClamped(d(tc.from), tc.months).Format(isoDay); got != tc.want {
			t.Errorf("addMonthsClamped(%s, %d) = %s, want %s", tc.from, tc.months, got, tc.want)
		}
	}
	if got, _ := unbilledLimit("trial", d("2026-11-15")); got.Format(isoDay) != "2027-01-14" {
		t.Errorf("trial limit = %s", got.Format(isoDay))
	}
	if _, ok := unbilledLimit("other", d("2026-11-15")); ok {
		t.Error("unknown reason has no limit")
	}
}

// An unbilled licence that ends within the Hub's renewal window is renewed in place: same
// licence, same key, new end at the longest term of its reason.
func TestLicenseRenewalExtendsUnbilledLicense(t *testing.T) {
	fixToday(t, "2028-02-29")
	cases := []struct {
		reason string
		want   string
	}{
		{"partner", addMonthsClamped(fakeToday(), 12).Format(isoDay)},
		{"internal", addMonthsClamped(fakeToday(), 12).Format(isoDay)},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			hub := newLicenseHub(t)
			hub.enableRenewal()
			useFakeHubEnv(t)
			soon := unbilledGateway(tc.reason).with(licenseAttrs{"unbilled_max_until": fmt.Sprintf("%q", day(10))})
			renewing := unbilledGateway(tc.reason)
			var id, key string
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				CheckDestroy:             checkLicensesEnded(hub),
				Steps: []resource.TestStep{
					{
						Config: licenseConfig(hub, soon),
						Check: resource.ComposeAggregateTestCheckFunc(
							captureAttr(licAddr, "id", &id),
							captureAttr(licAddr, "license_key", &key),
							resource.TestCheckResourceAttr(licAddr, "managed_by", "terraform"),
							resource.TestCheckResourceAttr(licAddr, "renewal_due", "true"),
							resource.TestCheckResourceAttr(licAddr, "renewal_reason", "expires_soon"),
							resource.TestCheckResourceAttr(licAddr, "auto_renew_unbilled", "true"),
						),
					},
					{
						// The end is no longer pinned: Terraform renews.
						Config: licenseConfig(hub, renewing),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(licAddr, plancheck.ResourceActionUpdate)},
						},
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(licAddr, "unbilled_max_until", tc.want),
							resource.TestCheckResourceAttr(licAddr, "valid_until", tc.want),
							resource.TestCheckResourceAttr(licAddr, "renewal_due", "false"),
							resource.TestCheckNoResourceAttr(licAddr, "renewal_reason"),
							resource.TestCheckResourceAttrPtr(licAddr, "id", &id),
							resource.TestCheckResourceAttrPtr(licAddr, "license_key", &key),
							func(*terraform.State) error {
								if l, _ := hub.license(id); l.Unbilled.MaxUntil != tc.want || l.Status != "active" || len(hub.issuedKeys(id)) != 1 {
									return fmt.Errorf("Hub licence: %+v, %d key(s)", l.Unbilled, len(hub.issuedKeys(id)))
								}
								return nil
							},
						),
					},
					{
						Config:           licenseConfig(hub, renewing),
						ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
					},
				},
			})
		})
	}
}

// An expired unbilled licence is revived by the same renewal.
func TestLicenseRenewalRevivesExpiredLicense(t *testing.T) {
	fixToday(t, "2026-11-15")
	hub := newLicenseHub(t)
	hub.enableRenewal()
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, unbilledGateway("partner"))
	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{Config: cfg, Check: captureAttr(licAddr, "id", &id)},
			{
				PreConfig: func() { hub.setUnbilledEndOutOfBand(id, day(-1), "expired") },
				Config:    cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(licAddr, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(licAddr, "status", "active"),
					resource.TestCheckResourceAttr(licAddr, "unbilled_max_until", addMonthsClamped(fakeToday(), 12).Format(isoDay)),
					resource.TestCheckResourceAttr(licAddr, "renewal_due", "false"),
				),
			},
		},
	})
}

// Trials are never renewed: a warning before the end, an error after it.
func TestLicenseRenewalNeverRenewsTrial(t *testing.T) {
	fixToday(t, "2026-11-15")
	hub := newLicenseHub(t)
	hub.enableRenewal()
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, unbilledGateway("trial"))
	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{Config: cfg, Check: captureAttr(licAddr, "id", &id)},
			{
				PreConfig: func() { hub.setUnbilledEndOutOfBand(id, day(5), "active") },
				Config:    cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(licAddr, "renewal_reason", "expires_soon"),
			},
			{
				PreConfig:   func() { hub.setUnbilledEndOutOfBand(id, day(-1), "expired") },
				Config:      cfg,
				PlanOnly:    true,
				ExpectError: regexpFlat("Terraform does not renew trials"),
			},
			{
				// A deliberately set end is the way out.
				PreConfig: func() { hub.setUnbilledEndOutOfBand(id, day(5), "active") },
				Config:    licenseConfig(hub, unbilledGateway("trial").with(licenseAttrs{"unbilled_max_until": fmt.Sprintf("%q", day(40))})),
				Check:     resource.TestCheckResourceAttr(licAddr, "unbilled_max_until", day(40)),
			},
		},
	})
}

// Fresh licences and licences far from their end are not touched.
func TestLicenseRenewalLeavesHealthyLicenseAlone(t *testing.T) {
	fixToday(t, "2026-11-15")
	hub := newLicenseHub(t)
	hub.enableRenewal()
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, unbilledGateway("partner"))
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{Config: cfg, Check: resource.TestCheckNoResourceAttr(licAddr, "renewal_reason")},
			{Config: cfg, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}},
		},
	})
}

// With auto_renew_unbilled = false an ending licence only warns; an expired
// one fails the plan and says what to do.
func TestLicenseRenewalRefusedWhenNotAutomatic(t *testing.T) {
	fixToday(t, "2026-11-15")
	hub := newLicenseHub(t)
	hub.enableRenewal()
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, unbilledGateway("partner").with(licenseAttrs{"auto_renew_unbilled": "false"}))
	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{Config: cfg, Check: captureAttr(licAddr, "id", &id)},
			{
				PreConfig: func() { hub.setUnbilledEndOutOfBand(id, day(5), "active") },
				Config:    cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(licAddr, "renewal_reason", "expires_soon"),
			},
			{
				PreConfig:   func() { hub.setUnbilledEndOutOfBand(id, day(-1), "expired") },
				Config:      cfg,
				PlanOnly:    true,
				ExpectError: regexpFlat("auto_renew_unbilled is false"),
			},
			{
				// Unblocked by pinning a later end.
				Config: licenseConfig(hub, unbilledGateway("partner").with(licenseAttrs{"auto_renew_unbilled": "false", "unbilled_max_until": fmt.Sprintf("%q", day(50))})),
				Check:  resource.TestCheckResourceAttr(licAddr, "unbilled_max_until", day(50)),
			},
		},
	})
}

// A licence on a contract position follows its contract: never renewed from
// Terraform, whatever the Hub says.
func TestLicenseRenewalIgnoresContractPosition(t *testing.T) {
	fixToday(t, "2026-11-15")
	hub := newLicenseHub(t)
	hub.enableRenewal()
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, gatewayLicense())
	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{Config: cfg, Check: captureAttr(licAddr, "id", &id)},
			{
				PreConfig: func() { hub.forceRenewalReason(id, "expires_soon") },
				Config:    cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(licAddr, "renewal_reason", "expires_soon"),
			},
		},
	})
}

// A licence suspended in the portal is an emergency stop: the plan warns and
// plans nothing.
func TestLicenseRenewalSuspendedWarns(t *testing.T) {
	fixToday(t, "2026-11-15")
	hub := newLicenseHub(t)
	hub.enableRenewal()
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, unbilledGateway("partner"))
	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             func(*terraform.State) error { return nil },
		Steps: []resource.TestStep{
			{Config: cfg, Check: captureAttr(licAddr, "id", &id)},
			{
				PreConfig: func() { hub.suspendLicenseOutOfBand(id) },
				Config:    cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(licAddr, "renewal_reason", "suspended"),
			},
			{PreConfig: func() { hub.resumeLicenseOutOfBand(id) }, Config: cfg},
		},
	})
}

// A Hub from before Spec 003 reports neither managed_by nor renewal: the
// attributes are null and nothing is planned.
func TestLicenseRenewalOldHubWithoutFields(t *testing.T) {
	fixToday(t, "2026-11-15")
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, unbilledGateway("trial").with(licenseAttrs{"unbilled_max_until": fmt.Sprintf("%q", day(5))}))
	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{Config: cfg, Check: captureAttr(licAddr, "id", &id)},
			{
				Config: cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(licAddr, "managed_by"),
					resource.TestCheckNoResourceAttr(licAddr, "renewal_due"),
					resource.TestCheckNoResourceAttr(licAddr, "renewal_reason"),
					resource.TestCheckResourceAttr(licAddr, "auto_renew_unbilled", "true"),
				),
			},
		},
	})
}

// ModifyPlan on its own: what it plans for each verdict of the Hub.
func TestLicenseRenewalPlanUnit(t *testing.T) {
	fixToday(t, "2026-11-15")
	base := func() map[string]any {
		return map[string]any{
			"id": "lic_1", "environment_id": "env_1", "product": "ai-gateway", "status": "active",
			"unbilled_reason": "partner", "unbilled_max_until": day(5), "renewal_reason": "expires_soon", "auto_renew_unbilled": true,
		}
	}
	with := func(over map[string]any) map[string]any {
		m := base()
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	// The configuration leaves the end unset; the plan carries the state's.
	cfgOf := func(m map[string]any) map[string]any {
		c := map[string]any{}
		for k, v := range m {
			c[k] = v
		}
		c["unbilled_max_until"] = tftypes.NewValue(tftypes.String, nil)
		return c
	}
	want12 := addMonthsClamped(fakeToday(), 12).Format(isoDay)

	t.Run("plans the longest term", func(t *testing.T) {
		resp := modifyPlanWithConfig(t, newLicenseResource(), base(), base(), cfgOf(base()))
		if resp.Diagnostics.HasError() {
			t.Fatal(resp.Diagnostics)
		}
		var got types.String
		resp.Diagnostics.Append(resp.Plan.GetAttribute(t.Context(), path.Root("unbilled_max_until"), &got)...)
		if !got.IsUnknown() {
			t.Fatalf("planned end = %q, want unknown (counted at apply time)", got.ValueString())
		}
		if !warningWith(resp.Diagnostics, "Renewing an unbilled licence") || !warningWith(resp.Diagnostics, want12) {
			t.Fatalf("no renewal warning naming %s: %v", want12, resp.Diagnostics)
		}
	})
	t.Run("state without auto_renew_unbilled counts as true", func(t *testing.T) {
		s := base()
		delete(s, "auto_renew_unbilled")
		p := base()
		delete(p, "auto_renew_unbilled")
		resp := modifyPlanWithConfig(t, newLicenseResource(), s, p, cfgOf(p))
		if resp.Diagnostics.HasError() || !warningWith(resp.Diagnostics, "Renewing") {
			t.Fatalf("diagnostics: %v", resp.Diagnostics)
		}
	})
	t.Run("nothing to do", func(t *testing.T) {
		for name, over := range map[string]map[string]any{
			"no reason":                            {"renewal_reason": tftypes.NewValue(tftypes.String, nil)},
			"ended is handled by rotation_trigger": {"renewal_reason": "ended"},
			"key revoked":                          {"renewal_reason": "key_revoked"},
			"already at the limit":                 {"unbilled_max_until": want12},
			"contract position":                    {"contract_position_id": "pos_1", "unbilled_reason": tftypes.NewValue(tftypes.String, nil), "unbilled_max_until": tftypes.NewValue(tftypes.String, nil)},
		} {
			t.Run(name, func(t *testing.T) {
				resp := modifyPlanWithConfig(t, newLicenseResource(), with(over), with(over), cfgOf(with(over)))
				if resp.Diagnostics.HasError() || warningWith(resp.Diagnostics, "Renewing") {
					t.Fatalf("diagnostics: %v", resp.Diagnostics)
				}
			})
		}
	})
	t.Run("a new end in the configuration wins", func(t *testing.T) {
		p := with(map[string]any{"unbilled_max_until": day(40)})
		resp := modifyPlanWithConfig(t, newLicenseResource(), base(), p, p)
		if resp.Diagnostics.HasError() || warningWith(resp.Diagnostics, "Renewing") {
			t.Fatalf("diagnostics: %v", resp.Diagnostics)
		}
	})
	t.Run("a pinned end only warns", func(t *testing.T) {
		resp := modifyPlanValues(t, newLicenseResource(), base(), base())
		if resp.Diagnostics.HasError() || !warningWith(resp.Diagnostics, "Unbilled licence ends soon") {
			t.Fatalf("diagnostics: %v", resp.Diagnostics)
		}
	})
	t.Run("auto_renew_unbilled = false: warns before the end, fails after", func(t *testing.T) {
		o := map[string]any{"auto_renew_unbilled": false}
		resp := modifyPlanWithConfig(t, newLicenseResource(), with(o), with(o), cfgOf(with(o)))
		if resp.Diagnostics.HasError() || !warningWith(resp.Diagnostics, "Unbilled licence ends soon") {
			t.Fatalf("diagnostics: %v", resp.Diagnostics)
		}
		o = map[string]any{"auto_renew_unbilled": false, "renewal_reason": "expired"}
		resp = modifyPlanWithConfig(t, newLicenseResource(), with(o), with(o), cfgOf(with(o)))
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error")
		}
	})
	t.Run("trial: warns before the end, fails after", func(t *testing.T) {
		soon := map[string]any{"unbilled_reason": "trial"}
		resp := modifyPlanWithConfig(t, newLicenseResource(), with(soon), with(soon), cfgOf(with(soon)))
		if resp.Diagnostics.HasError() || !warningWith(resp.Diagnostics, "Trial licence ends soon") || warningWith(resp.Diagnostics, "Renewing") {
			t.Fatalf("diagnostics: %v", resp.Diagnostics)
		}
		gone := map[string]any{"unbilled_reason": "trial", "renewal_reason": "expired"}
		resp = modifyPlanWithConfig(t, newLicenseResource(), with(gone), with(gone), cfgOf(with(gone)))
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error")
		}
	})
	t.Run("suspended warns", func(t *testing.T) {
		o := map[string]any{"status": "suspended", "renewal_reason": "suspended"}
		resp := modifyPlanWithConfig(t, newLicenseResource(), with(o), with(o), cfgOf(with(o)))
		if resp.Diagnostics.HasError() || !warningWith(resp.Diagnostics, "Terraform does not intervene") {
			t.Fatalf("diagnostics: %v", resp.Diagnostics)
		}
	})
}

// regexpFlat matches s although Terraform wraps long error text.
func regexpFlat(s string) *regexp.Regexp {
	return regexp.MustCompile(strings.ReplaceAll(regexp.QuoteMeta(s), " ", `\s+`))
}

// Staff link a Terraform licence to a contract position in the portal:
// while the configuration is the one Terraform applied last, the Hub's link
// stays (empty plan, no write); a changed configuration wins.
func TestLicenseBillingLinkSetInPortalIsKept(t *testing.T) {
	fixToday(t, "2026-11-15")
	hub := newLicenseHub(t)
	hub.enableRenewal()
	useFakeHubEnv(t)
	unbilled := licenseConfig(hub, unbilledGateway("partner"))
	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{Config: unbilled, Check: captureAttr(licAddr, "id", &id)},
			{
				// Linked in the portal; the configuration still says unbilled.
				PreConfig: func() { hub.linkPositionOutOfBand(id, alphaPositionID); hub.resetCalls() },
				Config:    unbilled,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: func(*terraform.State) error {
					if w := hub.writeCalls(); len(w) != 0 {
						return fmt.Errorf("Terraform wrote: %v", w)
					}
					if l, _ := hub.license(id); l.ContractPositionID == nil || *l.ContractPositionID != alphaPositionID {
						return fmt.Errorf("the Hub's link was undone: %+v", l.ContractPositionID)
					}
					return nil
				},
			},
			{
				// Another position in the configuration is taken over.
				Config: licenseConfig(hub, gatewayLicense().with(licenseAttrs{"contract_position_id": fmt.Sprintf("%q", alphaPosition2ID)})),
				Check:  resource.TestCheckResourceAttr(licAddr, "contract_position_id", alphaPosition2ID),
			},
		},
	})
}

// Terraform itself billed the licence on a position; the configuration then
// switches to unbilled: that is a changed configuration, it wins.
func TestLicenseTerraformPositionToUnbilled(t *testing.T) {
	fixToday(t, "2026-11-15")
	hub := newLicenseHub(t)
	hub.enableRenewal()
	useFakeHubEnv(t)
	var id string
	to := unbilledGateway("partner")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{Config: licenseConfig(hub, gatewayLicense()), Check: captureAttr(licAddr, "id", &id)},
			{
				Config: licenseConfig(hub, to),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(licAddr, "contract_position_id"),
					resource.TestCheckResourceAttr(licAddr, "unbilled_reason", "partner"),
					func(*terraform.State) error {
						if l, _ := hub.license(id); l.Unbilled == nil || l.ContractPositionID != nil || hub.positionLicense(alphaPositionID) != "" {
							return fmt.Errorf("Hub link: %+v / %v", l.Unbilled, l.ContractPositionID)
						}
						return nil
					},
				),
			},
			{Config: licenseConfig(hub, to), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}},
		},
	})
}

// Staff change the unbilled reason in the portal; the configuration is
// unchanged, so Terraform does not turn it back.
func TestLicensePortalReasonChangeIsKept(t *testing.T) {
	fixToday(t, "2026-11-15")
	hub := newLicenseHub(t)
	hub.enableRenewal()
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, unbilledGateway("partner"))
	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{Config: cfg, Check: captureAttr(licAddr, "id", &id)},
			{
				PreConfig: func() {
					hub.mu.Lock()
					l := hub.licenses[id]
					l.Unbilled.Reason, l.Unbilled.MaxUntil, l.ValidUntil = "internal", day(200), day(200)
					hub.mu.Unlock()
					hub.resetCalls()
				},
				Config: cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: func(*terraform.State) error {
					if w := hub.writeCalls(); len(w) != 0 {
						return fmt.Errorf("Terraform wrote: %v", w)
					}
					if l, _ := hub.license(id); l.Unbilled.Reason != "internal" {
						return fmt.Errorf("Hub reason undone: %+v", l.Unbilled)
					}
					return nil
				},
			},
		},
	})
}

// The configuration names a position; staff switch the licence to unbilled
// in the portal: Terraform does not link it back.
func TestLicensePortalUnlinkIsKept(t *testing.T) {
	fixToday(t, "2026-11-15")
	hub := newLicenseHub(t)
	hub.enableRenewal()
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
					hub.mu.Lock()
					l := hub.licenses[id]
					old := l.ContractPositionID
					l.ContractPositionID, l.ContractID = nil, nil
					l.Unbilled = &fakeUnbilled{Reason: "partner", MaxUntil: day(100)}
					l.ValidUntil = day(100)
					hub.linkPositionLocked(l, old)
					hub.mu.Unlock()
					hub.resetCalls()
				},
				Config: cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: func(*terraform.State) error {
					if w := hub.writeCalls(); len(w) != 0 {
						return fmt.Errorf("Terraform wrote: %v", w)
					}
					return nil
				},
			},
		},
	})
}

// State without private state (written by the earlier version, or an import):
// the Hub's position wins over an unbilled_reason in the configuration.
func TestLicenseBillingLinkWithoutPrivateStateHubPositionWins(t *testing.T) {
	fixToday(t, "2026-11-15")
	hub := newLicenseHub(t)
	hub.enableRenewal()
	useFakeHubEnv(t)
	id := seedLicenseIn(hub, "lic_import000001", alphaEnvID, "ai-gateway", "active")
	hub.linkPositionOutOfBand(id, alphaPositionID)
	cfg := licenseConfig(hub, unbilledGateway("partner").with(licenseAttrs{"domains": `["gpt.example.com"]`, "max_users": "5"}))
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             func(*terraform.State) error { return nil },
		Steps: []resource.TestStep{
			{Config: cfg, ImportState: true, ResourceName: licAddr, ImportStateId: id, ImportStatePersist: true},
			{
				PreConfig: func() { hub.resetCalls() },
				Config:    cfg,
				Check: func(*terraform.State) error {
					for _, w := range hub.writeCalls() {
						if strings.HasPrefix(w, "PATCH") {
							return fmt.Errorf("Terraform patched the Hub's link: %v", hub.writeCalls())
						}
					}
					return nil
				},
			},
		},
	})
}

// A saved plan applied after the UTC date changed: the renewal date is
// counted at apply time, so the plan stays valid.
func TestLicenseRenewalDateCountedAtApplyTime(t *testing.T) {
	fixToday(t, "2026-11-15")
	hub := newLicenseHub(t)
	hub.enableRenewal()
	useFakeHubEnv(t)
	soon := unbilledGateway("partner").with(licenseAttrs{"unbilled_max_until": fmt.Sprintf("%q", day(10))})
	renewing := unbilledGateway("partner")
	planDay := fakeToday()
	var calls atomic.Int32
	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{Config: licenseConfig(hub, soon), Check: captureAttr(licAddr, "id", &id)},
			{
				PreConfig: func() {
					// The first plan sees today; every later call (the apply) the next day.
					renewalClock = func() time.Time {
						if calls.Add(1) == 1 {
							return planDay
						}
						fakeNow = func() time.Time { return planDay.AddDate(0, 0, 1) } // the Hub's day moves on too
						return planDay.AddDate(0, 0, 1)
					}
				},
				Config: licenseConfig(hub, renewing),
				Check: func(*terraform.State) error {
					want := addMonthsClamped(planDay.AddDate(0, 0, 1), 12).Format(isoDay)
					if l, _ := hub.license(id); l.Unbilled.MaxUntil != want {
						return fmt.Errorf("renewed to %s, want %s (apply day + 12 months)", l.Unbilled.MaxUntil, want)
					}
					return nil
				},
			},
		},
	})
}

// A partner licence that runs out soon without a pinned date is renewed.
func TestLicenseRenewalPartnerExpiresSoonWithoutPinnedDate(t *testing.T) {
	fixToday(t, "2026-11-15")
	hub := newLicenseHub(t)
	hub.enableRenewal()
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, unbilledGateway("partner"))
	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{Config: cfg, Check: captureAttr(licAddr, "id", &id)},
			{
				PreConfig: func() { hub.setUnbilledEndOutOfBand(id, day(5), "active") },
				Config:    cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(licAddr, plancheck.ResourceActionUpdate)},
				},
				Check: resource.TestCheckResourceAttr(licAddr, "unbilled_max_until", addMonthsClamped(fakeToday(), 12).Format(isoDay)),
			},
			{Config: cfg, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}},
		},
	})
}

// air_gapped unset: the Hub's value (here switched on in the portal) stays.
func TestLicenseAirGappedUnsetKeepsHubValueWithBillingLink(t *testing.T) {
	fixToday(t, "2026-11-15")
	hub := newLicenseHub(t)
	hub.enableRenewal()
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, gatewayLicense())
	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{Config: cfg, Check: captureAttr(licAddr, "id", &id)},
			{
				PreConfig: func() { hub.setAirGappedOutOfBand(id, true); hub.resetCalls() },
				Config:    cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(licAddr, "air_gapped", "true"),
			},
		},
	})
}

// The Hub refuses to end a suspended licence for an API key (409
// license_suspended): Terraform says why and what to do.
func TestLicenseSuspendedCannotBeReplaced(t *testing.T) {
	fixToday(t, "2026-11-15")
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
				PreConfig:   func() { hub.suspendLicenseOutOfBand(id) },
				Config:      licenseConfig(hub, gatewayLicense().with(licenseAttrs{"product": `"ai-prices"`, "contract_position_id": fmt.Sprintf("%q", alphaPricesPosID)})),
				ExpectError: regexpFlat("suspended in the AI Hub (emergency stop)"),
			},
			{PreConfig: func() { hub.resumeLicenseOutOfBand(id) }, Config: cfg},
		},
	})
}

// A new licence for an environment and product whose licence is suspended is
// refused with the same explanation.
func TestLicenseCreateRefusedWhileSuspended(t *testing.T) {
	fixToday(t, "2026-11-15")
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
				PreConfig: func() { hub.suspendLicenseOutOfBand(id) },
				Config: cfg + `
resource "innfactoryhub_license" "second" {
  environment_id = "` + alphaEnvID + `"
  product        = "ai-gateway"
  domains        = ["gpt2.alpha.example"]
  max_users      = 5
  edition        = "all"
  unbilled_reason = "partner"
}
`,
				ExpectError: regexpFlat("emergency stop"),
			},
			{PreConfig: func() { hub.resumeLicenseOutOfBand(id) }, Config: cfg},
		},
	})
}

// A pinned unbilled_max_until that staff move in the portal stays while the
// configuration is unchanged; a changed configuration wins.
func TestLicensePortalChangeOfPinnedEndIsKept(t *testing.T) {
	fixToday(t, "2026-11-15")
	hub := newLicenseHub(t)
	hub.enableRenewal()
	useFakeHubEnv(t)
	pinned := func(d string) string {
		return licenseConfig(hub, unbilledGateway("partner").with(licenseAttrs{"unbilled_max_until": fmt.Sprintf("%q", d)}))
	}
	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{Config: pinned(day(50)), Check: captureAttr(licAddr, "id", &id)},
			{
				PreConfig: func() { hub.setUnbilledEndOutOfBand(id, day(100), "active"); hub.resetCalls() },
				Config:    pinned(day(50)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: func(*terraform.State) error {
					if w := hub.writeCalls(); len(w) != 0 {
						return fmt.Errorf("Terraform wrote: %v", w)
					}
					if l, _ := hub.license(id); l.Unbilled.MaxUntil != day(100) {
						return fmt.Errorf("the portal's end was turned back: %+v", l.Unbilled)
					}
					return nil
				},
			},
			{
				Config: pinned(day(60)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(licAddr, plancheck.ResourceActionUpdate)},
				},
				Check: resource.TestCheckResourceAttr(licAddr, "unbilled_max_until", day(60)),
			},
			{Config: pinned(day(60)), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}},
		},
	})
}
