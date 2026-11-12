package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*apiKeyResource)(nil)
	_ resource.ResourceWithConfigure      = (*apiKeyResource)(nil)
	_ resource.ResourceWithImportState    = (*apiKeyResource)(nil)
	_ resource.ResourceWithValidateConfig = (*apiKeyResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*apiKeyResource)(nil)
)

const apiKeysPath = "/api/v1/admin/api-keys"

type apiKeyResource struct {
	client *httpClient
}

func newApiKeyResource() resource.Resource {
	return &apiKeyResource{}
}

type apiKeyModel struct {
	ID              types.String `tfsdk:"id"`
	Name            types.String `tfsdk:"name"`
	TenantID        types.String `tfsdk:"tenant_id"`
	Scope           types.String `tfsdk:"scope"`
	Permissions     types.Set    `tfsdk:"permissions"`
	ExpiresAt       types.String `tfsdk:"expires_at"`
	RotationTrigger types.String `tfsdk:"rotation_trigger"`
	Plaintext       types.String `tfsdk:"plaintext"`
	DisplayPrefix   types.String `tfsdk:"display_prefix"`
	CreatedAt       types.String `tfsdk:"created_at"`
	Status          types.String `tfsdk:"status"`
}

const (
	scopeSystem       = "system"
	scopeOrganization = "organization"
)

// Values of status. Anything but active is a decision of the Hub (or the
// key's expiry) that Terraform does not undo on its own.
const (
	apiKeyActive  = "active"
	apiKeyRevoked = "revoked"
	apiKeyExpired = "expired"
	apiKeyDeleted = "deleted"
)

// apiKeyPermissions are the permissions of both hubs: innfactoryHUB knows
// read, read_accounting and write; the AI Hub adds licensing and
// read_licensing (system keys for Terraform) and read_maintenance
// (organization keys for companyADMIN).
var apiKeyPermissions = []string{"read", "read_accounting", "write", "licensing", "read_licensing", "read_maintenance"}

// systemOnlyPermissions may not be granted to organization keys.
var systemOnlyPermissions = []string{"write", "read_accounting", "licensing", "read_licensing"}

func (r *apiKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_api_key"
}

func (r *apiKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Mints an API key in innFactory Hub or the innFactory AI Hub. The plaintext token is returned exactly once at create time — store it carefully (sensitive output recommended). " +
			"A key revoked, deleted or expired in the Hub stays so: Terraform keeps it in state with its `status`, and plans fail until `rotation_trigger` changes.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Stable Hub-side key identifier (the `<id>` portion of `ak_<id>.<secret>`).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Human-readable name. Shown in the Hub admin UI; not used for uniqueness — multiple keys with the same name per tenant are allowed.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"tenant_id": schema.StringAttribute{
				Optional:    true,
				Description: "Tenant to scope the key to. Required for `scope = organization`; must be empty for `scope = system`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"scope": schema.StringAttribute{
				Required: true,
				Description: "`system` (no tenant binding) or `organization` (bound to `tenant_id`, read-only). " +
					"The AI Hub creates system keys only for staff signed in with TOTP, never for an API key.",
				Validators: []validator.String{stringvalidator.OneOf(scopeSystem, scopeOrganization)},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"permissions": schema.SetAttribute{
				Required:    true,
				ElementType: types.StringType,
				Description: "Permissions. Organization keys: `read` (everything a customer user of the tenant sees) and/or `read_maintenance` " +
					"(AI Hub: only `GET /customer/maintenance`, for companyADMIN — `read` next to it lifts that restriction). " +
					"System keys: `read`, `read_accounting`, `write`; on the AI Hub also `[\"read\", \"licensing\"]` (Terraform applies) and `[\"read_licensing\"]` alone (plans). Changing it forces a new key.",
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueStringsAre(stringvalidator.OneOf(apiKeyPermissions...)),
				},
				PlanModifiers: []planmodifier.Set{
					// permission changes force replace — the Hub doesn't support
					// in-place permission edits (would invalidate the hash chain).
					setplanmodifier.RequiresReplace(),
				},
			},
			"expires_at": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "When the key expires (RFC 3339, e.g. `2027-06-30T00:00:00Z`; sent in UTC). The AI Hub allows at most 24 months for organization keys " +
					"and gives system keys at most (and by default) 12 months. Without it the Hub's value is recorded. Changing it forces a new key; " +
					"removing it keeps the key. An expired key is treated like a revoked one.",
				Validators: []validator.String{rfc3339{}},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplace(),
				},
			},
			"rotation_trigger": schema.StringAttribute{
				Optional: true,
				Description: "Mints a new key: any change (including setting or removing it) revokes this key and creates a new one. " +
					"The only way to replace a key the Hub revoked, deleted or let expire.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"plaintext": schema.StringAttribute{
				Computed:    true,
				Sensitive:   true,
				Description: "The full `ak_<id>.<secret>` token. Returned only at create time; reading state after `terraform apply` is the only way to retrieve it again.",
			},
			"display_prefix": schema.StringAttribute{
				Computed:    true,
				Description: "First 8 characters of the plaintext (`ak_xxxxx…`), shown in the Hub UI listing.",
			},
			"created_at": schema.StringAttribute{
				Computed:    true,
				Description: "RFC-3339 timestamp the key was minted.",
			},
			"status": schema.StringAttribute{
				Computed: true,
				Description: "`active`, `revoked` (revoked in the Hub), `expired` (past `expires_at`) or `deleted` (no longer listed by the Hub — revoked and deleted, " +
					"or not visible to the admin key: a `licensing` key lists organization keys only). For anything but `active`, plans fail until `rotation_trigger` changes.",
			},
		},
	}
}

