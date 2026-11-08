package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// modifyPlan runs a resource's ModifyPlan for an update from state to plan;
// both are given as string attributes, everything else is null.
func modifyPlan(t *testing.T, r fwresource.Resource, state, plan map[string]string) fwresource.ModifyPlanResponse {
	t.Helper()
	ctx := context.Background()
	var schemaResp fwresource.SchemaResponse
	r.Schema(ctx, fwresource.SchemaRequest{}, &schemaResp)
	objType := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	raw := func(attrs map[string]string) tftypes.Value {
		vals := map[string]tftypes.Value{}
		for name, typ := range objType.AttributeTypes {
			vals[name] = tftypes.NewValue(typ, nil)
		}
		for name, v := range attrs {
			vals[name] = tftypes.NewValue(tftypes.String, v)
		}
		return tftypes.NewValue(objType, vals)
	}
	m, ok := r.(fwresource.ResourceWithModifyPlan)
	if !ok {
		t.Fatal("resource has no ModifyPlan")
	}
	planned := tfsdk.Plan{Schema: schemaResp.Schema, Raw: raw(plan)}
	resp := fwresource.ModifyPlanResponse{Plan: planned}
	m.ModifyPlan(ctx, fwresource.ModifyPlanRequest{
		State: tfsdk.State{Schema: schemaResp.Schema, Raw: raw(state)},
		Plan:  planned,
	}, &resp)
	return resp
}

func warningWith(diags diag.Diagnostics, text string) bool {
	for _, d := range diags.Warnings() {
		if strings.Contains(d.Summary()+" "+d.Detail(), text) {
			return true
		}
	}
	return false
}

// Re-issuing after a revocation in the Hub is legitimate, but it must not
// hide behind the generic replace marker (P-5).
func TestReissueAfterHubRevocationWarns(t *testing.T) {
	cases := []struct {
		name        string
		r           fwresource.Resource
		state, plan map[string]string
		wantWarn    string
	}{
		{name: "licence ended in the Hub", r: newLicenseResource(),
			state:    map[string]string{"id": "lic_1", "environment_id": "env_1", "status": "ended", "rotation_trigger": "a"},
			plan:     map[string]string{"id": "lic_1", "environment_id": "env_1", "status": "ended", "rotation_trigger": "b"},
			wantWarn: "Re-issuing a licence that was ended in the AI Hub"},
		{name: "API key revoked in the Hub", r: newApiKeyResource(),
			state:    map[string]string{"id": "KEY1", "name": "k", "status": "revoked"},
			plan:     map[string]string{"id": "KEY1", "name": "k", "status": "revoked", "rotation_trigger": "x"},
			wantWarn: "Re-minting an API key that was revoked in the Hub"},
		{name: "API key expired", r: newApiKeyResource(),
			state:    map[string]string{"id": "KEY1", "name": "k", "status": "expired", "rotation_trigger": "x"},
			plan:     map[string]string{"id": "KEY1", "name": "k", "status": "expired", "rotation_trigger": "y"},
			wantWarn: "Re-minting an API key that expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := modifyPlan(t, tc.r, tc.state, tc.plan)
			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected error: %v", resp.Diagnostics)
			}
			if !warningWith(resp.Diagnostics, tc.wantWarn) {
				t.Fatalf("missing warning %q: %v", tc.wantWarn, resp.Diagnostics)
			}
		})
	}
}

// Ordinary rotations and replacements do not warn.
func TestOrdinaryRotationDoesNotWarn(t *testing.T) {
	cases := []struct {
		name        string
		r           fwresource.Resource
		state, plan map[string]string
	}{
		{name: "licence key rotation", r: newLicenseResource(),
			state: map[string]string{"id": "lic_1", "status": "active", "rotation_trigger": "a"},
			plan:  map[string]string{"id": "lic_1", "status": "active", "rotation_trigger": "b"}},
		{name: "active API key rotation", r: newApiKeyResource(),
			state: map[string]string{"id": "KEY1", "name": "k", "status": "active"},
			plan:  map[string]string{"id": "KEY1", "name": "k", "status": "active", "rotation_trigger": "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := modifyPlan(t, tc.r, tc.state, tc.plan)
			if resp.Diagnostics.HasError() || len(resp.Diagnostics.Warnings()) != 0 {
				t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
			}
		})
	}
	// The ended licence is replaced through rotation_trigger.
	resp := modifyPlan(t, newLicenseResource(),
		map[string]string{"id": "lic_1", "status": "ended", "rotation_trigger": "a"},
		map[string]string{"id": "lic_1", "status": "ended", "rotation_trigger": "b"})
	if !resp.RequiresReplace.Contains(path.Root("rotation_trigger")) {
		t.Fatalf("RequiresReplace = %v", resp.RequiresReplace)
	}
}
