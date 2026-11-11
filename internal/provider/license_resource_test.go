package provider

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const licAddr = "innfactoryhub_license.gw"

// licenseAttrs maps attribute name → HCL expression; "" drops the attribute.
type licenseAttrs map[string]string

// gatewayLicense is the CompanyGPT rollout shape: an AI Gateway licence for
// Alpha's environment, billed through Alpha's contract position.
func gatewayLicense() licenseAttrs {
	return licenseAttrs{
		"environment_id":       fmt.Sprintf("%q", alphaEnvID),
		"product":              `"ai-gateway"`,
		"domains":              `["gpt.alpha.example"]`,
		"max_users":            "50",
		"edition":              `"all"`,
		"contract_position_id": fmt.Sprintf("%q", alphaPositionID),
	}
}

// pricesLicense is an AI Prices licence for Alpha, billed through Alpha's
// AI Prices position — a product without air-gap operation.
func pricesLicense() licenseAttrs {
	return gatewayLicense().with(licenseAttrs{"product": `"ai-prices"`, "contract_position_id": fmt.Sprintf("%q", alphaPricesPosID)})
}

// with returns a copy with the overrides applied ("" removes an attribute).
func (a licenseAttrs) with(overrides licenseAttrs) licenseAttrs {
	out := maps.Clone(a)
	for k, v := range overrides {
		if v == "" {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

func licenseConfig(hub *fakeHub, attrs licenseAttrs) string {
	var b strings.Builder
	b.WriteString(hub.providerBlock())
	b.WriteString("\nresource \"innfactoryhub_license\" \"gw\" {\n")
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s = %s\n", k, attrs[k])
	}
	b.WriteString("}\n")
	return b.String()
}

// checkLicensesEnded is the CheckDestroy for licence tests: destroy ends a
// licence in the Hub (it stays as history) instead of deleting it.
func checkLicensesEnded(hub *fakeHub) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if active := hub.licenseIDsByStatus("active"); len(active) != 0 {
			return fmt.Errorf("licences still active after destroy: %v", active)
		}
		return nil
	}
}

// stateAttr reads one attribute of the licence from Terraform state.
func stateAttr(s *terraform.State, attr string) string {
	rs, ok := s.RootModule().Resources[licAddr]
	if !ok {
		return ""
	}
	return rs.Primary.Attributes[attr]
}

// The rollout path: a licence bound to a contract position. The Hub issues
// the key exactly once; it lands in state, and the Hub's defaults (all
// features of the edition, 5 instances, no entitlements, not air-gapped) are
// filled in. air_gapped is not configured, so it is not sent.
func TestLicenseCreateLinkedToContractPosition(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{{
			Config: licenseConfig(hub, gatewayLicense()),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttrSet(licAddr, "id"),
				resource.TestCheckResourceAttr(licAddr, "tenant_id", alphaTenantID),
				resource.TestCheckResourceAttr(licAddr, "environment_id", alphaEnvID),
				resource.TestCheckResourceAttr(licAddr, "product", "ai-gateway"),
				resource.TestCheckResourceAttr(licAddr, "domains.#", "1"),
				resource.TestCheckTypeSetElemAttr(licAddr, "domains.*", "gpt.alpha.example"),
				resource.TestCheckResourceAttr(licAddr, "max_users", "50"),
				resource.TestCheckResourceAttr(licAddr, "unlimited", "false"),
				resource.TestCheckResourceAttr(licAddr, "max_instances", "5"),
				resource.TestCheckResourceAttr(licAddr, "edition", "all"),
				resource.TestCheckResourceAttr(licAddr, "features.#", fmt.Sprint(len(allGatewayFeatures))),
				resource.TestCheckTypeSetElemAttr(licAddr, "features.*", "guardrails"),
				resource.TestCheckResourceAttr(licAddr, "entitlements.#", "0"),
				resource.TestCheckResourceAttr(licAddr, "air_gapped", "false"),
				resource.TestCheckResourceAttr(licAddr, "contract_position_id", alphaPositionID),
				resource.TestCheckNoResourceAttr(licAddr, "unbilled_reason"),
				resource.TestCheckResourceAttr(licAddr, "status", "active"),
				resource.TestCheckResourceAttr(licAddr, "valid_until", positionEndDate),
				func(s *terraform.State) error {
					id := stateAttr(s, "id")
					keys := hub.issuedKeys(id)
					if len(keys) != 1 {
						return fmt.Errorf("expected the Hub to issue exactly one key, got %d", len(keys))
					}
					if got := stateAttr(s, "license_key"); got != keys[0] {
						return fmt.Errorf("license_key in state %q, Hub issued %q", got, keys[0])
					}
					want := map[string]any{
						"environment_id": alphaEnvID,
						"product":        "ai-gateway",
						"params": map[string]any{
							"domains":       []any{"gpt.alpha.example"},
							"max_users":     50,
							"max_instances": 5,
							"edition_id":    "all",
							"entitlements":  []any{},
						},
						"contract_position_id": alphaPositionID,
					}
					if got := hub.lastBody("POST /api/v1/admin/licenses"); !jsonEqual(got, want) {
						return fmt.Errorf("POST body\n got: %v\nwant: %v", got, want)
					}
					if got := hub.positionLicense(alphaPositionID); got != id {
						return fmt.Errorf("position linked to %q, want %q", got, id)
					}
					return nil
				},
			),
		}},
	})
}

