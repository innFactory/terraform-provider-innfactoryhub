package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                     = (*licenseResource)(nil)
	_ resource.ResourceWithConfigure        = (*licenseResource)(nil)
	_ resource.ResourceWithConfigValidators = (*licenseResource)(nil)
	_ resource.ResourceWithImportState      = (*licenseResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*licenseResource)(nil)
)

// licensesPath is served by the AI Hub's licence API behind
// hub.innfactory.ai (same envelope and error shape as the admin API).
const licensesPath = "/api/v1/admin/licenses"

// licenseResource issues a licence for an environment in the AI Hub. The
// Hub returns the licence key (`lk_…`) only in the create and rotation
// responses; it is kept in state and never read back. Read tracks whether
// that key is still the licence's key (key_id, key_status).
type licenseResource struct {
	client *httpClient
}

func newLicenseResource() resource.Resource {
	return &licenseResource{}
}

type licenseModel struct {
	ID                 types.String `tfsdk:"id"`
	EnvironmentID      types.String `tfsdk:"environment_id"`
	Product            types.String `tfsdk:"product"`
	Domains            types.Set    `tfsdk:"domains"`
	MaxUsers           types.Int64  `tfsdk:"max_users"`
	Unlimited          types.Bool   `tfsdk:"unlimited"`
	MaxInstances       types.Int64  `tfsdk:"max_instances"`
	Edition            types.String `tfsdk:"edition"`
	Features           types.Set    `tfsdk:"features"`
	Entitlements       types.Set    `tfsdk:"entitlements"`
	ContractPositionID types.String `tfsdk:"contract_position_id"`
	UnbilledReason     types.String `tfsdk:"unbilled_reason"`
	UnbilledMaxUntil   types.String `tfsdk:"unbilled_max_until"`
	RotationTrigger    types.String `tfsdk:"rotation_trigger"`
	LicenseKey         types.String `tfsdk:"license_key"`
	KeyID              types.String `tfsdk:"key_id"`
	KeyStatus          types.String `tfsdk:"key_status"`
	TenantID           types.String `tfsdk:"tenant_id"`
	Status             types.String `tfsdk:"status"`
	ValidUntil         types.String `tfsdk:"valid_until"`
}

const licenseStatusEnded = "ended"

// key_status values: the key in state is the licence's current key, or the
// Hub revoked it (e.g. rotated in the portal).
const (
	keyStatusActive  = "active"
	keyStatusRevoked = "revoked"
)

// Values the Hub accepts (OpenAPI LicenseCreateBody).
var (
	licenseProducts  = []string{"ai-gateway", "ai-prices", "companygpt-maintenance"}
	unbilledReasons  = []string{"trial", "partner", "internal"}
	hubRefIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	nameTokenPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
	// licenseDomainPattern is a lower-case host name, optionally with `*.`
	// as the whole leftmost label. Lower case avoids drift: the Hub stores
	// domains lower-cased.
	licenseDomainPattern = regexp.MustCompile(`^(\*\.)?` + hostNameExpr + `$`)
)

var (
	hubRefID  = stringvalidator.RegexMatches(hubRefIDPattern, "must be a Hub identifier (letters, digits, _ and -)")
	nameToken = stringvalidator.RegexMatches(nameTokenPattern, "must be lower-case letters, digits, _ . or - (at most 64 characters, starting with a letter or digit)")
)

func (r *licenseResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_license"
}

