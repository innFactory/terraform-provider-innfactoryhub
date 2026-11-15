package provider

import (
	"fmt"
	"regexp"
	"strings"
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

// An unbilled licence that ends within 30 days is renewed in place: same
// licence, same key, new end at the longest term of its reason.
func TestLicenseRenewalExtendsUnbilledLicense(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{"trial", day(60)},
		{"partner", fakeToday().AddDate(0, 12, 0).Format(isoDay)},
		{"internal", fakeToday().AddDate(0, 12, 0).Format(isoDay)},
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
					resource.TestCheckResourceAttr(licAddr, "unbilled_max_until", fakeToday().AddDate(0, 12, 0).Format(isoDay)),
					resource.TestCheckResourceAttr(licAddr, "renewal_due", "false"),
				),
			},
		},
	})
}

// Fresh licences and licences far from their end are not touched.
func TestLicenseRenewalLeavesHealthyLicenseAlone(t *testing.T) {
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

// With auto_renew_unbilled = false the plan fails and says what to do; so it
// does for an end pinned in the configuration.
func TestLicenseRenewalRefusedWhenNotAutomatic(t *testing.T) {
	cases := []struct {
		name    string
		before  licenseAttrs
		after   licenseAttrs
		wantErr string
	}{
		{name: "auto_renew_unbilled = false",
			before:  unbilledGateway("trial").with(licenseAttrs{"auto_renew_unbilled": "false", "unbilled_max_until": fmt.Sprintf("%q", day(10))}),
			after:   unbilledGateway("trial").with(licenseAttrs{"auto_renew_unbilled": "false"}),
			wantErr: `auto_renew_unbilled is false`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newLicenseHub(t)
			hub.enableRenewal()
			useFakeHubEnv(t)
			var id string
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				CheckDestroy:             checkLicensesEnded(hub),
				Steps: []resource.TestStep{
					{Config: licenseConfig(hub, tc.before), Check: captureAttr(licAddr, "id", &id)},
					{
						Config:      licenseConfig(hub, tc.after),
						PlanOnly:    true,
						ExpectError: regexpFlat(tc.wantErr),
					},
					{
						// Unblocked by pinning a later end: no renewal needed from Terraform.
						Config: licenseConfig(hub, tc.after.with(licenseAttrs{"unbilled_max_until": fmt.Sprintf("%q", day(50)), "auto_renew_unbilled": "true"})),
						Check:  resource.TestCheckResourceAttr(licAddr, "unbilled_max_until", day(50)),
					},
				},
			})
		})
	}
}

// A licence on a contract position follows its contract: never renewed from
// Terraform, whatever the Hub says.
func TestLicenseRenewalIgnoresContractPosition(t *testing.T) {
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
			{PreConfig: func() { hub.resumeLicenseOutOfBand(id) }, Config: cfg, PlanOnly: true},
		},
	})
}

// A Hub from before Spec 003 reports neither managed_by nor renewal: the
// attributes are null and nothing is planned.
func TestLicenseRenewalOldHubWithoutFields(t *testing.T) {
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
	want12 := fakeToday().AddDate(0, 12, 0).Format(isoDay)

	t.Run("plans the longest term", func(t *testing.T) {
		resp := modifyPlanWithConfig(t, newLicenseResource(), base(), base(), cfgOf(base()))
		if resp.Diagnostics.HasError() {
			t.Fatal(resp.Diagnostics)
		}
		var got types.String
		resp.Diagnostics.Append(resp.Plan.GetAttribute(t.Context(), path.Root("unbilled_max_until"), &got)...)
		if got.ValueString() != want12 {
			t.Fatalf("planned end = %q, want %q", got.ValueString(), want12)
		}
		if !warningWith(resp.Diagnostics, "Renewing an unbilled licence") {
			t.Fatalf("no renewal warning: %v", resp.Diagnostics)
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
	t.Run("auto_renew_unbilled = false fails", func(t *testing.T) {
		o := map[string]any{"auto_renew_unbilled": false}
		resp := modifyPlanWithConfig(t, newLicenseResource(), with(o), with(o), cfgOf(with(o)))
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
