package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                     = (*environmentResource)(nil)
	_ resource.ResourceWithConfigure        = (*environmentResource)(nil)
	_ resource.ResourceWithConfigValidators = (*environmentResource)(nil)
	_ resource.ResourceWithImportState      = (*environmentResource)(nil)
)

// environmentsPath is served by the AI Hub's licence API behind
// hub.innfactory.ai (same envelope and error shape as the admin API).
const environmentsPath = "/api/v1/admin/environments"

// environmentResource registers a customer installation (CompanyGPT /
// AI Gateway) in the Hub. The Hub resolves the customer from exactly one
// cloud identifier — or takes tenant_id directly — at create time.
type environmentResource struct {
	client *httpClient
}

func newEnvironmentResource() resource.Resource {
	return &environmentResource{}
}

type environmentModel struct {
	ID                  types.String `tfsdk:"id"`
	Name                types.String `tfsdk:"name"`
	Domain              types.String `tfsdk:"domain"`
	Cloud               types.String `tfsdk:"cloud"`
	TenantID            types.String `tfsdk:"tenant_id"`
	AzureTenantID       types.String `tfsdk:"azure_tenant_id"`
	AzureTenantDomain   types.String `tfsdk:"azure_tenant_domain"`
	AzureSubscriptionID types.String `tfsdk:"azure_subscription_id"`
	GcpProjectID        types.String `tfsdk:"gcp_project_id"`
	AwsAccountID        types.String `tfsdk:"aws_account_id"`
}

// environmentClouds are the values the Hub accepts for `cloud`.
var environmentClouds = []string{"azure", "gcp", "aws", "stackit", "other"}

// customerBindingAttrs are the mutually exclusive ways to name the customer.
var customerBindingAttrs = []string{"tenant_id", "azure_tenant_id", "azure_tenant_domain", "azure_subscription_id", "gcp_project_id", "aws_account_id"}

// lookups maps each lookup key of GET /tenants/lookup (and of the
// environment's `lookup` body) to its model field.
func (m *environmentModel) lookups() map[string]*types.String {
	return map[string]*types.String{
		"azure_tenant_id":       &m.AzureTenantID,
		"azure_tenant_domain":   &m.AzureTenantDomain,
		"azure_subscription_id": &m.AzureSubscriptionID,
		"gcp_project_id":        &m.GcpProjectID,
		"aws_account_id":        &m.AwsAccountID,
	}
}

func (r *environmentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_environment"
}

func (r *environmentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	lookupAttr := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{
			Optional:      true,
			Description:   desc + " Resolved to the customer by the Hub at create time. Changing it forces a new environment.",
			Validators:    []validator.String{notBlank},
			PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
		}
	}
	resp.Schema = schema.Schema{
		Description: "Registers an environment (one CompanyGPT / AI Gateway installation of a customer) in the innFactory AI Hub. " +
			"Bind it to the customer with exactly one of `tenant_id`, `azure_tenant_id`, `azure_tenant_domain`, `azure_subscription_id`, `gcp_project_id`, `aws_account_id`. " +
			"`name` and `domain` change in place; `cloud` and the customer binding force a new environment.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Hub-side environment identifier (`environment_id`). Use it to import.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Display name in the Hub. Updated in place.",
				Validators:  []validator.String{notBlank},
			},
			"domain": schema.StringAttribute{
				Required:    true,
				Description: "Lower-case host name the installation is served under, e.g. `gpt.example.com` (no scheme, port or path). Updated in place.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(hostNamePattern, "must be a lower-case host name such as gpt.example.com (no scheme, port or path)"),
				},
			},
			"cloud": schema.StringAttribute{
				Required:      true,
				Description:   "Where the installation runs: `azure`, `gcp`, `aws`, `stackit` or `other`. Changing it forces a new environment.",
				Validators:    []validator.String{stringvalidator.OneOf(environmentClouds...)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"tenant_id": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "Hub customer (`tnt_…`). Set it to bind the environment directly; otherwise it is computed from the lookup identifier. " +
					"Changing a configured value forces a new environment.",
				Validators: []validator.String{notBlank},
				// Keep the resolved customer through in-place updates; a
				// configured value that differs forces replacement. Switching
				// between tenant_id and a lookup replaces via the lookup
				// attribute's own RequiresReplace.
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplaceIfConfigured(),
				},
			},
			"azure_tenant_id":       lookupAttr("Azure (Entra ID) tenant / directory ID of the customer."),
			"azure_tenant_domain":   lookupAttr("Azure (Entra ID) tenant domain of the customer, e.g. `contoso.onmicrosoft.com`."),
			"azure_subscription_id": lookupAttr("Azure subscription ID of the customer."),
			"gcp_project_id":        lookupAttr("GCP project ID of the customer."),
			"aws_account_id":        lookupAttr("AWS account ID (12-digit) of the customer."),
		},
	}
}

// ConfigValidators enforces exactly one customer binding at plan time.
// Unknown values (e.g. tenant_id from a data source) defer the check.
func (r *environmentResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	exprs := make([]path.Expression, 0, len(customerBindingAttrs))
	for _, a := range customerBindingAttrs {
		exprs = append(exprs, path.MatchRoot(a))
	}
	return []resource.ConfigValidator{resourcevalidator.ExactlyOneOf(exprs...)}
}

