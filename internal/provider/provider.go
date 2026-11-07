package provider

import (
	"context"
	"net/url"
	"os"
	"strings"

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
		Description: "Provider for innFactory Hub (hub.innfactory.cloud) and the innFactory AI Hub (hub.innfactory.ai) — manages API keys, environments and licences and reads tenant identifiers.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional:    true,
				Description: "Base URL of the Hub admin API, e.g. https://hub.innfactory.ai for the innFactory AI Hub. Defaults to https://hub.innfactory.cloud. Must be https on innfactory.cloud, innfactory.ai or a subdomain of either. Override via env INNFACTORYHUB_ENDPOINT.",
			},
			"admin_key": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "System-scope `ak_<id>.<secret>` admin key with `write` permission. Override via env INNFACTORYHUB_ADMIN_KEY (recommended). Only required once a resource or data source actually calls the Hub, so it may stay empty in plans where every instance is disabled via `count`.",
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

	// A missing admin key is NOT an error here: callers such as CompanyGPT
	// configure the provider in every plan but only read the key from their
	// secret store when the feature is enabled (count-gated). The client
	// refuses each request instead, so the error surfaces exactly when an
	// operation needs the Hub — see errMissingAdminKey.

	// Endpoint hardening: refuse to send the bearer token to anything that
	// isn't https + the innfactory.cloud / innfactory.ai trust boundary. Without this, a
	// hostile module / typo'd env var / supply-chain compromise could
	// exfil the admin key to an attacker-controlled host. Operators who
	// genuinely need a custom endpoint (staging-via-tunnel, local dev) must
	// opt in explicitly with INNFACTORYHUB_INSECURE_ENDPOINT=1.
	insecure := os.Getenv("INNFACTORYHUB_INSECURE_ENDPOINT") == "1"
	if err := validateEndpoint(endpoint, insecure); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Refusing to send admin key", err.Error())
		return
	}

	client := newClient(endpoint, adminKey, p.version)
	resp.DataSourceData = client
	resp.ResourceData = client
}

// trustedDomains are the registrable domains the admin key may be sent to:
// innfactory.cloud (innfactoryHUB) and innfactory.ai (innFactory AI Hub).
// Each domain is accepted as apex and with any subdomain — nothing else.
var trustedDomains = []string{"innfactory.cloud", "innfactory.ai"}

// validateEndpoint enforces https + a trusted hostname unless the caller has
// explicitly opted into insecure mode. Reject `http://`, raw IPs, and
// arbitrary domains — those are the credential-exfil vectors a
// public-registry provider has to defend against.
func validateEndpoint(raw string, insecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errNotAUrl
	}
	if u.Scheme != "https" && !insecure {
		return errNotHttps
	}
	if isTrustedHost(u.Hostname()) || insecure {
		return nil
	}
	return errNotInnfactoryHost
}

// isTrustedHost matches label boundaries, so innfactory.ai.evil.com and
// evilinnfactory.ai are refused while hub.innfactory.ai is accepted.
func isTrustedHost(host string) bool {
	host = strings.ToLower(host)
	for _, domain := range trustedDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func (p *innfactoryhubProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		newApiKeyResource,
		newEnvironmentResource,
		newLicenseResource,
	}
}

func (p *innfactoryhubProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		newTenantDataSource,
	}
}

var (
	errNotAUrl           = errEndpoint("endpoint is not a valid URL")
	errNotHttps          = errEndpoint("endpoint must use https:// — set INNFACTORYHUB_INSECURE_ENDPOINT=1 to override for local dev")
	errNotInnfactoryHost = errEndpoint("endpoint host must be innfactory.cloud, innfactory.ai or one of their subdomains (e.g. hub.innfactory.cloud, hub.innfactory.ai) — set INNFACTORYHUB_INSECURE_ENDPOINT=1 to override for staging-via-tunnel or local dev")
)

type errEndpoint string

func (e errEndpoint) Error() string { return string(e) }

func strFromAttrOrEnv(v types.String, envKey, fallback string) string {
	if !v.IsNull() && !v.IsUnknown() && v.ValueString() != "" {
		return v.ValueString()
	}
	if env := os.Getenv(envKey); env != "" {
		return env
	}
	return fallback
}
