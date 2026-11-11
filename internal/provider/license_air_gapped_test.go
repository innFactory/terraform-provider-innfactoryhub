package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
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

// Air-gap operation (`params.air_gapped`): the AI Hub issues licence files
// only for such licences; an admin switches it in the portal, and switching
// it off revokes the files. The Hub treats it as an ordinary parameter
// (default false, changeable by PATCH, true only for ai-gateway). The
// provider sends it only when the configuration sets it: unset, the Hub's
// value stays and is only read.

// air_gapped is optional and computed without a default: unset, the plan
// keeps the value read from the Hub; set, it changes in place — never a new
// licence.
func TestLicenseSchemaAirGapped(t *testing.T) {
	ctx := context.Background()
	var resp fwresource.SchemaResponse
	newLicenseResource().Schema(ctx, fwresource.SchemaRequest{}, &resp)

	a, ok := resp.Schema.Attributes["air_gapped"].(schema.BoolAttribute)
	if !ok {
		t.Fatalf("air_gapped is %T, want a bool attribute", resp.Schema.Attributes["air_gapped"])
	}
	if !a.Optional || !a.Computed || a.Required || a.Sensitive {
		t.Errorf("air_gapped must be optional and computed: %+v", a)
	}
	if a.Default != nil {
		t.Errorf("air_gapped has a default; unset must leave the Hub's value alone")
	}

	// A resource in state (all attributes null is enough for the modifiers).
	objType := resp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	nulls := map[string]tftypes.Value{}
	for name, typ := range objType.AttributeTypes {
		nulls[name] = tftypes.NewValue(typ, nil)
	}
	inState := tfsdk.State{Schema: resp.Schema, Raw: tftypes.NewValue(objType, nulls)}
	cases := []struct {
		name                string
		config, state, plan types.Bool
		wantPlan            types.Bool
	}{
		{name: "unset keeps the Hub's true", config: types.BoolNull(), state: types.BoolValue(true), plan: types.BoolUnknown(), wantPlan: types.BoolValue(true)},
		{name: "unset keeps the Hub's false", config: types.BoolNull(), state: types.BoolValue(false), plan: types.BoolUnknown(), wantPlan: types.BoolValue(false)},
		{name: "configured false over the Hub's true", config: types.BoolValue(false), state: types.BoolValue(true), plan: types.BoolValue(false), wantPlan: types.BoolValue(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := planmodifier.BoolRequest{Path: path.Root("air_gapped"), ConfigValue: tc.config, StateValue: tc.state, PlanValue: tc.plan, State: inState}
			resp := planmodifier.BoolResponse{PlanValue: tc.plan}
			for _, m := range a.PlanModifiers {
				m.PlanModifyBool(ctx, req, &resp)
			}
			if resp.Diagnostics.HasError() || resp.RequiresReplace {
				t.Fatalf("plan modifiers: requires replace %v, %v", resp.RequiresReplace, resp.Diagnostics)
			}
			if !resp.PlanValue.Equal(tc.wantPlan) {
				t.Fatalf("planned %v, want %v", resp.PlanValue, tc.wantPlan)
			}
		})
	}
}

// Create sends air_gapped inside params only when it is configured; unset
// (unknown in the create plan), the Hub's false applies.
func TestLicenseCreateBodyAirGapped(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		value types.Bool
		want  any // nil: not sent
	}{
		{name: "unset", value: types.BoolUnknown()},
		{name: "null", value: types.BoolNull()},
		{name: "false", value: types.BoolValue(false), want: false},
		{name: "true", value: types.BoolValue(true), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			m := licenseModel{AirGapped: tc.value, Entitlements: strSet(ctx, &diags, nil)}
			raw, err := json.Marshal(m.createBody(ctx, &diags))
			if err != nil || diags.HasError() {
				t.Fatalf("createBody: %v %v", err, diags)
			}
			var body struct {
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			got, sent := body.Params["air_gapped"]
			if sent != (tc.want != nil) || got != tc.want {
				t.Errorf("params.air_gapped = %v (sent %v), want %v: %s", got, sent, tc.want, raw)
			}
		})
	}
}

