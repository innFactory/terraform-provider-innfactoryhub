package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const keyAddr = "innfactoryhub_api_key.k"

// maintenanceKeyConfig is the companyADMIN key: an organization key that
// reads only the maintenance status.
func maintenanceKeyConfig(provider, extra string) string {
	return provider + fmt.Sprintf(`
resource "innfactoryhub_api_key" "k" {
  name        = "companyADMIN Alpha"
  scope       = "organization"
  tenant_id   = %q
  permissions = ["read_maintenance"]
%s}
`, alphaTenantID, extra)
}

func keyStateAttr(s *terraform.State, attr string) string {
	rs, ok := s.RootModule().Resources[keyAddr]
	if !ok {
		return ""
	}
	return rs.Primary.Attributes[attr]
}

// A key revoked, deleted or expired in the Hub is not minted again behind
// the operator's back (SEC-006): it stays in state with its status, and
// plans fail until rotation_trigger changes, which mints a new key.
func TestAPIKeyHubRevocationIsNotUndone(t *testing.T) {
	cases := []struct {
		name    string
		act     func(hub *fakeHub, id string)
		status  string
		wantErr string
	}{
		{name: "revoked in the portal", act: (*fakeHub).revokeAPIKeyOutOfBand, status: "revoked",
			wantErr: `(?s)API key revoked in the Hub.*change rotation_trigger to mint a new one`},
		{name: "deleted in the portal", act: func(h *fakeHub, id string) { h.revokeAPIKeyOutOfBand(id); h.deleteAPIKeyOutOfBand(id) },
			status: "deleted", wantErr: `(?s)API key deleted in the Hub.*change rotation_trigger to mint a new one`},
		{name: "expired", act: (*fakeHub).expireAPIKeyOutOfBand, status: "expired",
			wantErr: `(?s)API key expired.*change rotation_trigger to mint a new one`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeHub(t)
			useFakeHubEnv(t)
			cfg := maintenanceKeyConfig(hub.providerBlock(), "")

			var id, plaintext string
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				Steps: []resource.TestStep{
					{
						Config: cfg,
						Check: resource.ComposeAggregateTestCheckFunc(
							captureAttr(keyAddr, "id", &id),
							captureAttr(keyAddr, "plaintext", &plaintext),
							resource.TestCheckResourceAttr(keyAddr, "status", "active"),
						),
					},
					{
						PreConfig: func() {
							tc.act(hub, id)
							hub.resetCalls()
						},
						Config:      cfg,
						PlanOnly:    true,
						ExpectError: regexp.MustCompile(tc.wantErr),
					},
					{
						PreConfig: func() {
							if w := hub.writeCalls(); len(w) != 0 {
								t.Errorf("a refused plan wrote to the Hub: %v", w)
							}
						},
						Config: maintenanceKeyConfig(hub.providerBlock(), `  rotation_trigger = "2026-11"`+"\n"),
						ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction(keyAddr, plancheck.ResourceActionDestroyBeforeCreate),
							expectPriorState(keyAddr, map[string]string{"status": tc.status}, func(a map[string]any) error {
								if a["id"] != id || a["plaintext"] != plaintext {
									return fmt.Errorf("the key was not kept in state")
								}
								return nil
							}),
						}},
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(keyAddr, "status", "active"),
							resource.TestCheckResourceAttr(keyAddr, "rotation_trigger", "2026-11"),
							func(s *terraform.State) error {
								if keyStateAttr(s, "id") == id || keyStateAttr(s, "plaintext") == plaintext {
									return fmt.Errorf("expected a newly minted key")
								}
								return nil
							},
						),
					},
				},
			})
		})
	}
}