// A licence without contract position names a reason. Without
// unbilled_max_until the Hub applies its default per reason and the provider
// records it; an explicit date is sent as given.
func TestLicenseCreateUnbilled(t *testing.T) {
	today := fakeToday()
	cases := []struct {
		name         string
		attrs        licenseAttrs
		wantUnbilled map[string]any
		wantMaxUntil string
	}{
		{
			name:         "trial with the Hub default",
			attrs:        licenseAttrs{"unbilled_reason": `"trial"`},
			wantUnbilled: map[string]any{"reason": "trial"},
			wantMaxUntil: today.AddDate(0, 0, 30).Format(isoDay),
		},
		{
			name:         "internal with the Hub default",
			attrs:        licenseAttrs{"unbilled_reason": `"internal"`},
			wantUnbilled: map[string]any{"reason": "internal"},
			wantMaxUntil: today.AddDate(0, 12, 0).Format(isoDay),
		},
		{
			name:         "partner with explicit end",
			attrs:        licenseAttrs{"unbilled_reason": `"partner"`, "unbilled_max_until": fmt.Sprintf("%q", today.AddDate(0, 6, 0).Format(isoDay))},
			wantUnbilled: map[string]any{"reason": "partner", "max_until": today.AddDate(0, 6, 0).Format(isoDay)},
			wantMaxUntil: today.AddDate(0, 6, 0).Format(isoDay),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newLicenseHub(t)
			useFakeHubEnv(t)
			attrs := gatewayLicense().with(licenseAttrs{"contract_position_id": ""}).with(tc.attrs)

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				CheckDestroy:             checkLicensesEnded(hub),
				Steps: []resource.TestStep{
					{
						Config: licenseConfig(hub, attrs),
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckNoResourceAttr(licAddr, "contract_position_id"),
							resource.TestCheckResourceAttr(licAddr, "unbilled_max_until", tc.wantMaxUntil),
							resource.TestCheckResourceAttr(licAddr, "valid_until", tc.wantMaxUntil),
							resource.TestCheckResourceAttrSet(licAddr, "license_key"),
							func(*terraform.State) error {
								body := hub.lastBody("POST /api/v1/admin/licenses")
								if _, ok := body["contract_position_id"]; ok {
									return fmt.Errorf("unbilled licence sent contract_position_id: %v", body)
								}
								if !jsonEqual(body["unbilled"], tc.wantUnbilled) {
									return fmt.Errorf("unbilled\n got: %v\nwant: %v", body["unbilled"], tc.wantUnbilled)
								}
								return nil
							},
						),
					},
					// The recorded Hub default must not show up as a diff.
					{Config: licenseConfig(hub, attrs), PlanOnly: true},
				},
			})
		})
	}
}

// Invalid configuration fails at plan time, before any Hub call — in
// particular a licence without billing link or without user limit.
func TestLicenseConfigValidation(t *testing.T) {
	unbilled := gatewayLicense().with(licenseAttrs{"contract_position_id": "", "unbilled_reason": `"trial"`})
	cases := []struct {
		name    string
		attrs   licenseAttrs
		wantErr string
	}{
		{name: "neither contract position nor unbilled reason", attrs: gatewayLicense().with(licenseAttrs{"contract_position_id": ""}),
			wantErr: `(?s)contract_position_id.*unbilled_reason|(?s)unbilled_reason.*contract_position_id`},
		{name: "contract position and unbilled reason", attrs: gatewayLicense().with(licenseAttrs{"unbilled_reason": `"trial"`}),
			wantErr: `Invalid Attribute Combination`},
		{name: "unbilled end with contract position", attrs: gatewayLicense().with(licenseAttrs{"unbilled_max_until": `"2026-12-31"`}),
			wantErr: `(?s)Invalid Attribute Combination.*unbilled_max_until|(?s)unbilled_max_until.*unbilled_reason`},
		{name: "max_users and unlimited", attrs: gatewayLicense().with(licenseAttrs{"unlimited": "true"}),
			wantErr: `(?s)Invalid Attribute Combination.*max_users`},
		{name: "neither max_users nor unlimited", attrs: gatewayLicense().with(licenseAttrs{"max_users": ""}),
			wantErr: `(?s)max_users.*unlimited`},
		{name: "unlimited false without max_users", attrs: gatewayLicense().with(licenseAttrs{"max_users": "", "unlimited": "false"}),
			wantErr: `(?s)max_users.*unlimited`},
		{name: "unknown product", attrs: gatewayLicense().with(licenseAttrs{"product": `"ai-chat"`}), wantErr: `(?s)product.*value must be one of`},
		{name: "unknown unbilled reason", attrs: unbilled.with(licenseAttrs{"unbilled_reason": `"friend"`}), wantErr: `(?s)unbilled_reason.*value must be one of`},
		{name: "unbilled end not a date", attrs: unbilled.with(licenseAttrs{"unbilled_max_until": `"31.12.2026"`}), wantErr: `(?s)unbilled_max_until.*YYYY-MM-DD`},
		{name: "unbilled end impossible date", attrs: unbilled.with(licenseAttrs{"unbilled_max_until": `"2026-02-30"`}), wantErr: `(?s)unbilled_max_until.*YYYY-MM-DD`},
		{name: "no domain", attrs: gatewayLicense().with(licenseAttrs{"domains": `[]`}), wantErr: `(?s)domains.*at least 1`},
		{name: "domain upper case", attrs: gatewayLicense().with(licenseAttrs{"domains": `["GPT.alpha.example"]`}), wantErr: `(?s)domains.*host\s+name`},
		{name: "domain with scheme", attrs: gatewayLicense().with(licenseAttrs{"domains": `["https://gpt.alpha.example"]`}), wantErr: `(?s)domains.*host\s+name`},
		{name: "domain with port", attrs: gatewayLicense().with(licenseAttrs{"domains": `["gpt.alpha.example:443"]`}), wantErr: `(?s)domains.*host\s+name`},
		{name: "wildcard not leftmost", attrs: gatewayLicense().with(licenseAttrs{"domains": `["gpt.*.alpha.example"]`}), wantErr: `(?s)domains.*host\s+name`},
		{name: "max_users zero", attrs: gatewayLicense().with(licenseAttrs{"max_users": "0"}), wantErr: `(?s)max_users.*between 1 and 1000000`},
		{name: "max_instances too high", attrs: gatewayLicense().with(licenseAttrs{"max_instances": "101"}), wantErr: `(?s)max_instances.*between 1 and 100`},
		{name: "feature upper case", attrs: gatewayLicense().with(licenseAttrs{"features": `["MCP"]`}), wantErr: `(?s)features.*lower-case`},
		{name: "entitlement with space", attrs: gatewayLicense().with(licenseAttrs{"entitlements": `["ai prices"]`}), wantErr: `(?s)entitlements.*lower-case`},
		{name: "blank edition", attrs: gatewayLicense().with(licenseAttrs{"edition": `"  "`}), wantErr: `(?s)edition.*must not be blank`},
		{name: "environment id with slash", attrs: gatewayLicense().with(licenseAttrs{"environment_id": `"env/../x"`}), wantErr: `(?s)environment_id.*identifier`},
		{name: "contract position id with slash", attrs: gatewayLicense().with(licenseAttrs{"contract_position_id": `"pos/1"`}), wantErr: `(?s)contract_position_id.*identifier`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newLicenseHub(t)
			useFakeHubEnv(t)

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				Steps: []resource.TestStep{{
					Config:      licenseConfig(hub, tc.attrs),
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

// expectUpdateInPlace asserts an in-place update that keeps id and the
// licence key known (i.e. carried over, not "known after apply").
func expectUpdateInPlace() []plancheck.PlanCheck {
	return []plancheck.PlanCheck{
		plancheck.ExpectResourceAction(licAddr, plancheck.ResourceActionUpdate),
		plancheck.ExpectKnownValue(licAddr, tfjsonpath.New("id"), knownvalue.NotNull()),
		plancheck.ExpectKnownValue(licAddr, tfjsonpath.New("license_key"), knownvalue.NotNull()),
	}
}

// checkPatchedOnce asserts exactly one PATCH with the given body, the same
// licence id and the same key as at create time, and no further key issued.
func checkPatchedOnce(hub *fakeHub, id, key *string, want map[string]any) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		if got := stateAttr(s, "id"); got != *id {
			return fmt.Errorf("licence id changed from %s to %s", *id, got)
		}
		if got := stateAttr(s, "license_key"); got != *key {
			return fmt.Errorf("license_key changed from %q to %q", *key, got)
		}
		if keys := hub.issuedKeys(*id); len(keys) != 1 {
			return fmt.Errorf("expected exactly one key issued, got %d", len(keys))
		}
		call := "PATCH " + licensePath(*id)
		if w := hub.writeCalls(); len(w) != 1 || w[0] != call {
			return fmt.Errorf("expected exactly one %s, got %v", call, w)
		}
		if got := sortedLists(hub.lastBody(call)); !jsonEqual(got, sortedLists(want)) {
			return fmt.Errorf("PATCH body\n got: %v\nwant: %v", got, want)
		}
		hub.resetCalls()
		return nil
	}
}

// Raising the user limit (the upgrade path) changes the licence in place:
// one PATCH with only max_users, same licence, same key.
func TestLicenseUpdateUserLimitKeepsLicenseKey(t *testing.T) {
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
					func(*terraform.State) error { hub.resetCalls(); return nil },
				),
			},
			{
				Config:           licenseConfig(hub, gatewayLicense().with(licenseAttrs{"max_users": "150"})),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: expectUpdateInPlace()},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(licAddr, "max_users", "150"),
					checkPatchedOnce(hub, &id, &key, map[string]any{"params": map[string]any{"max_users": 150}}),
					func(*terraform.State) error {
						if l, _ := hub.license(id); l.Revision != 2 || *l.Params.MaxUsers != 150 {
							return fmt.Errorf("Hub licence not updated: revision %d, max_users %v", l.Revision, l.Params.MaxUsers)
						}
						return nil
					},
				),
			},
			{
				Config:           licenseConfig(hub, gatewayLicense().with(licenseAttrs{"max_users": "", "unlimited": "true"})),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: expectUpdateInPlace()},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(licAddr, "unlimited", "true"),
					resource.TestCheckNoResourceAttr(licAddr, "max_users"),
					checkPatchedOnce(hub, &id, &key, map[string]any{"params": map[string]any{"max_users": nil}}),
				),
			},
			{
				Config:           licenseConfig(hub, gatewayLicense().with(licenseAttrs{"max_users": "10", "unlimited": "false"})),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: expectUpdateInPlace()},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(licAddr, "unlimited", "false"),
					resource.TestCheckResourceAttr(licAddr, "max_users", "10"),
					checkPatchedOnce(hub, &id, &key, map[string]any{"params": map[string]any{"max_users": 10}}),
				),
			},
		},
	})
}