func (r *environmentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// --- wire shapes (contract: AI Hub licenseApi, /api/v1/admin/environments) ---

type environmentIdentifier struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type environmentDTO struct {
	EnvironmentID  string                 `json:"environment_id"`
	TenantID       string                 `json:"tenant_id"`
	Name           string                 `json:"name"`
	Domain         string                 `json:"domain"`
	Cloud          string                 `json:"cloud"`
	IdentifierUsed *environmentIdentifier `json:"identifier_used"`
}

type environmentResponse struct {
	Environment environmentDTO `json:"environment"`
}

// environmentCreateBody carries either Lookup (one identifier) or TenantID.
type environmentCreateBody struct {
	Lookup   map[string]string `json:"lookup,omitempty"`
	TenantID string            `json:"tenant_id,omitempty"`
	Name     string            `json:"name"`
	Domain   string            `json:"domain"`
	Cloud    string            `json:"cloud"`
}

// environmentPatchBody holds the only two mutable fields.
type environmentPatchBody struct {
	Name   string `json:"name"`
	Domain string `json:"domain"`
}

// environmentCreateHints tell the operator how to resolve the Hub's
// documented create rejections.
var environmentCreateHints = map[string]string{
	"tenant_not_found":   "No customer in the Hub carries this identifier. Add it to the customer's identifiers in the Hub, or bind the environment with tenant_id.",
	"ambiguous_lookup":   "Several customers in the Hub carry this identifier. Clean up their identifiers in the Hub, or bind the environment with tenant_id.",
	"environment_exists": "This environment is already registered in the Hub. Adopt it with `terraform import <address> <environment_id>` instead of creating it again.",
}

func environmentPath(id string) string {
	return environmentsPath + "/" + url.PathEscape(id)
}

// customerBinding returns the create body's customer part. Config
// validation guarantees exactly one value; this re-checks defensively.
func (m *environmentModel) customerBinding() (lookup map[string]string, tenantID string, err error) {
	n := 0
	if v, ok := knownString(m.TenantID); ok {
		tenantID = v
		n++
	}
	for kind, field := range m.lookups() {
		if v, ok := knownString(*field); ok {
			lookup = map[string]string{kind: v}
			n++
		}
	}
	if n != 1 {
		return nil, "", fmt.Errorf("exactly one of %v must be set (got %d)", customerBindingAttrs, n)
	}
	return lookup, tenantID, nil
}

func (r *environmentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan environmentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	lookup, tenantID, err := plan.customerBinding()
	if err != nil {
		resp.Diagnostics.AddError("Invalid customer binding", err.Error())
		return
	}
	body := environmentCreateBody{
		Lookup:   lookup,
		TenantID: tenantID,
		Name:     plan.Name.ValueString(),
		Domain:   plan.Domain.ValueString(),
		Cloud:    plan.Cloud.ValueString(),
	}
	// A lost response is retried with the same Idempotency-Key: the Hub then
	// returns the environment it registered instead of a second one.
	var env envelope[environmentResponse]
	if _, err := r.client.create(ctx, environmentsPath, body, &env, nil); err != nil {
		resp.Diagnostics.AddError("Create environment failed", withHint(err, environmentCreateHints))
		return
	}

	// Configured attributes stay as planned; only computed ones come from
	// the Hub, so the result is always consistent with the plan.
	plan.ID = types.StringValue(env.Data.Environment.EnvironmentID)
	if _, ok := knownString(plan.TenantID); !ok {
		plan.TenantID = types.StringValue(env.Data.Environment.TenantID)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *environmentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state environmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var env envelope[environmentResponse]
	err := r.client.do(ctx, http.MethodGet, environmentPath(state.ID.ValueString()), nil, nil, &env)
	if isHubNotFound(err, codeEnvironmentNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Read environment failed", withHint(err, environmentCreateHints))
		return
	}

	state.applyRemote(env.Data.Environment)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

// applyRemote copies the Hub's view into the model. The lookup identifier
// is taken from identifier_used when the Hub reports one (needed after
// import); otherwise the configured identifier in state is kept.
func (m *environmentModel) applyRemote(e environmentDTO) {
	m.ID = types.StringValue(e.EnvironmentID)
	m.Name = types.StringValue(e.Name)
	m.Domain = types.StringValue(e.Domain)
	m.Cloud = types.StringValue(e.Cloud)
	m.TenantID = types.StringValue(e.TenantID)

	if e.IdentifierUsed == nil {
		return
	}
	lookups := m.lookups()
	if _, known := lookups[e.IdentifierUsed.Type]; !known && e.IdentifierUsed.Type != "tenant_id" {
		return
	}
	for kind, field := range lookups {
		if kind == e.IdentifierUsed.Type {
			*field = types.StringValue(e.IdentifierUsed.Value)
		} else {
			*field = types.StringNull()
		}
	}
}

// Update only ever sees name/domain changes: every other configurable
// attribute forces replacement.
func (r *environmentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state environmentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := environmentPatchBody{Name: plan.Name.ValueString(), Domain: plan.Domain.ValueString()}
	if err := r.client.do(ctx, http.MethodPatch, environmentPath(state.ID.ValueString()), nil, body, nil); err != nil {
		resp.Diagnostics.AddError("Update environment failed", withHint(err, environmentCreateHints))
		return
	}

	plan.ID = state.ID
	if _, ok := knownString(plan.TenantID); !ok {
		plan.TenantID = state.TenantID
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *environmentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state environmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.client.do(ctx, http.MethodDelete, environmentPath(state.ID.ValueString()), nil, nil, nil)
	if err != nil && !isHubNotFound(err, codeEnvironmentNotFound) {
		resp.Diagnostics.AddError("Delete environment failed", withHint(err, environmentCreateHints))
	}
}

// ImportState takes the environment id; Read fills in the rest, including
// the lookup identifier when the Hub reports identifier_used.
func (r *environmentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