// rotation_trigger mints a new key on purpose: the old one is revoked first.
func TestAPIKeyRotationTriggerMintsNewKey(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)

	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		CheckDestroy: func(*terraform.State) error {
			if ids := hub.apiKeyIDs(); len(ids) != 0 {
				return fmt.Errorf("keys still active after destroy: %v", ids)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{Config: maintenanceKeyConfig(hub.providerBlock(), ""), Check: captureAttr(keyAddr, "id", &id)},
			{
				Config: maintenanceKeyConfig(hub.providerBlock(), `  rotation_trigger = "2026-11"`+"\n"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(keyAddr, plancheck.ResourceActionDestroyBeforeCreate),
				}},
				Check: func(s *terraform.State) error {
					if old, _ := hub.apiKey(id); old.RevokedAt == nil {
						return fmt.Errorf("previous key %s not revoked", id)
					}
					if ids := hub.apiKeyIDs(); len(ids) != 1 || ids[0] != keyStateAttr(s, "id") {
						return fmt.Errorf("active keys %v, state %s", ids, keyStateAttr(s, "id"))
					}
					return nil
				},
			},
			{
				Config:           maintenanceKeyConfig(hub.providerBlock(), `  rotation_trigger = "2026-11"`+"\n"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
		},
	})
}

// expires_at is sent on create in UTC, kept as configured (another notation
// of the same instant is no diff), read back from the Hub, and a new value
// mints a new key.
func TestAPIKeyExpiresAt(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)
	in := time.Now().Add(400 * 24 * time.Hour).Truncate(time.Second)
	cest := time.FixedZone("CEST", 2*3600)
	configured := in.In(cest).Format(time.RFC3339)
	later := in.Add(24 * time.Hour).UTC().Format(time.RFC3339)
	withExpiry := func(v string) string {
		return maintenanceKeyConfig(hub.providerBlock(), fmt.Sprintf("  expires_at = %q\n", v))
	}

	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: withExpiry(configured),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(keyAddr, "id", &id),
					resource.TestCheckResourceAttr(keyAddr, "expires_at", configured),
					func(*terraform.State) error {
						body := hub.lastBody("POST /api/v1/admin/api-keys")
						if want := in.UTC().Format(time.RFC3339); body["expires_at"] != want {
							return fmt.Errorf("sent expires_at %v, want %s (UTC)", body["expires_at"], want)
						}
						k, _ := hub.apiKey(id)
						if k.ExpiresAt == nil || *k.ExpiresAt == configured {
							return fmt.Errorf("the fake must answer in its own notation, got %v", k.ExpiresAt)
						}
						return nil
					},
				),
			},
			{
				Config:           withExpiry(configured),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
			{
				// Removing it keeps the key and its expiry.
				Config:           maintenanceKeyConfig(hub.providerBlock(), ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
			{
				Config: withExpiry(later),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(keyAddr, plancheck.ResourceActionDestroyBeforeCreate),
				}},
				Check: resource.TestCheckResourceAttr(keyAddr, "expires_at", later),
			},
			{
				Config:                  withExpiry(later),
				ResourceName:            keyAddr,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"plaintext", "expires_at"},
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					got, err := time.Parse(time.RFC3339, states[0].Attributes["expires_at"])
					if err != nil || !got.Equal(in.Add(24*time.Hour)) {
						return fmt.Errorf("imported expires_at %q, want the instant %s", states[0].Attributes["expires_at"], later)
					}
					return nil
				},
			},
		},
	})
}