// Re-running Terraform without changes plans nothing and writes nothing —
// no new revision, no new key, no touched contract position.
func TestLicenseNoChangesMeansNoWrites(t *testing.T) {
	unbilled := gatewayLicense().with(licenseAttrs{"contract_position_id": "", "unbilled_reason": `"trial"`})
	cases := []struct {
		name  string
		attrs licenseAttrs
	}{
		{name: "contract position, Hub defaults", attrs: gatewayLicense()},
		{name: "trial with the Hub default end", attrs: unbilled},
		{name: "unlimited users", attrs: gatewayLicense().with(licenseAttrs{"max_users": "", "unlimited": "true"})},
		{name: "everything configured", attrs: gatewayLicense().with(licenseAttrs{
			"domains":       `["gpt.alpha.example", "*.alpha.example"]`,
			"max_instances": "3",
			"features":      `["mcp", "flows"]`,
			"entitlements":  `["ai-prices"]`,
			"air_gapped":    "true",
		})},
		{name: "air_gapped = false written out", attrs: gatewayLicense().with(licenseAttrs{"air_gapped": "false"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newLicenseHub(t)
			useFakeHubEnv(t)
			cfg := licenseConfig(hub, tc.attrs)

			var id string
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				CheckDestroy:             checkLicensesEnded(hub),
				Steps: []resource.TestStep{
					{
						Config: cfg,
						Check: resource.ComposeAggregateTestCheckFunc(
							captureAttr(licAddr, "id", &id),
							func(*terraform.State) error { hub.resetCalls(); return nil },
						),
					},
					{
						Config: cfg,
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
						},
						Check: func(*terraform.State) error {
							if w := hub.writeCalls(); len(w) != 0 {
								return fmt.Errorf("unchanged config sent writes: %v", w)
							}
							if l, _ := hub.license(id); l.Revision != 1 || len(hub.issuedKeys(id)) != 1 {
								return fmt.Errorf("Hub licence touched: revision %d, %d key(s)", l.Revision, len(hub.issuedKeys(id)))
							}
							return nil
						},
					},
				},
			})
		})
	}
}