func (r *licenseResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Issues a licence for an environment in the innFactory AI Hub and returns its licence key once.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Hub-side licence identifier (`license_id`). Use it to import.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"environment_id": schema.StringAttribute{
				Required:      true,
				Description:   "Environment (`innfactoryhub_environment.<name>.id`) the licence is issued for. Changing it issues a new licence.",
				Validators:    []validator.String{hubRefID},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"product": schema.StringAttribute{
				Required:      true,
				Description:   "Licensed product: `ai-gateway`, `ai-prices` or `companygpt-maintenance`. Changing it issues a new licence.",
				Validators:    []validator.String{stringvalidator.OneOf(licenseProducts...)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"domains": schema.SetAttribute{
				Required:    true,
				ElementType: types.StringType,
				Description: "Lower-case host names the licence is valid for; `*.example.com` covers exactly one label below example.com.",
				Validators: []validator.Set{
					setvalidator.SizeBetween(1, 50),
					setvalidator.ValueStringsAre(stringvalidator.RegexMatches(licenseDomainPattern,
						"must be a lower-case host name such as gpt.example.com or *.example.com (no scheme, port or path)")),
				},
			},
			"max_users": schema.Int64Attribute{
				Optional:    true,
				Description: "User limit (1–1000000). Set either this or `unlimited = true`.",
				Validators:  []validator.Int64{int64validator.Between(1, 1_000_000)},
			},
			"unlimited": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "`true` issues the licence without a user limit. Set either this or `max_users`.",
			},
			"max_instances": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Default:     int64default.StaticInt64(5),
				Description: "Number of installations that may use the key at the same time (1–100). Defaults to 5.",
				Validators:  []validator.Int64{int64validator.Between(1, 100)},
			},
			"edition": schema.StringAttribute{
				Required:    true,
				Description: "Edition of the catalog product, e.g. `all`.",
				Validators:  []validator.String{notBlank, stringvalidator.LengthAtMost(40)},
			},
			"features": schema.SetAttribute{
				Optional:      true,
				Computed:      true,
				ElementType:   types.StringType,
				Description:   "Subset of the edition's features. Defaults to all features of the edition.",
				Validators:    []validator.Set{setvalidator.SizeAtMost(100), setvalidator.ValueStringsAre(nameToken)},
				PlanModifiers: []planmodifier.Set{useStateUnlessChanged("edition")},
			},
			"entitlements": schema.SetAttribute{
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				Default:     setdefault.StaticValue(types.SetValueMust(types.StringType, []attr.Value{})),
				Description: "Additional entitlements such as `ai-prices`. Defaults to none.",
				Validators:  []validator.Set{setvalidator.SizeAtMost(50), setvalidator.ValueStringsAre(nameToken)},
			},
			"contract_position_id": schema.StringAttribute{
				Optional:    true,
				Description: "Contract position that bills the licence. Set either this or `unbilled_reason`.",
				Validators:  []validator.String{hubRefID},
			},
			"unbilled_reason": schema.StringAttribute{
				Optional:    true,
				Description: "Why the licence is not billed: `trial`, `partner` or `internal`. Set either this or `contract_position_id`.",
				Validators:  []validator.String{stringvalidator.OneOf(unbilledReasons...)},
			},
			"unbilled_max_until": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Last day (`YYYY-MM-DD`) of an unbilled licence. Without it the Hub applies its default per reason.",
				Validators: []validator.String{
					isoDate{},
					stringvalidator.AlsoRequires(path.MatchRoot("unbilled_reason")),
				},
				PlanModifiers: []planmodifier.String{useStateUnlessChanged("unbilled_reason")},
			},
			"rotation_trigger": schema.StringAttribute{
				Optional: true,
				Description: "Rotates the licence key: any change (including setting or removing it) rotates the key of the same licence in place " +
					"(`POST …/keys/rotate`) — the previous key is revoked at once, the new one lands in `license_key`; licence id, instances " +
					"and contract position stay. For a licence ended in the Hub, a change issues a new licence instead.",
			},
			"license_key": schema.StringAttribute{
				Computed:    true,
				Sensitive:   true,
				Description: "Licence key (`lk_<id>.<secret>`). Returned by the Hub only when the licence is issued or its key rotated; empty after import.",
				// The Hub never returns the key again: without this, every
				// update would plan it as unknown and lose it.
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"key_id": schema.StringAttribute{
				Computed:      true,
				Description:   "Id of the key in `license_key` (the `<id>` of `lk_<id>.<secret>`); empty when state holds no key (after import).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"key_status": schema.StringAttribute{
				Computed: true,
				Description: "`active` while `license_key` is the licence's current key; `revoked` once the Hub revoked it, e.g. after a rotation in the portal — " +
					"plans then fail until `rotation_trigger` changes. Empty when state holds no key.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"tenant_id": schema.StringAttribute{
				Computed:      true,
				Description:   "Hub customer (`tnt_…`) of the environment.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"status": schema.StringAttribute{
				Computed: true,
				Description: "`active`, `suspended`, `expired` or `ended`. Terraform never changes it: a licence suspended or ended in the Hub stays so; " +
					"for an ended one, plans fail until `rotation_trigger` changes (which issues a new licence).",
			},
			"valid_until": schema.StringAttribute{
				Computed:    true,
				Description: "Last day the licence is valid (`YYYY-MM-DD`).",
			},
		},
	}
}