func (r *apiKeyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*httpClient)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("expected *httpClient, got %T", req.ProviderData))
		return
	}
	r.client = client
}

// rfc3339 accepts a timestamp in RFC 3339 form.
type rfc3339 struct{}

func (rfc3339) Description(context.Context) string {
	return "must be an RFC 3339 timestamp such as 2027-06-30T00:00:00Z"
}

func (v rfc3339) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v rfc3339) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := time.Parse(time.RFC3339, req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid timestamp",
			fmt.Sprintf("%s, got %q", v.Description(ctx), req.ConfigValue.ValueString()))
	}
}

// ValidateConfig applies the Hub's scope and permission rules at plan time.
// Unknown values defer the affected checks.
func (r *apiKeyResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg apiKeyModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() || cfg.Scope.IsUnknown() {
		return
	}
	perms := knownElements(cfg.Permissions)
	hasTenant := cfg.TenantID.IsUnknown() || cfg.TenantID.ValueString() != ""
	permissions, tenant := path.Root("permissions"), path.Root("tenant_id")

	switch cfg.Scope.ValueString() {
	case scopeOrganization:
		if !hasTenant {
			resp.Diagnostics.AddAttributeError(tenant, "tenant_id required", "Organization-scoped keys must set tenant_id.")
		}
		for _, p := range systemOnlyPermissions {
			if slices.Contains(perms, p) {
				resp.Diagnostics.AddAttributeError(permissions, "System-scope permission",
					fmt.Sprintf("%q is a system-scope permission; organization keys may hold read and/or read_maintenance.", p))
			}
		}
		if slices.Contains(perms, "read") && slices.Contains(perms, "read_maintenance") {
			resp.Diagnostics.AddAttributeWarning(permissions, "read lifts the read_maintenance restriction",
				"With read next to read_maintenance the key reads everything a customer user of the tenant sees (contracts, invoices, licences, documents), "+
					"not just the maintenance status. For companyADMIN use [\"read_maintenance\"] alone.")
		}
	case scopeSystem:
		if hasTenant {
			resp.Diagnostics.AddAttributeError(tenant, "tenant_id forbidden", "System-scoped keys must not carry a tenant_id.")
		}
		if slices.Contains(perms, "read_maintenance") {
			resp.Diagnostics.AddAttributeError(permissions, "Organization-scope permission",
				"read_maintenance is an organization-scope permission (companyADMIN); system keys use read, write, licensing or read_licensing.")
		}
		if detail := licensingCombination(perms); detail != "" {
			resp.Diagnostics.AddAttributeError(permissions, "Invalid permission combination", detail)
		}
	}
}

// licensingCombination checks the AI Hub's rules for the Terraform keys:
// ["read", "licensing"] to apply, ["read_licensing"] alone to plan.
func licensingCombination(perms []string) string {
	has := func(p string) bool { return slices.Contains(perms, p) }
	switch {
	case has("read_licensing") && len(perms) > 1:
		return "read_licensing stands alone: it is the read-only key for plans and carries no other permission."
	case has("licensing") && has("write"):
		return "licensing and write exclude each other (write already grants everything); Terraform applies use [\"read\", \"licensing\"]."
	case has("licensing") && !has("read"):
		return "licensing needs read: use [\"read\", \"licensing\"]."
	}
	return ""
}

