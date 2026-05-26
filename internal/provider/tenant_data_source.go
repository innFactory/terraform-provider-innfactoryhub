package provider

import (
	"context"
	"fmt"
	"net/url"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*tenantDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*tenantDataSource)(nil)
)

type tenantDataSource struct {
	client *httpClient
}

func newTenantDataSource() datasource.DataSource {
	return &tenantDataSource{}
}

type tenantDataModel struct {
	ID                     types.String `tfsdk:"id"`
	GcpProjectID           types.String `tfsdk:"gcp_project_id"`
	AzureSubscriptionID    types.String `tfsdk:"azure_subscription_id"`
	AwsAccountID           types.String `tfsdk:"aws_account_id"`
	TenantID               types.String `tfsdk:"tenant_id"`
	LegalName              types.String `tfsdk:"legal_name"`
	GcpProjectIDs          types.List   `tfsdk:"gcp_project_ids"`
	AzureSubscriptionIDs   types.List   `tfsdk:"azure_subscription_ids"`
	AwsAccountIDs          types.List   `tfsdk:"aws_account_ids"`
}

func (d *tenantDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_tenant"
}

func (d *tenantDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Resolves a Hub tenant from one of its cloud-handle identifiers. Exactly one of `gcp_project_id`, `azure_subscription_id`, or `aws_account_id` must be set. Returns the tenant_id, legal_name, and ALL identifier arrays so a single call can enumerate the tenant's other cloud handles.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Hub tenant_id (mirrors `tenant_id`). Provided for Terraform convention.",
			},
			"gcp_project_id": schema.StringAttribute{
				Optional:    true,
				Description: "GCP project ID to look up.",
			},
			"azure_subscription_id": schema.StringAttribute{
				Optional:    true,
				Description: "Azure subscription ID to look up.",
			},
			"aws_account_id": schema.StringAttribute{
				Optional:    true,
				Description: "AWS account ID to look up (12-digit).",
			},
			"tenant_id": schema.StringAttribute{
				Computed:    true,
				Description: "Hub tenant identifier.",
			},
			"legal_name": schema.StringAttribute{
				Computed:    true,
				Description: "Legal name of the tenant as configured in the Hub.",
			},
			"gcp_project_ids": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "All GCP project IDs bound to this tenant.",
			},
			"azure_subscription_ids": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "All Azure subscription IDs bound to this tenant.",
			},
			"aws_account_ids": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "All AWS account IDs bound to this tenant.",
			},
		},
	}
}

func (d *tenantDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, _ *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	d.client = req.ProviderData.(*httpClient)
}

type tenantLookupResponse struct {
	Tenant struct {
		TenantID    string `json:"tenant_id"`
		LegalName   string `json:"legal_name"`
		Identifiers struct {
			GcpProjectIDs        []string `json:"gcp_project_ids"`
			AzureSubscriptionIDs []string `json:"azure_subscription_ids"`
			AwsAccountIDs        []string `json:"aws_account_ids"`
		} `json:"identifiers"`
	} `json:"tenant"`
}

func (d *tenantDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg tenantDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	q := url.Values{}
	count := 0
	if !cfg.GcpProjectID.IsNull() && cfg.GcpProjectID.ValueString() != "" {
		q.Set("gcp_project_id", cfg.GcpProjectID.ValueString())
		count++
	}
	if !cfg.AzureSubscriptionID.IsNull() && cfg.AzureSubscriptionID.ValueString() != "" {
		q.Set("azure_subscription_id", cfg.AzureSubscriptionID.ValueString())
		count++
	}
	if !cfg.AwsAccountID.IsNull() && cfg.AwsAccountID.ValueString() != "" {
		q.Set("aws_account_id", cfg.AwsAccountID.ValueString())
		count++
	}
	if count != 1 {
		resp.Diagnostics.AddError(
			"Exactly one identifier required",
			fmt.Sprintf("Set exactly one of gcp_project_id, azure_subscription_id, aws_account_id (got %d).", count),
		)
		return
	}

	var env envelope[tenantLookupResponse]
	if err := d.client.do(ctx, "GET", "/api/v1/admin/tenants/lookup", q, nil, &env); err != nil {
		resp.Diagnostics.AddError("Tenant lookup failed", err.Error())
		return
	}

	t := env.Data.Tenant
	cfg.ID = types.StringValue(t.TenantID)
	cfg.TenantID = types.StringValue(t.TenantID)
	cfg.LegalName = types.StringValue(t.LegalName)
	cfg.GcpProjectIDs = strList(ctx, &resp.Diagnostics, t.Identifiers.GcpProjectIDs)
	cfg.AzureSubscriptionIDs = strList(ctx, &resp.Diagnostics, t.Identifiers.AzureSubscriptionIDs)
	cfg.AwsAccountIDs = strList(ctx, &resp.Diagnostics, t.Identifiers.AwsAccountIDs)

	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