// ConfigValidators enforce the billing link and the user limit at plan
// time. Unknown values (e.g. a position id from another resource) defer.
func (r *licenseResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.ExactlyOneOf(path.MatchRoot("contract_position_id"), path.MatchRoot("unbilled_reason")),
		userLimit{},
	}
}

// userLimit requires either max_users or unlimited = true, not both.
// `unlimited = false` next to max_users is fine, so callers can write
// `unlimited = var.max_users == null`.
type userLimit struct{}

func (userLimit) Description(context.Context) string {
	return "exactly one of max_users or unlimited = true must be set"
}

func (v userLimit) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (userLimit) ValidateResource(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var maxUsers types.Int64
	var unlimited types.Bool
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("max_users"), &maxUsers)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("unlimited"), &unlimited)...)
	if resp.Diagnostics.HasError() || maxUsers.IsUnknown() || unlimited.IsUnknown() {
		return
	}
	switch {
	case unlimited.ValueBool() && !maxUsers.IsNull():
		resp.Diagnostics.AddAttributeError(path.Root("max_users"), "Invalid Attribute Combination",
			"max_users cannot be combined with unlimited = true: set max_users for a user limit, or unlimited = true for none.")
	case !unlimited.ValueBool() && maxUsers.IsNull():
		resp.Diagnostics.AddAttributeError(path.Root("max_users"), "Missing Attribute Configuration",
			"Set max_users for a user limit, or unlimited = true for a licence without user limit.")
	}
}

// isoDate accepts a calendar date in the form YYYY-MM-DD.
type isoDate struct{}

const isoDateLayout = "2006-01-02"

func (isoDate) Description(context.Context) string { return "must be a date in the form YYYY-MM-DD" }

func (v isoDate) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v isoDate) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := time.Parse(isoDateLayout, req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid date",
			fmt.Sprintf("%s, got %q", v.Description(ctx), req.ConfigValue.ValueString()))
	}
}