// Read takes air_gapped from the Hub, so state shows a switch made in the
// portal; a response without the field (a licence the Hub stored before it
// existed) reads as false, like the Hub itself reads it.
func TestLicenseAirGappedFromHubResponse(t *testing.T) {
	const params = `"domains":["gpt.alpha.example"],"max_users":50,"max_instances":5,"edition_id":"all","features":[],"entitlements":[]`
	cases := []struct {
		name   string
		params string
		state  types.Bool
		want   bool
	}{
		{name: "air-gapped", params: params + `,"air_gapped":true`, state: types.BoolValue(false), want: true},
		{name: "online", params: params + `,"air_gapped":false`, state: types.BoolValue(true), want: false},
		{name: "response without the field", params: params, state: types.BoolValue(true), want: false},
		{name: "state written before air_gapped existed", params: params + `,"air_gapped":false`, state: types.BoolNull(), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"license":{"license_id":"lic_1","tenant_id":"tnt_alpha","environment_id":"env_1","product":"ai-gateway",` +
				`"contract_position_id":null,"unbilled":{"reason":"trial","max_until":"2026-12-31"},"params":{` + tc.params + `},` +
				`"valid_until":"2026-12-31","status":"active"},"keys":[]}`
			var resp licenseResponse
			if err := json.Unmarshal([]byte(raw), &resp); err != nil {
				t.Fatal(err)
			}
			var diags diag.Diagnostics
			m := licenseModel{AirGapped: tc.state}
			m.applyRemote(context.Background(), &diags, resp.License)
			if diags.HasError() {
				t.Fatal(diags)
			}
			if !m.AirGapped.Equal(types.BoolValue(tc.want)) {
				t.Fatalf("air_gapped = %v, want %v", m.AirGapped, tc.want)
			}
		})
	}
}

// State written before air_gapped existed: Terraform hands the stored JSON
// to the provider, which reads the missing attribute as null; the refresh
// fills in the Hub's false, and the plan for the unchanged configuration is
// empty — no perpetual diff, no write.
func TestLicenseStateWithoutAirGappedPlansNoChange(t *testing.T) {
	ctx := context.Background()
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	fifty, pos := int64(50), alphaPositionID
	hub.seedLicense(fakeLicense{
		LicenseID: "lic_before000001", TenantID: alphaTenantID, EnvironmentID: alphaEnvID, Product: "ai-gateway",
		ContractPositionID: &pos, Status: "active", Revision: 1, ValidUntil: positionEndDate,
		Params: fakeLicenseParams{Domains: []string{"gpt.alpha.example"}, MaxUsers: &fifty, MaxInstances: 5,
			EditionID: "all", Features: allGatewayFeatures, Entitlements: []string{}},
	})
	hub.rotateKeyOutOfBand("lic_before000001") // the key the earlier apply stored
	key := hub.issuedKeys("lic_before000001")[0]
	const name = "innfactoryhub_license"
	oldState := []byte(`{"id":"lic_before000001","environment_id":"` + alphaEnvID + `","product":"ai-gateway",` +
		`"domains":["gpt.alpha.example"],"max_users":50,"unlimited":false,"max_instances":5,"edition":"all",` +
		`"features":["mcp","flows","agents","companion","guardrails","insights"],"entitlements":[],` +
		`"contract_position_id":"` + alphaPositionID + `","unbilled_reason":null,"unbilled_max_until":null,"rotation_trigger":null,` +
		`"license_key":"` + key + `","key_id":"` + licenseKeyID(key) + `","key_status":"active",` +
		`"tenant_id":"` + alphaTenantID + `","status":"active","valid_until":"` + positionEndDate + `"}`)

	server := providerserver.NewProtocol6(New("test")())()
	schemas, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	providerType, licType := schemas.Provider.ValueType(), schemas.ResourceSchemas[name].ValueType()
	dynamic := func(typ tftypes.Type, v tftypes.Value) *tfprotov6.DynamicValue {
		t.Helper()
		dv, err := tfprotov6.NewDynamicValue(typ, v)
		if err != nil {
			t.Fatal(err)
		}
		return &dv
	}
	noErrors := func(step string, err error, diags []*tfprotov6.Diagnostic) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		for _, d := range diags {
			if d.Severity == tfprotov6.DiagnosticSeverityError {
				t.Fatalf("%s: %s: %s", step, d.Summary, d.Detail)
			}
		}
	}
	attrs := func(dv *tfprotov6.DynamicValue) map[string]tftypes.Value {
		t.Helper()
		v, err := dv.Unmarshal(licType)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]tftypes.Value{}
		if err := v.As(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	conf, err := server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: dynamic(providerType, tftypes.NewValue(providerType, map[string]tftypes.Value{
		"endpoint": tftypes.NewValue(tftypes.String, hub.URL()), "admin_key": tftypes.NewValue(tftypes.String, testAdminKey),
		"audit_context": tftypes.NewValue(tftypes.String, nil),
	}))})
	noErrors("configure", err, conf.Diagnostics)

	up, err := server.UpgradeResourceState(ctx, &tfprotov6.UpgradeResourceStateRequest{TypeName: name, RawState: &tfprotov6.RawState{JSON: oldState}})
	noErrors("upgrade", err, up.Diagnostics)
	if v := attrs(up.UpgradedState)["air_gapped"]; !v.IsNull() {
		t.Fatalf("air_gapped in the loaded state = %v, want null", v)
	}

	read, err := server.ReadResource(ctx, &tfprotov6.ReadResourceRequest{TypeName: name, CurrentState: up.UpgradedState})
	noErrors("read", err, read.Diagnostics)
	prior := attrs(read.NewState)
	if !prior["air_gapped"].Equal(tftypes.NewValue(tftypes.Bool, false)) {
		t.Fatalf("air_gapped after the refresh = %v, want false", prior["air_gapped"])
	}

	// The configuration of that licence: everything not set is null.
	config := map[string]tftypes.Value{}
	for k, v := range prior {
		config[k] = tftypes.NewValue(licType.(tftypes.Object).AttributeTypes[k], nil)
		switch k {
		case "environment_id", "product", "domains", "max_users", "edition", "contract_position_id":
			config[k] = v
		}
	}
	plan, err := server.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{
		TypeName:         name,
		PriorState:       read.NewState,
		ProposedNewState: read.NewState, // Terraform keeps prior values of unset computed attributes
		Config:           dynamic(licType, tftypes.NewValue(licType, config)),
	})
	noErrors("plan", err, plan.Diagnostics)
	planned := attrs(plan.PlannedState)
	for k, v := range prior {
		if !planned[k].Equal(v) {
			t.Errorf("plan changes %s: %v -> %v", k, v, planned[k])
		}
	}
	if len(plan.RequiresReplace) != 0 {
		t.Errorf("plan replaces the licence: %v", plan.RequiresReplace)
	}
	if w := hub.writeCalls(); len(w) != 0 {
		t.Errorf("refresh and plan wrote to the Hub: %v", w)
	}
}

// PATCH carries air_gapped only when a configured value differs from state.
// Unset, the plan holds the state's value (or is unknown), so nothing is
// sent — not even when the Hub's value came from the portal.
func TestLicensePatchAirGapped(t *testing.T) {
	cases := []struct {
		name        string
		state, plan types.Bool
		want        any // nil: not sent
	}{
		{name: "configured on", state: types.BoolValue(false), plan: types.BoolValue(true), want: true},
		{name: "configured off over the Hub's true", state: types.BoolValue(true), plan: types.BoolValue(false), want: false},
		{name: "stays on (configured or unset)", state: types.BoolValue(true), plan: types.BoolValue(true)},
		{name: "stays off (configured or unset)", state: types.BoolValue(false), plan: types.BoolValue(false)},
		{name: "unset, unknown in the plan", state: types.BoolValue(true), plan: types.BoolUnknown()},
		{name: "unset, state without air_gapped", state: types.BoolNull(), plan: types.BoolNull()},
		{name: "configured off, state without air_gapped", state: types.BoolNull(), plan: types.BoolValue(false), want: false},
		{name: "configured on, state without air_gapped", state: types.BoolNull(), plan: types.BoolValue(true), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			// Everything else is equal in plan and state.
			model := func(airGapped types.Bool) licenseModel {
				none := types.SetNull(types.StringType)
				return licenseModel{Domains: none, Features: none, Entitlements: none, AirGapped: airGapped}
			}
			got := paramsPatch(context.Background(), &diags, model(tc.plan), model(tc.state))
			if diags.HasError() {
				t.Fatal(diags)
			}
			v, sent := got["air_gapped"]
			if tc.want == nil {
				if sent || len(got) != 0 {
					t.Fatalf("PATCH params = %v, want none", got)
				}
				return
			}
			if !sent || v != tc.want || len(got) != 1 {
				t.Fatalf("PATCH params = %v, want only air_gapped = %v", got, tc.want)
			}
		})
	}
}

// A configured false over the Hub's true revokes the licence files issued
// before; the plan warns about it, and only about that. An unset air_gapped
// plans the state's value and never warns.
func TestLicenseAirGapRemovedWarns(t *testing.T) {
	const want = "Switching off air-gap operation revokes licence files"
	gw := map[string]any{"id": "lic_1", "environment_id": "env_1", "product": "ai-gateway", "status": "active"}
	with := func(overrides map[string]any) map[string]any {
		out := maps.Clone(gw)
		maps.Copy(out, overrides)
		return out
	}
	on, off := with(map[string]any{"air_gapped": true}), with(map[string]any{"air_gapped": false})
	cases := []struct {
		name        string
		state, plan map[string]any
		warn        bool
	}{
		{name: "configured false over the Hub's true", state: on, plan: off, warn: true},
		{name: "configured false with a key rotation", state: with(map[string]any{"air_gapped": true, "rotation_trigger": "a"}),
			plan: with(map[string]any{"air_gapped": false, "rotation_trigger": "b"}), warn: true},
		{name: "configured true", state: off, plan: on},
		{name: "stays on (configured or unset)", state: on, plan: on},
		{name: "state without air_gapped", state: gw, plan: off},
		{name: "unknown at plan time", state: on, plan: with(map[string]any{"air_gapped": tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue)})},
		{name: "new licence for another environment", state: on, plan: with(map[string]any{"air_gapped": false, "environment_id": "env_2"})},
		{name: "licence ended in the Hub, re-issued", state: with(map[string]any{"air_gapped": true, "status": "ended", "rotation_trigger": "a"}),
			plan: with(map[string]any{"air_gapped": false, "status": "ended", "rotation_trigger": "b"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := modifyPlanValues(t, newLicenseResource(), tc.state, tc.plan)
			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected error: %v", resp.Diagnostics)
			}
			if got := warningWith(resp.Diagnostics, want); got != tc.warn {
				t.Fatalf("warning %q = %v, want %v: %v", want, got, tc.warn, resp.Diagnostics)
			}
			for _, d := range resp.Diagnostics.Warnings() {
				wd, ok := d.(diag.DiagnosticWithPath)
				if d.Summary() == want && (!ok || !wd.Path().Equal(path.Root("air_gapped"))) {
					t.Fatalf("warning not attached to air_gapped: %v", d)
				}
			}
		})
	}
}

// Create sends air_gapped only when it is configured; unset, the Hub's
// false applies and is read back. The next plan is empty either way.
func TestLicenseCreateAirGapped(t *testing.T) {
	cases := []struct {
		name     string
		value    string // HCL; "" = unset
		wantSent any    // nil = no air_gapped key in params
		want     bool
	}{
		{name: "unset", want: false},
		{name: "false", value: "false", wantSent: false, want: false},
		{name: "true", value: "true", wantSent: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newLicenseHub(t)
			useFakeHubEnv(t)
			cfg := licenseConfig(hub, gatewayLicense().with(licenseAttrs{"air_gapped": tc.value}))

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				CheckDestroy:             checkLicensesEnded(hub),
				Steps: []resource.TestStep{
					{
						Config: cfg,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(licAddr, "air_gapped", fmt.Sprint(tc.want)),
							func(s *terraform.State) error {
								params, _ := hub.lastBody("POST /api/v1/admin/licenses")["params"].(map[string]any)
								got, sent := params["air_gapped"]
								if sent != (tc.wantSent != nil) || got != tc.wantSent {
									return fmt.Errorf("POST params.air_gapped = %v (sent %v), want %v", got, sent, tc.wantSent)
								}
								if l, _ := hub.license(stateAttr(s, "id")); l.Params.AirGapped != tc.want {
									return fmt.Errorf("Hub licence air_gapped = %v, want %v", l.Params.AirGapped, tc.want)
								}
								return nil
							},
						),
					},
					{Config: cfg, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}},
				},
			})
		})
	}
}

// noWrites asserts that the step sent nothing but reads to the Hub.
func noWrites(hub *fakeHub) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if w := hub.writeCalls(); len(w) != 0 {
			return fmt.Errorf("expected no writes to the Hub, got %v", w)
		}
		return nil
	}
}

// An admin switches air-gap operation in the portal. Unset in the
// configuration, Terraform reads the switch into state and leaves it: no
// diff, no PATCH. Only a configured value is enforced — false over the Hub's
// true PATCHes it off (the plan warns, see TestLicenseAirGapRemovedWarns);
// removing the attribute again leaves the Hub's value alone.
func TestLicenseAirGappedSwitchedInPortal(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	unset := licenseConfig(hub, gatewayLicense())
	configured := func(v string) string { return licenseConfig(hub, gatewayLicense().with(licenseAttrs{"air_gapped": v})) }

	var id, key string
	switchOn := func() { hub.setAirGappedOutOfBand(id, true); hub.resetCalls() }
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{
				Config: unset,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(licAddr, "air_gapped", "false"),
					captureAttr(licAddr, "id", &id),
					captureAttr(licAddr, "license_key", &key),
				),
			},
			{
				// Portal switch, attribute unset: kept as the Hub has it.
				PreConfig:        switchOn,
				Config:           unset,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check:            resource.ComposeAggregateTestCheckFunc(resource.TestCheckResourceAttr(licAddr, "air_gapped", "true"), noWrites(hub)),
			},
			{
				// Explicit false over the Hub's true: switched off in place.
				Config: configured("false"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: append(expectUpdateInPlace(),
					plancheck.ExpectKnownValue(licAddr, tfjsonpath.New("air_gapped"), knownvalue.Bool(false)),
				)},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(licAddr, "air_gapped", "false"),
					checkPatchedOnce(hub, &id, &key, map[string]any{"params": map[string]any{"air_gapped": false}}),
				),
			},
			{
				// Explicit true matching a portal switch: adopted, no write.
				PreConfig:        switchOn,
				Config:           configured("true"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check:            resource.ComposeAggregateTestCheckFunc(resource.TestCheckResourceAttr(licAddr, "air_gapped", "true"), noWrites(hub)),
			},
			{
				// The attribute removed again: the Hub keeps true.
				PreConfig:        hub.resetCalls,
				Config:           unset,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(licAddr, "air_gapped", "true"),
					noWrites(hub),
					func(*terraform.State) error {
						if l, _ := hub.license(id); !l.Params.AirGapped {
							return fmt.Errorf("Hub licence no longer air-gapped")
						}
						return nil
					},
				),
			},
			{
				// Another change while air_gapped is unset: only that change is sent.
				Config:           licenseConfig(hub, gatewayLicense().with(licenseAttrs{"max_users": "70"})),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: expectUpdateInPlace()},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(licAddr, "air_gapped", "true"),
					checkPatchedOnce(hub, &id, &key, map[string]any{"params": map[string]any{"max_users": 70}}),
				),
			},
		},
	})
}

// A licence issued in the portal imports with the Hub's air_gapped and
// plans nothing — whether the configuration leaves it unset or says true.
func TestLicenseImportAirGapped(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	fifty, pos := int64(50), alphaPositionID
	hub.seedLicense(fakeLicense{
		LicenseID: "lic_airgap000001", TenantID: alphaTenantID, EnvironmentID: alphaEnvID, Product: "ai-gateway",
		ContractPositionID: &pos, Status: "active", Revision: 1, ValidUntil: positionEndDate,
		Params: fakeLicenseParams{Domains: []string{"gpt.alpha.example"}, MaxUsers: &fifty, MaxInstances: 5,
			EditionID: "all", Features: allGatewayFeatures, Entitlements: []string{}, AirGapped: true},
	})
	unset := licenseConfig(hub, gatewayLicense())
	configured := licenseConfig(hub, gatewayLicense().with(licenseAttrs{"air_gapped": "true"}))

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{
				Config:             unset,
				ResourceName:       licAddr,
				ImportState:        true,
				ImportStateId:      "lic_airgap000001",
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if got := states[0].Attributes["air_gapped"]; got != "true" {
						return fmt.Errorf("imported air_gapped = %q, want true", got)
					}
					return nil
				},
			},
			{
				PreConfig:        hub.resetCalls,
				Config:           unset,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check:            noWrites(hub),
			},
			{
				Config:           configured,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check:            noWrites(hub),
			},
		},
	})
}

// air_gapped = true on a product other than ai-gateway is the Hub's to
// refuse (422 air_gapped_unsupported); the provider shows the product and
// what to do, and leaves state and Hub as they were.
func TestLicenseUpdateAirGappedRejectedForOtherProducts(t *testing.T) {
	hub := newLicenseHub(t)
	useFakeHubEnv(t)
	original := licenseConfig(hub, pricesLicense())

	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy:             checkLicensesEnded(hub),
		Steps: []resource.TestStep{
			{Config: original, Check: captureAttr(licAddr, "id", &id)},
			{
				Config:      licenseConfig(hub, pricesLicense().with(licenseAttrs{"air_gapped": "true"})),
				ExpectError: regexp.MustCompile(`(?s)Update licence failed.*422 air_gapped_unsupported.*"product":"ai-prices".*only\s+for\s+ai-gateway`),
			},
			{
				Config:           original,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check: func(*terraform.State) error {
					if l, _ := hub.license(id); l.Params.AirGapped || l.Revision != 1 {
						return fmt.Errorf("Hub licence changed: air_gapped %v, revision %d", l.Params.AirGapped, l.Revision)
					}
					return nil
				},
			},
		},
	})
}