// Values the Hub defaulted stay known while unrelated attributes change, so
// an upgrade plan shows only the upgrade. They are recomputed when what they
// derive from changes: features with the edition, the end with the reason.
func TestLicenseUpdatePlanKeepsHubDefaults(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	trial := gatewayLicense().with(licenseAttrs{"contract_position_id": "", "unbilled_reason": `"trial"`})
	trialEnd := fakeToday().AddDate(0, 0, 30).Format(isoDay)
	internalEnd := fakeToday().AddDate(0, 12, 0).Format(isoDay)

	var id, key string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{
				Config: licenseConfig(hub, trial),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(licAddr, "id", &id),
					captureAttr(licAddr, "license_key", &key),
					func(*terraform.State) error { hub.resetCalls(); return nil },
				),
			},
			{
				Config: licenseConfig(hub, trial.with(licenseAttrs{"max_users": "80"})),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: append(expectUpdateInPlace(),
					plancheck.ExpectKnownValue(licAddr, tfjsonpath.New("features"), knownvalue.SetSizeExact(len(allGatewayFeatures))),
					plancheck.ExpectKnownValue(licAddr, tfjsonpath.New("unbilled_max_until"), knownvalue.StringExact(trialEnd)),
				)},
				Check: checkPatchedOnce(hub, &id, &key, map[string]any{"params": map[string]any{"max_users": 80}}),
			},
			{
				Config: licenseConfig(hub, trial.with(licenseAttrs{"max_users": "80", "edition": `"basic"`})),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: append(expectUpdateInPlace(),
					plancheck.ExpectUnknownValue(licAddr, tfjsonpath.New("features")),
				)},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(licAddr, "features.#", "2"),
					resource.TestCheckTypeSetElemAttr(licAddr, "features.*", "flows"),
					checkPatchedOnce(hub, &id, &key, map[string]any{"params": map[string]any{"edition_id": "basic"}}),
				),
			},
			{
				Config: licenseConfig(hub, trial.with(licenseAttrs{"max_users": "80", "edition": `"basic"`, "unbilled_reason": `"internal"`})),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: append(expectUpdateInPlace(),
					plancheck.ExpectUnknownValue(licAddr, tfjsonpath.New("unbilled_max_until")),
				)},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(licAddr, "unbilled_max_until", internalEnd),
					resource.TestCheckResourceAttr(licAddr, "valid_until", internalEnd),
					checkPatchedOnce(hub, &id, &key, map[string]any{"unbilled": map[string]any{"reason": "internal"}}),
				),
			},
		},
	})
}

// sortedLists sorts the string lists inside params: sets have no order, so
// the provider may send domains, features and entitlements in any order.
func sortedLists(body map[string]any) map[string]any {
	params, ok := body["params"].(map[string]any)
	if !ok {
		return body
	}
	out := maps.Clone(body)
	out["params"] = maps.Clone(params)
	for k, v := range params {
		switch list := v.(type) {
		case []any:
			strs := make([]string, 0, len(list))
			for _, e := range list {
				strs = append(strs, fmt.Sprint(e))
			}
			slices.Sort(strs)
			out["params"].(map[string]any)[k] = strs
		case []string:
			strs := slices.Clone(list)
			slices.Sort(strs)
			out["params"].(map[string]any)[k] = strs
		}
	}
	return out
}

