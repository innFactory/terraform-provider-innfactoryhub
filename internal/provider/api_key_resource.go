package provider

import (
	"context"
	"net/url"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*apiKeyResource)(nil)
	_ resource.ResourceWithConfigure   = (*apiKeyResource)(nil)
	_ resource.ResourceWithImportState = (*apiKeyResource)(nil)
)

type apiKeyResource struct {
	client *httpClient
}

func newApiKeyResource() resource.Resource {
	return &apiKeyResource{}
}

type apiKeyModel struct {
	ID            types.String `tfsdk:"id"`
	Name          types.String `tfsdk:"name"`
	TenantID      types.String `tfsdk:"tenant_id"`
	Scope         types.String `tfsdk:"scope"`
	Permissions   types.Set    `tfsdk:"permissions"`
	Plaintext     types.String `tfsdk:"plaintext"`
	DisplayPrefix types.String `tfsdk:"display_prefix"`
	CreatedAt     types.String `tfsdk:"created_at"`
}

func (r *apiKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_api_key"
}

func (r *apiKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Mints an API key in innFactory Hub. The plaintext token is returned exactly once at create time — store it carefully (sensitive output recommended).",
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
				Required:    true,
				Description: "`system` (no tenant binding, can carry read/write) or `organization` (bound to a tenant_id, currently read-only).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"permissions": schema.SetAttribute{
				Required:    true,
				ElementType: types.StringType,
				Description: "Set of `read` and/or `write`. Organization-scoped keys must request only `read`.",
				PlanModifiers: []planmodifier.Set{
					// permission changes force replace — the Hub doesn't support
					// in-place permission edits (would invalidate the hash chain).
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
		},
	}
}

func (r *apiKeyResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.client = req.ProviderData.(*httpClient)
}

type apiKeyCreateBody struct {
	Name        string   `json:"name"`
	Scope       string   `json:"scope"`
	TenantID    *string  `json:"tenant_id,omitempty"`
	Permissions []string `json:"permissions"`
}

type apiKeyCreateResponse struct {
	Key struct {
		KeyID         string   `json:"key_id"`
		Name          string   `json:"name"`
		Scope         string   `json:"scope"`
		TenantID      *string  `json:"tenant_id"`
		Permissions   []string `json:"permissions"`
		DisplayPrefix string   `json:"display_prefix"`
		CreatedAt     string   `json:"created_at"`
	} `json:"key"`
	Plaintext string `json:"plaintext"`
}

type apiKeyListEntry struct {
	KeyID         string   `json:"key_id"`
	Name          string   `json:"name"`
	Scope         string   `json:"scope"`
	TenantID      *string  `json:"tenant_id"`
	Permissions   []string `json:"permissions"`
	DisplayPrefix string   `json:"display_prefix"`
	CreatedAt     string   `json:"created_at"`
	RevokedAt     *string  `json:"revoked_at"`
}

type apiKeyListResponse struct {
	Keys []apiKeyListEntry `json:"keys"`
}

func (r *apiKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan apiKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	scope := plan.Scope.ValueString()
	if scope != "system" && scope != "organization" {
		resp.Diagnostics.AddAttributeError(path.Root("scope"), "Invalid scope", "scope must be 'system' or 'organization'")
		return
	}
	var perms []string
	resp.Diagnostics.Append(plan.Permissions.ElementsAs(ctx, &perms, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if scope == "organization" {
		if plan.TenantID.IsNull() || plan.TenantID.ValueString() == "" {
			resp.Diagnostics.AddAttributeError(path.Root("tenant_id"), "tenant_id required", "Organization-scoped keys must set tenant_id.")
			return
		}
		for _, p := range perms {
			if p == "write" {
				resp.Diagnostics.AddAttributeError(path.Root("permissions"), "Read-only required", "Organization-scoped keys must not request 'write'.")
				return
			}
		}
	} else if !plan.TenantID.IsNull() && plan.TenantID.ValueString() != "" {
		resp.Diagnostics.AddAttributeError(path.Root("tenant_id"), "tenant_id forbidden", "System-scoped keys must not carry a tenant_id.")
		return
	}

	body := apiKeyCreateBody{
		Name:        plan.Name.ValueString(),
		Scope:       scope,
		Permissions: perms,
	}
	if scope == "organization" {
		t := plan.TenantID.ValueString()
		body.TenantID = &t
	}
	var env envelope[apiKeyCreateResponse]
	if err := r.client.do(ctx, "POST", "/api/v1/admin/api-keys", nil, body, &env); err != nil {
		resp.Diagnostics.AddError("Create API key failed", err.Error())
		return
	}

	plan.ID = types.StringValue(env.Data.Key.KeyID)
	plan.Plaintext = types.StringValue(env.Data.Plaintext)
	plan.DisplayPrefix = types.StringValue(env.Data.Key.DisplayPrefix)
	plan.CreatedAt = types.StringValue(env.Data.Key.CreatedAt)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *apiKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state apiKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Hub today exposes list-only; we filter client-side.
	var env envelope[apiKeyListResponse]
	if err := r.client.do(ctx, "GET", "/api/v1/admin/api-keys", nil, nil, &env); err != nil {
		resp.Diagnostics.AddError("Read API key failed", err.Error())
		return
	}
	id := state.ID.ValueString()
	for _, k := range env.Data.Keys {
		if k.KeyID != id {
			continue
		}
		if k.RevokedAt != nil {
			// Treat revoked keys as gone — terraform will plan a recreate.
			resp.State.RemoveResource(ctx)
			return
		}
		state.Name = types.StringValue(k.Name)
		state.Scope = types.StringValue(k.Scope)
		if k.TenantID != nil {
			state.TenantID = types.StringValue(*k.TenantID)
		}
		state.DisplayPrefix = types.StringValue(k.DisplayPrefix)
		state.CreatedAt = types.StringValue(k.CreatedAt)
		permVals := make([]string, 0, len(k.Permissions))
		permVals = append(permVals, k.Permissions...)
		permSet, d := types.SetValueFrom(ctx, types.StringType, permVals)
		resp.Diagnostics.Append(d...)
		state.Permissions = permSet
		resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
		return
	}
	// Not found — drop from state.
	resp.State.RemoveResource(ctx)
}

func (r *apiKeyResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Every mutable attribute on this resource is marked RequiresReplace,
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
	err := r.client.do(ctx, "POST", "/api/v1/admin/api-keys/"+url.PathEscape(id)+"/revoke", nil, struct{}{}, nil)
	if err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError("Revoke API key failed", err.Error())
	}
}

func (r *apiKeyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Imports are by key_id. Note: plaintext can NEVER be recovered on import
	// (the Hub doesn't store it). Operators who import an existing key cannot
	// then use `.plaintext` downstream — they must mint a fresh key for that.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