func (r *licenseResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// --- wire shapes (contract: AI Hub licenseApi, /api/v1/admin/licenses) ---

type licenseParamsDTO struct {
	Domains      []string `json:"domains"`
	MaxUsers     *int64   `json:"max_users"`
	MaxInstances int64    `json:"max_instances"`
	EditionID    string   `json:"edition_id"`
	Features     []string `json:"features"`
	Entitlements []string `json:"entitlements"`
}

// licenseUnbilled is `unbilled` in requests and responses; requests omit
// max_until to get the Hub's default for the reason.
type licenseUnbilled struct {
	Reason   string `json:"reason"`
	MaxUntil string `json:"max_until,omitempty"`
}

type licenseDTO struct {
	LicenseID          string           `json:"license_id"`
	TenantID           string           `json:"tenant_id"`
	EnvironmentID      string           `json:"environment_id"`
	Product            string           `json:"product"`
	ContractPositionID *string          `json:"contract_position_id"`
	Unbilled           *licenseUnbilled `json:"unbilled"`
	Params             licenseParamsDTO `json:"params"`
	ValidUntil         string           `json:"valid_until"`
	Status             string           `json:"status"`
}

// licenseKeyInfo is LicenseKeyInfo without the fields the provider ignores.
type licenseKeyInfo struct {
	KeyID     string  `json:"key_id"`
	RevokedAt *string `json:"revoked_at"`
}

// licenseResponse is the body of GET and PATCH; only GET carries keys.
type licenseResponse struct {
	License licenseDTO       `json:"license"`
	Keys    []licenseKeyInfo `json:"keys"`
}

// licenseKeyRotation is the body of POST …/keys/rotate.
type licenseKeyRotation struct {
	LicenseKey string         `json:"license_key"`
	Key        licenseKeyInfo `json:"key"`
}

type licenseCreateResponse struct {
	License    licenseDTO `json:"license"`
	LicenseKey string     `json:"license_key"`
}

// licenseCreateParams always sends max_users (null = unlimited) and
// entitlements; features only when configured (else: all of the edition).
type licenseCreateParams struct {
	Domains      []string  `json:"domains"`
	MaxUsers     *int64    `json:"max_users"`
	MaxInstances int64     `json:"max_instances"`
	EditionID    string    `json:"edition_id"`
	Features     *[]string `json:"features,omitempty"`
	Entitlements []string  `json:"entitlements"`
}

type licenseCreateBody struct {
	EnvironmentID      string              `json:"environment_id"`
	Product            string              `json:"product"`
	Params             licenseCreateParams `json:"params"`
	ContractPositionID string              `json:"contract_position_id,omitempty"`
	Unbilled           *licenseUnbilled    `json:"unbilled,omitempty"`
}

// licensePatchBody follows LicensePatchBody: params are partial (lists
// replace), and at most one billing link. Params is a map so that a changed
// max_users can be sent as an explicit null while unchanged keys are left out.
type licensePatchBody struct {
	Params             map[string]any   `json:"params,omitempty"`
	ContractPositionID string           `json:"contract_position_id,omitempty"`
	Unbilled           *licenseUnbilled `json:"unbilled,omitempty"`
}

// licenseHints tell the operator how to resolve the Hub's documented
// rejections of a create or update.
var licenseHints = map[string]string{
	"billing_link_required": "Set exactly one of contract_position_id or unbilled_reason.",
	"position_not_found":    "The Hub has no contract position with this id (or it was removed). Check contract_position_id against the contract in the portal.",
	"position_already_linked": "Another licence (details.license_id) holds this contract position. Adopt it with `terraform import <address> <license_id>`, or end it first. " +
		"Do not combine a contract position with lifecycle { create_before_destroy = true }: the old licence must release the position before the new one is issued.",
	"position_tenant_mismatch":  "The contract position belongs to another customer than the environment. Use a position of the environment's customer.",
	"position_product_mismatch": "The contract position is for a different product. Use a position whose product matches the licence's product.",
	"position_ended":            "The contract position has ended. Link a running position, or issue the licence unbilled.",
	"environment_not_found":     "The Hub has no environment with this id. Reference innfactoryhub_environment.<name>.id.",
	"tenant_archived":           "The environment's customer is archived in the Hub; no licences are issued for it.",
	"product_not_in_catalog":    "The Hub's catalog has no product for this licence product. Add it in the portal first.",
	"edition_unknown":           "Use one of the editions listed in details.available (the editions of the catalog product in the portal).",
	"feature_not_in_edition":    "features must be a subset of the edition's features (details.features lists the others); omit features to grant all of them.",
	"invalid_domain":            "details.domains lists the rejected entries. Use host names such as gpt.example.com or *.example.com.",
	"unbilled_max_exceeded":     "unbilled_max_until is beyond the limit in details.max_until_limit: trial at most 60 days, partner and internal at most 12 months from today.",
	"unbilled_max_in_past":      "unbilled_max_until must be today or later.",
	"license_ended": "The licence was ended in the AI Hub. Terraform does not re-issue it on its own: change rotation_trigger to issue a new licence, " +
		"or remove the resource from the configuration.",
}

func licensePath(id string) string {
	return licensesPath + "/" + url.PathEscape(id)
}

// maxUsers is the wire value: nil means unlimited.
func (m *licenseModel) maxUsers() *int64 {
	if m.Unlimited.ValueBool() || m.MaxUsers.IsNull() || m.MaxUsers.IsUnknown() {
		return nil
	}
	v := m.MaxUsers.ValueInt64()
	return &v
}

func (m *licenseModel) createBody(ctx context.Context, diags *diag.Diagnostics) licenseCreateBody {
	body := licenseCreateBody{
		EnvironmentID: m.EnvironmentID.ValueString(),
		Product:       m.Product.ValueString(),
		Params: licenseCreateParams{
			Domains:      stringsOf(ctx, diags, m.Domains),
			MaxUsers:     m.maxUsers(),
			MaxInstances: m.MaxInstances.ValueInt64(),
			EditionID:    m.Edition.ValueString(),
			Entitlements: stringsOf(ctx, diags, m.Entitlements),
		},
	}
	if body.Params.Entitlements == nil {
		body.Params.Entitlements = []string{}
	}
	if features := stringsOf(ctx, diags, m.Features); features != nil {
		body.Params.Features = &features
	}
	body.ContractPositionID, body.Unbilled = m.billingLink()
	return body
}

// billingLink is the configured billing link: a contract position, or else
// an unbilled reason (with max_until only when it is known).
func (m *licenseModel) billingLink() (position string, unbilled *licenseUnbilled) {
	if v, ok := knownString(m.ContractPositionID); ok {
		return v, nil
	}
	if reason, ok := knownString(m.UnbilledReason); ok {
		maxUntil, _ := knownString(m.UnbilledMaxUntil)
		return "", &licenseUnbilled{Reason: reason, MaxUntil: maxUntil}
	}
	return "", nil
}

// setKey stores a key the Hub just returned, with its id; the key is then
// the licence's current one.
func (m *licenseModel) setKey(plaintext, keyID string) {
	m.LicenseKey = types.StringValue(plaintext)
	m.KeyID, m.KeyStatus = types.StringNull(), types.StringNull()
	if keyID != "" {
		m.KeyID, m.KeyStatus = types.StringValue(keyID), types.StringValue(keyStatusActive)
	}
}

// licenseKeyID is the `<id>` of `lk_<id>.<secret>` ("" for another shape).
func licenseKeyID(plaintext string) string {
	rest, ok := strings.CutPrefix(plaintext, "lk_")
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(rest, ".")
	if !ok {
		return ""
	}
	return id
}

// trackKey records whether the key in state is still the licence's current
// key. keys == nil means the Hub reported none: what is known stays.
func (m *licenseModel) trackKey(keys []licenseKeyInfo) {
	if _, ok := knownString(m.KeyID); !ok {
		// State written before key_id existed: derive it from the key.
		if id := licenseKeyID(m.LicenseKey.ValueString()); id != "" {
			m.KeyID = types.StringValue(id)
		}
	}
	id, ok := knownString(m.KeyID)
	if !ok {
		m.KeyID, m.KeyStatus = types.StringNull(), types.StringNull()
		return
	}
	if keys == nil {
		return
	}
	m.KeyStatus = types.StringValue(keyStatusRevoked)
	for _, k := range keys {
		if k.KeyID == id && k.RevokedAt == nil {
			m.KeyStatus = types.StringValue(keyStatusActive)
		}
	}
}

// fillComputed copies what only the Hub knows into a planned model:
// identifiers, status, validity, and Hub defaults left unknown in the plan.
func (m *licenseModel) fillComputed(ctx context.Context, diags *diag.Diagnostics, l licenseDTO) {
	m.ID = types.StringValue(l.LicenseID)
	m.TenantID = types.StringValue(l.TenantID)
	m.Status = types.StringValue(l.Status)
	m.ValidUntil = types.StringValue(l.ValidUntil)
	if m.Features.IsUnknown() {
		m.Features = strSet(ctx, diags, l.Params.Features)
	}
	if m.UnbilledMaxUntil.IsUnknown() {
		m.UnbilledMaxUntil = types.StringNull()
		if l.Unbilled != nil {
			m.UnbilledMaxUntil = types.StringValue(l.Unbilled.MaxUntil)
		}
	}
}

// applyRemote copies the Hub's view of every Hub-owned attribute into the
// model. license_key and rotation_trigger are Terraform-side and untouched.
func (m *licenseModel) applyRemote(ctx context.Context, diags *diag.Diagnostics, l licenseDTO) {
	m.ID = types.StringValue(l.LicenseID)
	m.TenantID = types.StringValue(l.TenantID)
	m.EnvironmentID = types.StringValue(l.EnvironmentID)
	m.Product = types.StringValue(l.Product)
	m.Domains = strSet(ctx, diags, l.Params.Domains)
	m.MaxUsers = types.Int64PointerValue(l.Params.MaxUsers)
	m.Unlimited = types.BoolValue(l.Params.MaxUsers == nil)
	m.MaxInstances = types.Int64Value(l.Params.MaxInstances)
	m.Edition = types.StringValue(l.Params.EditionID)
	m.Features = strSet(ctx, diags, l.Params.Features)
	m.Entitlements = strSet(ctx, diags, l.Params.Entitlements)
	m.ContractPositionID = types.StringPointerValue(l.ContractPositionID)
	m.UnbilledReason, m.UnbilledMaxUntil = types.StringNull(), types.StringNull()
	if l.Unbilled != nil {
		m.UnbilledReason = types.StringValue(l.Unbilled.Reason)
		m.UnbilledMaxUntil = types.StringValue(l.Unbilled.MaxUntil)
	}
	m.Status = types.StringValue(l.Status)
	m.ValidUntil = types.StringValue(l.ValidUntil)
}

func (r *licenseResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan licenseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := plan.createBody(ctx, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	var env envelope[licenseCreateResponse]
	if err := r.client.do(ctx, http.MethodPost, licensesPath, nil, body, &env); err != nil {
		resp.Diagnostics.AddError("Create licence failed", withHint(err, licenseHints))
		return
	}

	plan.fillComputed(ctx, &resp.Diagnostics, env.Data.License)
	plan.setKey(env.Data.LicenseKey, licenseKeyID(env.Data.LicenseKey))
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *licenseResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state licenseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var env envelope[licenseResponse]
	err := r.client.do(ctx, http.MethodGet, licensePath(state.ID.ValueString()), nil, nil, &env)
	// The Hub never deletes a licence; 404 means another Hub or lost data.
	if isNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Read licence failed", withHint(err, licenseHints))
		return
	}

	// An ended licence stays in state with status "ended" — dropping it would
	// re-issue it on the next apply and undo the Hub's decision (SEC-006).
	// ModifyPlan refuses to plan it until rotation_trigger changes.
	state.applyRemote(ctx, &resp.Diagnostics, env.Data.License)
	state.trackKey(env.Data.Keys)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

// ModifyPlan keeps the Hub's revocations: a licence ended or a key revoked
// in the Hub is not replaced behind the operator's back. Only a
// rotation_trigger change acts on them — a new licence for an ended one, an
// in-place key rotation otherwise.
func (r *licenseResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return // create or destroy
	}
	var state, plan licenseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(resp.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	rotate := !plan.RotationTrigger.Equal(state.RotationTrigger)
	trigger := path.Root("rotation_trigger")

	switch {
	case state.Status.ValueString() == licenseStatusEnded && rotate:
		resp.RequiresReplace = append(resp.RequiresReplace, trigger)
	case state.Status.ValueString() == licenseStatusEnded:
		resp.Diagnostics.AddAttributeError(trigger, "Licence ended in the AI Hub – change rotation_trigger to issue a new one",
			fmt.Sprintf("Licence %s was ended in the AI Hub (e.g. in the portal). Terraform does not undo that on its own. "+
				"To issue a new licence with a new key, change rotation_trigger; to retire it, remove the resource from the configuration.",
				state.ID.ValueString()))
	case state.KeyStatus.ValueString() == keyStatusRevoked && !rotate:
		resp.Diagnostics.AddAttributeError(trigger, "Licence key revoked in the AI Hub – change rotation_trigger to issue a new key",
			fmt.Sprintf("The licence key in state (key %s of licence %s) was revoked in the AI Hub, e.g. because it was rotated in the portal. "+
				"Terraform does not hand out the revoked key and does not rotate on its own: change rotation_trigger to rotate the key "+
				"of the same licence in place and store the new key.", state.KeyID.ValueString(), state.ID.ValueString()))
	case rotate:
		planKey(ctx, resp, types.StringUnknown(), types.StringUnknown(), types.StringValue(keyStatusActive))
	default:
		// No rotation: the key stays as it is, including "no key" after import.
		planKey(ctx, resp, state.LicenseKey, state.KeyID, state.KeyStatus)
	}
}

// planKey plans the key attributes.
func planKey(ctx context.Context, resp *resource.ModifyPlanResponse, key, id, status types.String) {
	for name, v := range map[string]types.String{"license_key": key, "key_id": id, "key_status": status} {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(name), v)...)
	}
}

