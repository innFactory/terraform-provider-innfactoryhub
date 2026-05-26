package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Compile-time interface check.
var _ provider.Provider = (*innfactoryhubProvider)(nil)

type innfactoryhubProvider struct {
	version string
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &innfactoryhubProvider{version: version}
	}
}

func (p *innfactoryhubProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "innfactoryhub"
	resp.Version = p.version
}

type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	AdminKey types.String `tfsdk:"admin_key"`
}

func (p *innfactoryhubProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Provider for innFactory Hub — manages reseller API keys and reads tenant identifiers.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional:    true,
				Description: "Base URL of the Hub admin API. Defaults to https://hub.innfactory.cloud. Override via env INNFACTORYHUB_ENDPOINT.",
			},
			"admin_key": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "System-scope `ak_<id>.<secret>` admin key with `write` permission. Override via env INNFACTORYHUB_ADMIN_KEY (recommended).",
			},
		},
	}
}

func (p *innfactoryhubProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint := strFromAttrOrEnv(data.Endpoint, "INNFACTORYHUB_ENDPOINT", "https://hub.innfactory.cloud")
	adminKey := strFromAttrOrEnv(data.AdminKey, "INNFACTORYHUB_ADMIN_KEY", "")

	if adminKey == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("admin_key"),
			"Missing admin key",
			"Set provider.admin_key, or export INNFACTORYHUB_ADMIN_KEY with a system-scope key minted in the Hub admin UI.",
		)
		return
	}

	client := newClient(endpoint, adminKey, p.version)
	resp.DataSourceData = client
	resp.ResourceData = client
}

func (p *innfactoryhubProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		newApiKeyResource,
	}
}

func (p *innfactoryhubProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		newTenantDataSource,
	}
}

func strFromAttrOrEnv(v types.String, envKey, fallback string) string {
	if !v.IsNull() && !v.IsUnknown() && v.ValueString() != "" {
		return v.ValueString()
	}
	if env := os.Getenv(envKey); env != "" {
		return env
	}
	return fallback
}