// Every other parameter and the billing link change in place: one PATCH with
// exactly the changed field, same licence, same key.
func TestLicenseUpdateInPlace(t *testing.T) {
	trial := gatewayLicense().with(licenseAttrs{"contract_position_id": "", "unbilled_reason": `"trial"`})
	inSixWeeks := fakeToday().AddDate(0, 0, 42).Format(isoDay)
	inFourMonths := fakeToday().AddDate(0, 4, 0).Format(isoDay)
	cases := []struct {
		name  string
		from  licenseAttrs
		to    licenseAttrs
		patch map[string]any
		check resource.TestCheckFunc
	}{
		{
			name: "domains", from: gatewayLicense(),
			to:    gatewayLicense().with(licenseAttrs{"domains": `["gpt.alpha.example", "*.alpha.example"]`}),
			patch: map[string]any{"params": map[string]any{"domains": []string{"gpt.alpha.example", "*.alpha.example"}}},
			check: resource.TestCheckTypeSetElemAttr(licAddr, "domains.*", "*.alpha.example"),
		},
		{
			name: "max_instances", from: gatewayLicense(),
			to:    gatewayLicense().with(licenseAttrs{"max_instances": "10"}),
			patch: map[string]any{"params": map[string]any{"max_instances": 10}},
			check: resource.TestCheckResourceAttr(licAddr, "max_instances", "10"),
		},
		{
			name: "features narrowed", from: gatewayLicense(),
			to:    gatewayLicense().with(licenseAttrs{"features": `["mcp", "agents"]`}),
			patch: map[string]any{"params": map[string]any{"features": []string{"mcp", "agents"}}},
			check: resource.TestCheckResourceAttr(licAddr, "features.#", "2"),
		},
		{
			name: "entitlements", from: gatewayLicense(),
			to:    gatewayLicense().with(licenseAttrs{"entitlements": `["ai-prices"]`}),
			patch: map[string]any{"params": map[string]any{"entitlements": []string{"ai-prices"}}},
			check: resource.TestCheckTypeSetElemAttr(licAddr, "entitlements.*", "ai-prices"),
		},
		{
			name: "air_gapped switched on", from: gatewayLicense(),
			to:    gatewayLicense().with(licenseAttrs{"air_gapped": "true"}),
			patch: map[string]any{"params": map[string]any{"air_gapped": true}},
			check: resource.TestCheckResourceAttr(licAddr, "air_gapped", "true"),
		},
		{
			// Only an explicit false switches it off; removing the attribute
			// would leave the Hub's value (TestLicenseAirGappedSwitchedInPortal).
			name: "air_gapped switched off", from: gatewayLicense().with(licenseAttrs{"air_gapped": "true"}),
			to:    gatewayLicense().with(licenseAttrs{"air_gapped": "false"}),
			patch: map[string]any{"params": map[string]any{"air_gapped": false}},
			check: resource.TestCheckResourceAttr(licAddr, "air_gapped", "false"),
		},
		{
			name:  "edition with configured features",
			from:  gatewayLicense().with(licenseAttrs{"features": `["mcp", "flows"]`}),
			to:    gatewayLicense().with(licenseAttrs{"features": `["mcp", "flows"]`, "edition": `"basic"`}),
			patch: map[string]any{"params": map[string]any{"edition_id": "basic", "features": []string{"mcp", "flows"}}},
			check: resource.TestCheckResourceAttr(licAddr, "edition", "basic"),
		},
		{
			name: "trial becomes billed", from: trial,
			to:    gatewayLicense(),
			patch: map[string]any{"contract_position_id": alphaPositionID},
			check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(licAddr, "valid_until", positionEndDate),
				resource.TestCheckNoResourceAttr(licAddr, "unbilled_reason"),
				resource.TestCheckNoResourceAttr(licAddr, "unbilled_max_until"),
			),
		},
		{
			name: "other contract position", from: gatewayLicense(),
			to:    gatewayLicense().with(licenseAttrs{"contract_position_id": fmt.Sprintf("%q", alphaPosition2ID)}),
			patch: map[string]any{"contract_position_id": alphaPosition2ID},
			check: resource.TestCheckResourceAttr(licAddr, "valid_until", position2EndDate),
		},
		{
			name: "billed becomes partner", from: gatewayLicense(),
			to:    gatewayLicense().with(licenseAttrs{"contract_position_id": "", "unbilled_reason": `"partner"`, "unbilled_max_until": fmt.Sprintf("%q", inFourMonths)}),
			patch: map[string]any{"unbilled": map[string]any{"reason": "partner", "max_until": inFourMonths}},
			check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckNoResourceAttr(licAddr, "contract_position_id"),
				resource.TestCheckResourceAttr(licAddr, "valid_until", inFourMonths),
			),
		},
		{
			name: "trial extended", from: trial,
			to:    trial.with(licenseAttrs{"unbilled_max_until": fmt.Sprintf("%q", inSixWeeks)}),
			patch: map[string]any{"unbilled": map[string]any{"reason": "trial", "max_until": inSixWeeks}},
			check: resource.TestCheckResourceAttr(licAddr, "valid_until", inSixWeeks),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newLicenseHub(t)
			useFakeHubEnv(t)

			var id, key string
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				CheckDestroy:             checkLicensesEnded(hub),
				Steps: []resource.TestStep{
					{
						Config: licenseConfig(hub, tc.from),
						Check: resource.ComposeAggregateTestCheckFunc(
							captureAttr(licAddr, "id", &id),
							captureAttr(licAddr, "license_key", &key),
							func(*terraform.State) error { hub.resetCalls(); return nil },
						),
					},
					{
						Config:           licenseConfig(hub, tc.to),
						ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: expectUpdateInPlace()},
						Check: resource.ComposeAggregateTestCheckFunc(
							checkPatchedOnce(hub, &id, &key, tc.patch),
							tc.check,
							checkPositionLinks(hub, &id),
						),
					},
					{Config: licenseConfig(hub, tc.to), PlanOnly: true},
				},
			})
		})
	}
}

// checkPositionLinks asserts the Hub links exactly the licence's configured
// position (if any) and no other position to it.
func checkPositionLinks(hub *fakeHub, id *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		want := stateAttr(s, "contract_position_id")
		for _, pos := range []string{alphaPositionID, alphaPosition2ID} {
			linked := hub.positionLicense(pos) == *id
			if linked != (pos == want) {
				return fmt.Errorf("position %s linked=%v, licence position %q", pos, linked, want)
			}
		}
		return nil
	}
}

// checkReplaced asserts that the licence in state is a new one with a new
// key, the previous licence is ended in the Hub, and the contract position
// (if any) now points at the new licence.
func checkReplaced(hub *fakeHub, oldID, oldKey *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		id, key := stateAttr(s, "id"), stateAttr(s, "license_key")
		if id == *oldID || key == *oldKey {
			return fmt.Errorf("expected a new licence and key, still %s", id)
		}
		if keys := hub.issuedKeys(id); len(keys) != 1 || keys[0] != key {
			return fmt.Errorf("state key does not match the key issued for %s", id)
		}
		if old, _ := hub.license(*oldID); old.Status != "ended" {
			return fmt.Errorf("previous licence %s is %q, want ended", *oldID, old.Status)
		}
		if pos := stateAttr(s, "contract_position_id"); pos != "" && hub.positionLicense(pos) != id {
			return fmt.Errorf("position %s linked to %q, want %s", pos, hub.positionLicense(pos), id)
		}
		*oldID, *oldKey = id, key
		return nil
	}
}

// Environment and product are the licence's identity; changing them issues
// a new licence (and key) instead of re-pointing the existing one. Destroy
// runs first, so a contract position moves over to the new licence.
func TestLicenseReplacement(t *testing.T) {
	cases := []struct {
		name string
		to   licenseAttrs
	}{
		{name: "environment changes", to: gatewayLicense().with(licenseAttrs{"environment_id": fmt.Sprintf("%q", alphaStagingEnvID)})},
		{name: "product changes", to: gatewayLicense().with(licenseAttrs{
			"product": `"ai-prices"`, "contract_position_id": fmt.Sprintf("%q", alphaPricesPosID),
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
						),
					},
					{
						Config: licenseConfig(hub, tc.to),
						ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction(licAddr, plancheck.ResourceActionDestroyBeforeCreate),
						}},
						Check: checkReplaced(hub, &id, &key),
					},
				},
			})
		})
	}
}