// knownElements lists the known strings of a set (unknown elements skipped).
func knownElements(s types.Set) []string {
	var out []string
	for _, e := range s.Elements() {
		if v, ok := e.(types.String); ok && !v.IsUnknown() && !v.IsNull() {
			out = append(out, v.ValueString())
		}
	}
	return out
}

// ModifyPlan keeps the Hub's revocations: a key revoked, deleted or expired
// is not minted again unless rotation_trigger changes (which replaces it via
// RequiresReplace).
func (r *apiKeyResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return // create or destroy
	}
	var state, plan apiKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(resp.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	status := state.Status.ValueString()
	if status == "" || status == apiKeyActive {
		return
	}
	what := map[string]string{
		apiKeyRevoked: "revoked in the Hub",
		apiKeyExpired: "expired",
		apiKeyDeleted: "deleted in the Hub",
	}[status]
	if !plan.RotationTrigger.Equal(state.RotationTrigger) {
		summary := "Re-minting an API key that expired"
		if status != apiKeyExpired {
			summary = "Re-minting an API key that was " + what
		}
		resp.Diagnostics.AddAttributeWarning(path.Root("rotation_trigger"), summary,
			fmt.Sprintf("Key %s (%q) is %s. rotation_trigger changed, so this apply mints a new key with the same permissions. "+
				"Make sure the key was not revoked on purpose.", state.ID.ValueString(), state.Name.ValueString(), status))
		return
	}
	resp.Diagnostics.AddAttributeError(path.Root("rotation_trigger"),
		fmt.Sprintf("API key %s – change rotation_trigger to mint a new one", what),
		fmt.Sprintf("Key %s (%q) is %s. Terraform does not mint a replacement on its own, so a revocation in the Hub stays effective. "+
			"To mint a new key, change rotation_trigger; to retire it, remove the resource from the configuration. "+
			"Status deleted can also mean the admin key cannot see the key: a licensing key lists organization keys only.",
			state.ID.ValueString(), state.Name.ValueString(), status))
}

type apiKeyCreateBody struct {
	Name        string   `json:"name"`
	Scope       string   `json:"scope"`
	TenantID    *string  `json:"tenant_id,omitempty"`
	Permissions []string `json:"permissions"`
	ExpiresAt   string   `json:"expires_at,omitempty"`
}

// apiKeyDTO is ApiKey of both hubs, without the fields the provider ignores.
type apiKeyDTO struct {
	KeyID         string   `json:"key_id"`
	Name          string   `json:"name"`
	Scope         string   `json:"scope"`
	TenantID      *string  `json:"tenant_id"`
	Permissions   []string `json:"permissions"`
	DisplayPrefix string   `json:"display_prefix"`
	CreatedAt     string   `json:"created_at"`
	RevokedAt     *string  `json:"revoked_at"`
	ExpiresAt     *string  `json:"expires_at"`
}

type apiKeyCreateResponse struct {
	Key       apiKeyDTO `json:"key"`
	Plaintext string    `json:"plaintext"`
}

type apiKeyListResponse struct {
	Keys []apiKeyDTO `json:"keys"`
}

// apiKeyHints tell the operator how to resolve the Hub's rejections of a
// key; the key-permission refusals are in commonHints.
var apiKeyHints = map[string]string{
	"forbidden_org_write":     "Organization keys are read-only: use read and/or read_maintenance.",
	"forbidden_org_licensing": "licensing and read_licensing are system-scope permissions.",
}