// Update changes parameters and billing link in place with one PATCH that
// carries only what changed, then — if rotation_trigger changed — rotates
// the key of the same licence. Without a rotation the key stays as issued.
func (r *licenseResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state licenseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := patchBody(ctx, &resp.Diagnostics, plan, state)
	if resp.Diagnostics.HasError() {
		return
	}

	// Unchanged parameters and billing link are not sent. With nothing to
	// change (a pure rotation) the licence is only read for its status.
	id := state.ID.ValueString()
	var env envelope[licenseResponse]
	var err error
	if body.isEmpty() {
		err = r.client.do(ctx, http.MethodGet, licensePath(id), nil, nil, &env)
	} else {
		err = r.client.do(ctx, http.MethodPatch, licensePath(id), nil, body, &env)
	}
	if err != nil {
		resp.Diagnostics.AddError("Update licence failed", withHint(err, licenseHints))
		return
	}
	plan.fillComputed(ctx, &resp.Diagnostics, env.Data.License)
	plan.LicenseKey, plan.KeyID, plan.KeyStatus = state.LicenseKey, state.KeyID, state.KeyStatus

	if !plan.RotationTrigger.Equal(state.RotationTrigger) {
		var rot envelope[licenseKeyRotation]
		if err := r.client.do(ctx, http.MethodPost, licensePath(id)+"/keys/rotate", nil, struct{}{}, &rot); err != nil {
			// Keep the update, not the rotation: the previous key and trigger
			// stay in state, so the next apply retries the rotation.
			plan.RotationTrigger = state.RotationTrigger
			resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
			resp.Diagnostics.AddError("Rotate licence key failed", withHint(err, licenseHints))
			return
		}
		plan.setKey(rot.Data.LicenseKey, rot.Data.Key.KeyID)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (b licensePatchBody) isEmpty() bool {
	return len(b.Params) == 0 && b.ContractPositionID == "" && b.Unbilled == nil
}

// patchBody diffs plan against state: changed params, and the billing link
// only when it changes (the same position or reason is never re-sent).
func patchBody(ctx context.Context, diags *diag.Diagnostics, plan, state licenseModel) licensePatchBody {
	body := licensePatchBody{Params: paramsPatch(ctx, diags, plan, state)}
	position, unbilled := plan.billingLink()
	switch {
	case position != "":
		if !plan.ContractPositionID.Equal(state.ContractPositionID) {
			body.ContractPositionID = position
		}
	case unbilled != nil:
		reasonChanged := !plan.UnbilledReason.Equal(state.UnbilledReason)
		endChanged := unbilled.MaxUntil != "" && !plan.UnbilledMaxUntil.Equal(state.UnbilledMaxUntil)
		if reasonChanged || endChanged {
			body.Unbilled = unbilled
		}
	}
	return body
}

// paramsPatch holds the changed params only.
func paramsPatch(ctx context.Context, diags *diag.Diagnostics, plan, state licenseModel) map[string]any {
	params := map[string]any{}
	if !plan.Domains.Equal(state.Domains) {
		params["domains"] = stringsOf(ctx, diags, plan.Domains)
	}
	if !equalInt64Ptr(plan.maxUsers(), state.maxUsers()) {
		params["max_users"] = plan.maxUsers()
	}
	if !plan.MaxInstances.Equal(state.MaxInstances) {
		params["max_instances"] = plan.MaxInstances.ValueInt64()
	}
	editionChanged := !plan.Edition.Equal(state.Edition)
	if editionChanged {
		params["edition_id"] = plan.Edition.ValueString()
	}
	// Known features go along with a new edition too: without them the Hub
	// would grant all features of that edition.
	if !plan.Features.IsUnknown() && (editionChanged || !plan.Features.Equal(state.Features)) {
		params["features"] = stringsOf(ctx, diags, plan.Features)
	}
	if !plan.Entitlements.Equal(state.Entitlements) {
		params["entitlements"] = stringsOf(ctx, diags, plan.Entitlements)
	}
	return params
}

func equalInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Delete ends the licence; the Hub keeps it as history and frees its
// contract position. A licence the Hub no longer knows counts as gone.
func (r *licenseResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state licenseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.client.do(ctx, http.MethodDelete, licensePath(state.ID.ValueString()), nil, nil, nil)
	if err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError("End licence failed", err.Error())
	}
}

// ImportState takes the licence id; Read fills in the rest. The key cannot
// be recovered: the Hub stores only its hash.
func (r *licenseResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
	resp.Diagnostics.AddWarning(
		"Licence key not recoverable on import",
		"The Hub returns a licence key only when it issues the licence or rotates its key, so license_key is empty after import. "+
			"To get a key into state, set or change rotation_trigger: that rotates the key of this licence in place (the previous key is revoked).",
	)
}
