package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// notBlank rejects whitespace-only values, which would otherwise pass the
// "set" checks and come back as a confusing 404 from the Hub.
var notBlank = stringvalidator.RegexMatches(regexp.MustCompile(`\S`), "must not be blank")

// hostNameExpr is an unanchored lower-case DNS name with at least two labels
// and no scheme, port, path or wildcard.
const hostNameExpr = `([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`

// hostNamePattern matches exactly a host name — what the gateway compares
// against the host of its base URL. Lower case keeps state and Hub free of
// case drift.
var hostNamePattern = regexp.MustCompile(`^` + hostNameExpr + `$`)

// knownString reports a value that is known and non-empty — i.e. one the
// user configured (validators already reject blank strings). Null, unknown
// and "" all count as "not set".
func knownString(v types.String) (string, bool) {
	if v.IsNull() || v.IsUnknown() || v.ValueString() == "" {
		return "", false
	}
	return v.ValueString(), true
}

// strList wraps types.ListValueFrom for []string with the boilerplate of
// appending the diag and returning a null list on error.
func strList(ctx context.Context, diags *diag.Diagnostics, in []string) types.List {
	v, d := types.ListValueFrom(ctx, types.StringType, in)
	diags.Append(d...)
	if d.HasError() {
		return types.ListNull(types.StringType)
	}
	return v
}

// strSet is strList for sets; nil becomes an empty set.
func strSet(ctx context.Context, diags *diag.Diagnostics, in []string) types.Set {
	if in == nil {
		in = []string{}
	}
	v, d := types.SetValueFrom(ctx, types.StringType, in)
	diags.Append(d...)
	if d.HasError() {
		return types.SetNull(types.StringType)
	}
	return v
}

// stringsOf reads a known set of strings; null or unknown yields nil, a
// known empty set an empty, non-nil slice (callers tell "omit" from "[]").
func stringsOf(ctx context.Context, diags *diag.Diagnostics, s types.Set) []string {
	if s.IsNull() || s.IsUnknown() {
		return nil
	}
	out := []string{}
	diags.Append(s.ElementsAs(ctx, &out, false)...)
	return out
}

// commonHints explain refusals any operation can meet: the admin key's
// permissions on the AI Hub.
var commonHints = map[string]string{
	"forbidden_key_scope": "The admin key's permissions do not cover this request. A key with [\"read\", \"licensing\"] manages environments, licences " +
		"(incl. key rotation), the tenant lookup and companyADMIN keys — organization, [\"read_maintenance\"], with an expires_at at most 24 months ahead — nothing else.",
	"forbidden_key_minting": "API keys cannot create system keys in the AI Hub. Create system keys in the portal (staff sign-in with TOTP); " +
		"Terraform manages organization keys only.",
	"forbidden_read_only_key": "The admin key is read-only ([\"read_licensing\"], meant for plans in pull requests, or [\"read_maintenance\"]). " +
		"Apply with a key that has [\"read\", \"licensing\"].",
	"invalid_api_key": "The Hub does not accept the admin key: it is unknown to this Hub, revoked or expired. Check INNFACTORYHUB_ADMIN_KEY and the endpoint.",
}

// withHint renders err for a diagnostic and, when the Hub answered with an
// error code listed in hints (or commonHints), appends the matching
// remediation text.
func withHint(err error, hints map[string]string) string {
	msg := err.Error()
	var ae *apiError
	if !errors.As(err, &ae) {
		return msg
	}
	if hint, ok := hints[ae.Code]; ok {
		return msg + "\n\n" + hint
	}
	if hint, ok := commonHints[ae.Code]; ok {
		return msg + "\n\n" + hint
	}
	return msg
}

// stateUnlessChanged is UseStateForUnknown with a condition: an unconfigured
// computed attribute keeps its prior value unless one of the attributes the
// Hub derives it from changes in this plan. Unlike UseStateForUnknown it
// also keeps a null prior value. deps name string attributes.
type stateUnlessChanged struct {
	deps []string
}

func useStateUnlessChanged(deps ...string) stateUnlessChanged {
	return stateUnlessChanged{deps: deps}
}

func (m stateUnlessChanged) Description(context.Context) string {
	return fmt.Sprintf("keeps the prior value unless %s changes", strings.Join(m.deps, " or "))
}

func (m stateUnlessChanged) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

// depsUnchanged reports an update (not a create) in which no dependency
// changes.
func (m stateUnlessChanged) depsUnchanged(ctx context.Context, state tfsdk.State, plan tfsdk.Plan, diags *diag.Diagnostics) bool {
	if state.Raw.IsNull() {
		return false
	}
	for _, dep := range m.deps {
		var before, after types.String
		diags.Append(state.GetAttribute(ctx, path.Root(dep), &before)...)
		diags.Append(plan.GetAttribute(ctx, path.Root(dep), &after)...)
		if diags.HasError() || !before.Equal(after) {
			return false
		}
	}
	return true
}

func (m stateUnlessChanged) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.ConfigValue.IsNull() && req.PlanValue.IsUnknown() && m.depsUnchanged(ctx, req.State, req.Plan, &resp.Diagnostics) {
		resp.PlanValue = req.StateValue
	}
}

func (m stateUnlessChanged) PlanModifySet(ctx context.Context, req planmodifier.SetRequest, resp *planmodifier.SetResponse) {
	if req.ConfigValue.IsNull() && req.PlanValue.IsUnknown() && m.depsUnchanged(ctx, req.State, req.Plan, &resp.Diagnostics) {
		resp.PlanValue = req.StateValue
	}
}