// The rollout lifecycle: create, raise the user limit in place (key stays),
// then import the licence by id. Everything but the key round-trips; the Hub
// never returns the key again, so it is empty after import.
func TestLicenseLifecycleCreateUpdateImport(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	upgraded := licenseConfig(hub, gatewayLicense().with(licenseAttrs{"max_users": "150"}))

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
					func(*terraform.State) error { hub.resetCalls(); return nil },
				),
			},
			{
				Config:           upgraded,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: expectUpdateInPlace()},
				Check:            checkPatchedOnce(hub, &id, &key, map[string]any{"params": map[string]any{"max_users": 150}}),
			},
			{
				Config:                  upgraded,
				ResourceName:            licAddr,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"license_key", "key_id", "key_status"},
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("expected one imported instance, got %d", len(states))
					}
					if k := states[0].Attributes["license_key"]; k != "" {
						return fmt.Errorf("imported license_key = %q, want empty", k)
					}
					return nil
				},
			},
		},
	})
}

// A licence an admin issued in the portal is adopted by import; the next
// plan is empty. Its key is unknown to Terraform until a rotation_trigger
// change rotates the key of the same licence.
func TestLicenseImportPortalCreatedThenRotate(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	trialEnd := fakeToday().AddDate(0, 0, 30).Format(isoDay)
	fifty := int64(50)
	hub.seedLicense(fakeLicense{
		LicenseID: "lic_portal000001", TenantID: alphaTenantID, EnvironmentID: alphaEnvID, Product: "ai-gateway",
		Unbilled: &fakeUnbilled{Reason: "trial", MaxUntil: trialEnd},
		Params: fakeLicenseParams{Domains: []string{"gpt.alpha.example"}, MaxUsers: &fifty, MaxInstances: 5,
			EditionID: "all", Features: allGatewayFeatures, Entitlements: []string{}},
		ValidFrom: fakeToday().Format(isoDay), ValidUntil: trialEnd, Status: "active", Revision: 1,
		CreatedBySource: "admin", CreatedAt: nowISO(), CreatedBy: "uid_admin", UpdatedAt: nowISO(), UpdatedBy: "uid_admin",
	})
	trial := gatewayLicense().with(licenseAttrs{"contract_position_id": "", "unbilled_reason": `"trial"`})

	oldID, oldKey := "lic_portal000001", ""
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{
				Config:             licenseConfig(hub, trial),
				ResourceName:       licAddr,
				ImportState:        true,
				ImportStateId:      oldID,
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					a := states[0].Attributes
					want := map[string]string{
						"id": oldID, "tenant_id": alphaTenantID, "environment_id": alphaEnvID, "product": "ai-gateway",
						"max_users": "50", "unlimited": "false", "max_instances": "5", "edition": "all",
						"unbilled_reason": "trial", "unbilled_max_until": trialEnd, "valid_until": trialEnd,
						"features.#": fmt.Sprint(len(allGatewayFeatures)), "domains.#": "1", "license_key": "",
						"key_id": "", "key_status": "", "status": "active", "air_gapped": "false",
					}
					for k, v := range want {
						if a[k] != v {
							return fmt.Errorf("imported %s = %q, want %q", k, a[k], v)
						}
					}
					return nil
				},
			},
			{
				Config:           licenseConfig(hub, trial),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check: func(*terraform.State) error {
					if w := hub.writeCalls(); len(w) != 0 {
						return fmt.Errorf("adopting the licence wrote to the Hub: %v", w)
					}
					return nil
				},
			},
			{
				// Rotation is the way to a key after import: same licence, new key.
				PreConfig:        hub.resetCalls,
				Config:           licenseConfig(hub, trial.with(licenseAttrs{"rotation_trigger": `"after-import"`})),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: expectRotationInPlace()},
				Check:            checkRotatedInPlace(hub, &oldID, &oldKey, false),
			},
		},
	})
}

// Import cannot recover the key and says so, pointing at rotation_trigger.
func TestLicenseImportWarnsAboutMissingKey(t *testing.T) {
	ctx := context.Background()
	r := newLicenseResource()
	var schemaResp fwresource.SchemaResponse
	r.Schema(ctx, fwresource.SchemaRequest{}, &schemaResp)

	importer, ok := r.(fwresource.ResourceWithImportState)
	if !ok {
		t.Fatal("innfactoryhub_license does not support import")
	}
	resp := fwresource.ImportStateResponse{State: tfsdk.State{
		Schema: schemaResp.Schema,
		Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
	}}
	importer.ImportState(ctx, fwresource.ImportStateRequest{ID: "lic_000000000001"}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("import failed: %v", resp.Diagnostics)
	}
	var warned bool
	for _, d := range resp.Diagnostics.Warnings() {
		warned = warned || strings.Contains(d.Detail(), "rotation_trigger")
	}
	if !warned {
		t.Fatalf("expected a warning that points at rotation_trigger, got %v", resp.Diagnostics)
	}
	var key types.String
	resp.Diagnostics.Append(resp.State.GetAttribute(ctx, path.Root("license_key"), &key)...)
	if !key.IsNull() {
		t.Fatalf("license_key after import = %v, want null", key)
	}
}

// Destroy ends the licence in the Hub (it stays as history) and frees the
// contract position for a successor.
func TestLicenseDestroyEndsLicense(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, gatewayLicense())

	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(licAddr, "id", &id),
					func(*terraform.State) error { hub.resetCalls(); return nil },
				),
			},
			{
				Config:  cfg,
				Destroy: true,
				Check: func(*terraform.State) error {
					l, ok := hub.license(id)
					if !ok || l.Status != "ended" || l.EndedAt == nil {
						return fmt.Errorf("licence %s not ended in the Hub: %+v", id, l)
					}
					if p := hub.positionLicense(alphaPositionID); p != "" {
						return fmt.Errorf("position still linked to %s", p)
					}
					if w := hub.writeCalls(); len(w) != 1 || w[0] != "DELETE "+licensePath(id) {
						return fmt.Errorf("expected exactly one DELETE, got %v", w)
					}
					return nil
				},
			},
		},
	})
}

// A licence the Hub no longer knows (404) counts as ended: destroy succeeds.
func TestLicenseDestroyToleratesUnknownLicense(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	cfg := licenseConfig(hub, gatewayLicense())

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				PreConfig: func() {
					hub.failMethod(http.MethodDelete, http.StatusNotFound, `{"error":{"code":"license_not_found","message":"Lizenz nicht gefunden"}}`)
				},
				Config:  cfg,
				Destroy: true,
			},
		},
	})
}