func (r *apiKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan apiKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := apiKeyCreateBody{
		Name:        plan.Name.ValueString(),
		Scope:       plan.Scope.ValueString(),
		Permissions: stringsOf(ctx, &resp.Diagnostics, plan.Permissions),
	}
	if t, ok := knownString(plan.TenantID); ok {
		body.TenantID = &t
	}
	// Both hubs take UTC; innfactoryHUB accepts no offset.
	if at, ok := parseTimestamp(plan.ExpiresAt); ok {
		body.ExpiresAt = at.UTC().Format(time.RFC3339Nano)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	var env envelope[apiKeyCreateResponse]
	if err := r.client.do(ctx, http.MethodPost, apiKeysPath, nil, body, &env); err != nil {
		resp.Diagnostics.AddError("Create API key failed", withHint(err, apiKeyHints))
		return
	}

	k := env.Data.Key
	plan.ID = types.StringValue(k.KeyID)
	plan.Plaintext = types.StringValue(env.Data.Plaintext)
	plan.DisplayPrefix = types.StringValue(k.DisplayPrefix)
	plan.CreatedAt = types.StringValue(k.CreatedAt)
	plan.ExpiresAt = sameInstant(plan.ExpiresAt, k.ExpiresAt)
	plan.Status = types.StringValue(statusOf(k, time.Now()))
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *apiKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state apiKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Both hubs list keys only; filter client-side.
	var env envelope[apiKeyListResponse]
	if err := r.client.do(ctx, http.MethodGet, apiKeysPath, nil, nil, &env); err != nil {
		resp.Diagnostics.AddError("Read API key failed", withHint(err, apiKeyHints))
		return
	}
	id := state.ID.ValueString()
	i := slices.IndexFunc(env.Data.Keys, func(k apiKeyDTO) bool { return k.KeyID == id })
	if i < 0 {
		if state.CreatedAt.IsNull() {
			// Import of an id the Hub does not know.
			resp.State.RemoveResource(ctx)
			return
		}
		// Gone from the Hub (deleted after revocation): keep it, so the next
		// apply does not mint a replacement (SEC-006).
		state.Status = types.StringValue(apiKeyDeleted)
		resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
		return
	}

	k := env.Data.Keys[i]
	if k.Scope != scopeSystem && k.Scope != scopeOrganization {
		resp.Diagnostics.AddWarning(
			"Unknown scope from Hub",
			"Key "+k.KeyID+" has scope=\""+k.Scope+"\" — provider only recognises system|organization. Bump the provider version.",
		)
	}
	state.Name = types.StringValue(k.Name)
	state.Scope = types.StringValue(k.Scope)
	if k.TenantID != nil {
		state.TenantID = types.StringValue(*k.TenantID)
	}
	state.DisplayPrefix = types.StringValue(k.DisplayPrefix)
	state.CreatedAt = types.StringValue(k.CreatedAt)
	state.Permissions = strSet(ctx, &resp.Diagnostics, k.Permissions)
	state.ExpiresAt = sameInstant(state.ExpiresAt, k.ExpiresAt)
	state.Status = types.StringValue(statusOf(k, time.Now()))
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

// statusOf derives the status of a listed key; revocation wins over expiry.
func statusOf(k apiKeyDTO, now time.Time) string {
	if k.RevokedAt != nil {
		return apiKeyRevoked
	}
	if k.ExpiresAt != nil {
		if at, err := time.Parse(time.RFC3339Nano, *k.ExpiresAt); err == nil && !at.After(now) {
			return apiKeyExpired
		}
	}
	return apiKeyActive
}

// parseTimestamp reads a known RFC 3339 value.
func parseTimestamp(v types.String) (time.Time, bool) {
	s, ok := knownString(v)
	if !ok {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, s)
	return at, err == nil
}

// sameInstant returns the Hub's timestamp, unless the current value names
// the same instant in another notation (offset, no milliseconds) — then the
// current value stays and no diff appears.
func sameInstant(current types.String, hub *string) types.String {
	if hub == nil {
		return types.StringNull()
	}
	if cur, ok := parseTimestamp(current); ok {
		if at, err := time.Parse(time.RFC3339Nano, *hub); err == nil && at.Equal(cur) {
			return current
		}
	}
	return types.StringValue(*hub)
}

func (r *apiKeyResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Every configurable attribute on this resource is marked RequiresReplace,
	// so Update should never be invoked. Surface as a defensive error.
	resp.Diagnostics.AddError("Update not supported", "All updatable attributes on innfactoryhub_api_key require replacement; this code path is unreachable.")
}

func (r *apiKeyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state apiKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	if id == "" {
		return
	}
	// Revoking is idempotent in both hubs; a key already gone counts as revoked.
	err := r.client.do(ctx, http.MethodPost, apiKeysPath+"/"+url.PathEscape(id)+"/revoke", nil, struct{}{}, nil)
	if err != nil && !isHubNotFound(err, codeAPIKeyNotFound) {
		resp.Diagnostics.AddError("Revoke API key failed", withHint(err, apiKeyHints))
	}
}

func (r *apiKeyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Imports are by key_id. Plaintext can NEVER be recovered on import (the
	// Hub doesn't store it). Set it to null explicitly so any downstream
	// `output { value = innfactoryhub_api_key.foo.plaintext }` is obviously
	// null instead of an unknown that drifts at next plan.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("plaintext"), types.StringNull())...)
	resp.Diagnostics.AddWarning(
		"Plaintext not recoverable on import",
		"The Hub does not store the plaintext token. .plaintext is null after import; mint a fresh key with this provider if a downstream output needs the token.",
	)
}