// The Hub's rules on scopes and permissions are checked at plan time, before
// any Hub call.
func TestAPIKeyPermissionValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "licensing on an organization key", body: `scope = "organization"
  tenant_id = "tnt_alpha"
  permissions = ["read", "licensing"]`, wantErr: `(?s)Error: System-scope permission.*"licensing" is a system-scope permission`},
		{name: "read_licensing on an organization key", body: `scope = "organization"
  tenant_id = "tnt_alpha"
  permissions = ["read_licensing"]`, wantErr: `(?s)Error: System-scope permission.*"read_licensing" is a system-scope permission`},
		{name: "read_maintenance on a system key", body: `scope = "system"
  permissions = ["read_maintenance"]`, wantErr: `(?s)Error: Organization-scope permission.*read_maintenance is an organization-scope`},
		{name: "write on an organization key", body: `scope = "organization"
  tenant_id = "tnt_alpha"
  permissions = ["write"]`, wantErr: `(?s)Error: System-scope permission.*"write" is a system-scope permission`},
		{name: "organization key without tenant", body: `scope = "organization"
  permissions = ["read_maintenance"]`, wantErr: `(?s)Error: tenant_id required`},
		{name: "system key with tenant", body: `scope = "system"
  tenant_id = "tnt_alpha"
  permissions = ["read_licensing"]`, wantErr: `(?s)Error: tenant_id forbidden`},
		{name: "read_licensing with other permissions", body: `scope = "system"
  permissions = ["read", "read_licensing"]`, wantErr: `(?s)Error: Invalid permission combination.*read_licensing stands alone`},
		{name: "licensing without read", body: `scope = "system"
  permissions = ["licensing"]`, wantErr: `(?s)Error: Invalid permission combination.*licensing needs read`},
		{name: "licensing with write", body: `scope = "system"
  permissions = ["read", "write", "licensing"]`, wantErr: `(?s)Error: Invalid permission combination.*licensing and write exclude each other`},
		{name: "unknown permission", body: `scope = "organization"
  tenant_id = "tnt_alpha"
  permissions = ["admin"]`, wantErr: `(?s)Error: Invalid Attribute Value Match.*permissions.*value must be one of`},
		{name: "unknown scope", body: `scope = "tenant"
  permissions = ["read"]`, wantErr: `(?s)Error: Invalid Attribute Value Match.*scope.*value must be one of`},
		{name: "no permission", body: `scope = "system"
  permissions = []`, wantErr: `(?s)Error: Invalid Attribute Value.*permissions.*at least 1`},
		{name: "expires_at not RFC 3339", body: `scope = "organization"
  tenant_id = "tnt_alpha"
  permissions = ["read_maintenance"]
  expires_at = "2027-01-01"`, wantErr: `(?s)Error: Invalid timestamp.*RFC 3339`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeHub(t)
			useFakeHubEnv(t)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				Steps: []resource.TestStep{{
					Config: hub.providerBlock() + `
resource "innfactoryhub_api_key" "k" {
  name = "validation"
  ` + tc.body + `
}
`,
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

// read next to read_maintenance lifts the restriction; the provider warns.
func TestAPIKeyWarnsAboutReadWithReadMaintenance(t *testing.T) {
	cases := []struct {
		perms    []string
		wantWarn bool
	}{
		{perms: []string{"read", "read_maintenance"}, wantWarn: true},
		{perms: []string{"read_maintenance"}},
		{perms: []string{"read"}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.perms, "+"), func(t *testing.T) {
			diags := validateAPIKeyConfig(t, "organization", alphaTenantID, tc.perms)
			if diags.HasError() {
				t.Fatalf("unexpected errors: %v", diags)
			}
			var warned bool
			for _, d := range diags.Warnings() {
				warned = warned || strings.Contains(d.Detail(), "read_maintenance")
			}
			if warned != tc.wantWarn {
				t.Fatalf("warning = %v, want %v (%v)", warned, tc.wantWarn, diags)
			}
		})
	}
}

// validateAPIKeyConfig runs the resource's config validation directly.
func validateAPIKeyConfig(t *testing.T, scope, tenant string, perms []string) diag.Diagnostics {
	t.Helper()
	ctx := context.Background()
	r := newApiKeyResource()
	var schemaResp fwresource.SchemaResponse
	r.Schema(ctx, fwresource.SchemaRequest{}, &schemaResp)
	objType := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)

	vals := map[string]tftypes.Value{}
	for name, typ := range objType.AttributeTypes {
		vals[name] = tftypes.NewValue(typ, nil)
	}
	permVals := make([]tftypes.Value, 0, len(perms))
	for _, p := range perms {
		permVals = append(permVals, tftypes.NewValue(tftypes.String, p))
	}
	vals["name"] = tftypes.NewValue(tftypes.String, "validation")
	vals["scope"] = tftypes.NewValue(tftypes.String, scope)
	vals["tenant_id"] = tftypes.NewValue(tftypes.String, tenant)
	vals["permissions"] = tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, permVals)

	v, ok := r.(fwresource.ResourceWithValidateConfig)
	if !ok {
		t.Fatal("innfactoryhub_api_key has no config validation")
	}
	resp := fwresource.ValidateConfigResponse{}
	v.ValidateConfig(ctx, fwresource.ValidateConfigRequest{
		Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: tftypes.NewValue(objType, vals)},
	}, &resp)
	return resp.Diagnostics
}