// Hub rejections surface their code, the Hub's details and a hint on how to
// fix them; nothing ends up in state and no licence is issued.
func TestLicenseCreateRejectedByHub(t *testing.T) {
	trial := gatewayLicense().with(licenseAttrs{"contract_position_id": "", "unbilled_reason": `"trial"`})
	yesterday := fakeToday().AddDate(0, 0, -1).Format(isoDay)
	inNinetyDays := fakeToday().AddDate(0, 0, 90).Format(isoDay)
	cases := []struct {
		name    string
		attrs   licenseAttrs
		seed    bool // seed a licence that already holds alphaPositionID
		wantErr string
	}{
		{name: "position already linked", attrs: gatewayLicense(), seed: true,
			wantErr: `(?s)409 position_already_linked.*lic_holder000001.*terraform\s+import.*create_before_destroy`},
		{name: "position unknown", attrs: gatewayLicense().with(licenseAttrs{"contract_position_id": `"pos_missing"`}),
			wantErr: `(?s)404 position_not_found.*contract_position_id`},
		{name: "position of another customer", attrs: gatewayLicense().with(licenseAttrs{"contract_position_id": fmt.Sprintf("%q", betaPositionID)}),
			wantErr: `(?s)409 position_tenant_mismatch.*another\s+customer`},
		{name: "position for another product", attrs: gatewayLicense().with(licenseAttrs{"contract_position_id": fmt.Sprintf("%q", alphaPricesPosID)}),
			wantErr: `(?s)409 position_product_mismatch.*product`},
		{name: "environment unknown", attrs: gatewayLicense().with(licenseAttrs{"environment_id": `"env_missing"`}),
			wantErr: `(?s)404 environment_not_found.*innfactoryhub_environment`},
		{name: "edition unknown", attrs: gatewayLicense().with(licenseAttrs{"edition": `"enterprise"`}),
			wantErr: `(?s)422 edition_unknown.*"available":\["all","basic"\].*editions`},
		{name: "feature outside the edition", attrs: gatewayLicense().with(licenseAttrs{"features": `["mcp", "teleport"]`}),
			wantErr: `(?s)422 feature_not_in_edition.*teleport.*omit\s+features`},
		{name: "domain the Hub rejects", attrs: gatewayLicense().with(licenseAttrs{"domains": `["gpt.alpha.123"]`}),
			wantErr: `(?s)422 invalid_domain.*gpt\.alpha\.123`},
		{name: "trial end beyond the limit", attrs: trial.with(licenseAttrs{"unbilled_max_until": fmt.Sprintf("%q", inNinetyDays)}),
			wantErr: `(?s)400 unbilled_max_exceeded.*max_until_limit.*60\s+days`},
		{name: "trial end in the past", attrs: trial.with(licenseAttrs{"unbilled_max_until": fmt.Sprintf("%q", yesterday)}),
			wantErr: `(?s)400 unbilled_max_in_past.*today\s+or\s+later`},
		{name: "air-gapped AI Prices licence", attrs: pricesLicense().with(licenseAttrs{"air_gapped": "true"}),
			wantErr: `(?s)422 air_gapped_unsupported.*"product":"ai-prices".*only\s+for\s+ai-gateway`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newLicenseHub(t)
			useFakeHubEnv(t)
			if tc.seed {
				pos, fifty := alphaPositionID, int64(50)
				hub.seedLicense(fakeLicense{
					// In Alpha's staging environment: a running licence in the
					// target environment would stop the create before the Hub.
					LicenseID: "lic_holder000001", TenantID: alphaTenantID, EnvironmentID: alphaStagingEnvID, Product: "ai-gateway",
					ContractPositionID: &pos, Status: "active", Revision: 1,
					Params: fakeLicenseParams{Domains: []string{"old.alpha.example"}, MaxUsers: &fifty, MaxInstances: 5, EditionID: "all"},
				})
			}

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				Steps: []resource.TestStep{{
					Config:      licenseConfig(hub, tc.attrs),
					ExpectError: regexp.MustCompile(`(?s)Create licence failed.*` + tc.wantErr),
				}},
			})
			want := 0
			if tc.seed {
				want = 1
			}
			if ids := hub.licenseIDsByStatus(""); len(ids) != want {
				t.Fatalf("expected %d licence(s) in the Hub, got %v", want, ids)
			}
		})
	}
}

// A rejected PATCH surfaces the Hub's code and leaves state untouched, so a
// later run with the original config plans nothing.
func TestLicenseUpdateRejectedByHub(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	original := licenseConfig(hub, gatewayLicense())

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{Config: original},
			{
				Config:      licenseConfig(hub, gatewayLicense().with(licenseAttrs{"contract_position_id": fmt.Sprintf("%q", betaPositionID)})),
				ExpectError: regexp.MustCompile(`(?s)Update licence failed.*409 position_tenant_mismatch.*another\s+customer`),
			},
			{
				Config:           original,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
		},
	})
}

// The licence key is a sensitive attribute, and the resource has nowhere to
// put a licence file or token: no such attribute exists.
func TestLicenseSchemaKeepsKeySensitiveAndNoLicenseFile(t *testing.T) {
	ctx := context.Background()
	server := providerserver.NewProtocol6(New("test")())()
	resp, err := server.GetProviderSchema(ctx, new(tfprotov6.GetProviderSchemaRequest))
	if err != nil {
		t.Fatalf("GetProviderSchema: %v", err)
	}
	s, ok := resp.ResourceSchemas["innfactoryhub_license"]
	if !ok {
		t.Fatal("innfactoryhub_license is not registered")
	}
	for _, a := range s.Block.Attributes {
		if a.Name == "license_key" && (!a.Sensitive || !a.Computed || a.Optional || a.Required) {
			t.Errorf("license_key must be computed-only and sensitive: %+v", a)
		}
		if strings.Contains(a.Name, "file") || strings.Contains(a.Name, "token") || strings.Contains(a.Name, "jwt") {
			t.Errorf("attribute %q could hold a licence file or token", a.Name)
		}
	}
}

// Over a full lifecycle the provider talks only to the licence CRUD paths —
// never to the licence-file endpoint — and state holds no signed token.
func TestLicenseStateHoldsNoLicenseFile(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	allowed := regexp.MustCompile(`^((GET|POST) /api/v1/admin/licenses|(GET|PATCH|DELETE) /api/v1/admin/licenses/[A-Za-z0-9_-]+)$`)

	noTokenInState := func(s *terraform.State) error {
		for k, v := range s.RootModule().Resources[licAddr].Primary.Attributes {
			if strings.HasPrefix(v, "eyJ") {
				return fmt.Errorf("state attribute %s holds a signed token", k)
			}
		}
		return nil
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy: func(s *terraform.State) error {
			for _, c := range hub.allCalls() {
				if !allowed.MatchString(c) {
					return fmt.Errorf("unexpected Hub call %q", c)
				}
			}
			return checkLicensesEnded(hub)(s)
		},
		Steps: []resource.TestStep{
			{Config: licenseConfig(hub, gatewayLicense()), Check: noTokenInState},
			{Config: licenseConfig(hub, gatewayLicense().with(licenseAttrs{"max_users": "70"})), Check: noTokenInState},
		},
	})
}

// The CompanyGPT rollout: environment and licence in one apply, the licence
// bound to the environment's id (unknown at plan time). Destroy ends the
// licence before the Hub may delete the environment.
func TestLicenseRolloutWithNewEnvironment(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)
	cfg := func(maxUsers int) string {
		return hub.providerBlock() + fmt.Sprintf(`
resource "innfactoryhub_environment" "gw" {
  azure_tenant_id = %q
  name            = "Alpha AI Gateway"
  domain          = "gpt.alpha.example"
  cloud           = "azure"
}

resource "innfactoryhub_license" "gw" {
  environment_id       = innfactoryhub_environment.gw.id
  product              = "ai-gateway"
  domains              = [innfactoryhub_environment.gw.domain]
  max_users            = %d
  edition              = "all"
  contract_position_id = %q
}
`, alphaAzureTenantID, maxUsers, alphaPositionID)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkLicensesEnded(hub),
			checkEnvironmentGone(hub),
		),
		Steps: []resource.TestStep{
			{
				Config: cfg(50),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(licAddr, "environment_id", envAddr, "id"),
					resource.TestCheckResourceAttrPair(licAddr, "tenant_id", envAddr, "tenant_id"),
					resource.TestCheckResourceAttrSet(licAddr, "license_key"),
				),
			},
			{
				Config: cfg(150),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(licAddr, plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction(envAddr, plancheck.ResourceActionNoop),
				}},
			},
			{
				Config:           cfg(150),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
		},
	})
}

// The documented idiom drives limit and billing link from nullable inputs:
// `unlimited = var.max_users == null` and a reason only without position.
func TestLicenseNullableInputIdiom(t *testing.T) {
	cases := []struct {
		name       string
		maxUsers   string
		position   string
		wantParams map[string]any
		wantBody   map[string]any
	}{
		{name: "limited and billed", maxUsers: "50", position: fmt.Sprintf("%q", alphaPositionID),
			wantBody: map[string]any{"contract_position_id": alphaPositionID}, wantParams: map[string]any{"max_users": 50}},
		{name: "unlimited trial", maxUsers: "null", position: "null",
			wantBody: map[string]any{"unbilled": map[string]any{"reason": "trial"}}, wantParams: map[string]any{"max_users": nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newLicenseHub(t)
			useFakeHubEnv(t)
			cfg := hub.providerBlock() + fmt.Sprintf(`
locals {
  max_users = %s
  position  = %s
}

resource "innfactoryhub_license" "gw" {
  environment_id       = %q
  product              = "ai-gateway"
  domains              = ["gpt.alpha.example"]
  edition              = "all"
  max_users            = local.max_users
  unlimited            = local.max_users == null
  contract_position_id = local.position
  unbilled_reason      = local.position == null ? "trial" : null
}
`, tc.maxUsers, tc.position, alphaEnvID)

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				CheckDestroy:             checkLicensesEnded(hub),
				Steps: []resource.TestStep{
					{
						Config: cfg,
						Check: func(*terraform.State) error {
							body := hub.lastBody("POST /api/v1/admin/licenses")
							params, _ := body["params"].(map[string]any)
							if !jsonEqual(params["max_users"], tc.wantParams["max_users"]) {
								return fmt.Errorf("max_users = %v, want %v", params["max_users"], tc.wantParams["max_users"])
							}
							for k, v := range tc.wantBody {
								if !jsonEqual(body[k], v) {
									return fmt.Errorf("%s = %v, want %v", k, body[k], v)
								}
							}
							return nil
						},
					},
					{Config: cfg, PlanOnly: true},
				},
			})
		})
	}
}

// An imported licence (no key in state) can still be changed in place; the
// key stays empty instead of turning into an unknown value.
func TestLicenseImportedLicenseUpdatesInPlace(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	fifty, pos := int64(50), alphaPositionID
	hub.seedLicense(fakeLicense{
		LicenseID: "lic_portal000002", TenantID: alphaTenantID, EnvironmentID: alphaEnvID, Product: "ai-gateway",
		ContractPositionID: &pos, Status: "active", Revision: 1, ValidUntil: positionEndDate,
		Params: fakeLicenseParams{Domains: []string{"gpt.alpha.example"}, MaxUsers: &fifty, MaxInstances: 5,
			EditionID: "all", Features: allGatewayFeatures, Entitlements: []string{}},
	})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{
				Config:             licenseConfig(hub, gatewayLicense()),
				ResourceName:       licAddr,
				ImportState:        true,
				ImportStateId:      "lic_portal000002",
				ImportStatePersist: true,
			},
			{
				Config: licenseConfig(hub, gatewayLicense().with(licenseAttrs{"max_users": "120"})),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(licAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(licAddr, "id", "lic_portal000002"),
					resource.TestCheckResourceAttr(licAddr, "max_users", "120"),
					resource.TestCheckNoResourceAttr(licAddr, "license_key"),
					resource.TestCheckNoResourceAttr(licAddr, "key_id"),
					resource.TestCheckNoResourceAttr(licAddr, "key_status"),
				),
			},
		},
	})
}
