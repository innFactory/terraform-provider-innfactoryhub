package provider

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
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
	Endpoint     types.String `tfsdk:"endpoint"`
	AdminKey     types.String `tfsdk:"admin_key"`
	AuditContext types.String `tfsdk:"audit_context"`
}

func (p *innfactoryhubProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Provider for innFactory Hub (hub.innfactory.cloud) and the innFactory AI Hub (hub.innfactory.ai) — manages API keys, environments and licences and reads tenant identifiers.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional:    true,
				Description: "Base URL of the Hub admin API: https://hub.innfactory.ai (innFactory AI Hub) or https://hub.innfactory.cloud (innfactoryHUB, the default). Another host only with INNFACTORYHUB_INSECURE_ENDPOINT=1 plus INNFACTORYHUB_INSECURE_ENDPOINT_HOST=<that host> (local development; warns). Override via env INNFACTORYHUB_ENDPOINT.",
			},
			"admin_key": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				Description: "System-scope `ak_<id>.<secret>` key. On the innFactory AI Hub use permissions `[\"read\", \"licensing\"]` to apply " +
					"and `[\"read_licensing\"]` (read-only) for plans, e.g. in pull requests; on innfactoryHUB a key with `write`. " +
					"Override via env INNFACTORYHUB_ADMIN_KEY (recommended). Only required once a resource or data source actually calls the Hub, " +
					"so it may stay empty in plans where every instance is disabled via `count`.",
			},
			"audit_context": schema.StringAttribute{
				Optional: true,
				Description: "Free text the Hub stores with every change in its audit log (header `X-Audit-Context`), e.g. the CI run URL, commit and customer. " +
					"Sanitised to at most 256 printable ASCII characters on one line. Override via env INNFACTORYHUB_AUDIT_CONTEXT.",
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

	// Endpoint hardening: refuse to send the bearer token to anything but
	// https on one of the two hub hosts. Without this, a hostile module /
	// typo'd env var / supply-chain compromise could exfil the admin key to
	// an attacker-controlled host. Operators who genuinely need a custom
	// endpoint (staging-via-tunnel, local dev) must opt in explicitly with
	// INNFACTORYHUB_INSECURE_ENDPOINT=1 and name that very host in
	// INNFACTORYHUB_INSECURE_ENDPOINT_HOST.
	insecure := os.Getenv("INNFACTORYHUB_INSECURE_ENDPOINT") == "1"
	allowedHost := os.Getenv("INNFACTORYHUB_INSECURE_ENDPOINT_HOST")
	if err := validateEndpoint(endpoint, insecure, allowedHost); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Refusing to send admin key", err.Error())
		return
	}
	if insecure {
		resp.Diagnostics.AddAttributeWarning(path.Root("endpoint"), "Endpoint host pin disabled",
			fmt.Sprintf("INNFACTORYHUB_INSECURE_ENDPOINT=1 is set: the admin key is sent to %s (INNFACTORYHUB_INSECURE_ENDPOINT_HOST=%q) "+
				"instead of only to https://hub.innfactory.ai or https://hub.innfactory.cloud. Use it for local development only, never in CI.",
				redact(endpoint), allowedHost))
	}

	client := newClient(endpoint, adminKey, p.version)
	client.auditContext = sanitizeAuditContext(strFromAttrOrEnv(data.AuditContext, "INNFACTORYHUB_AUDIT_CONTEXT", ""))
	resp.DataSourceData = client
	resp.ResourceData = client
}

// trustedHosts are the only hosts the admin key may be sent to: the
// innfactoryHUB and the innFactory AI Hub. Exact names, no subdomains — any
// other name in these domains (staging sites, the licence site, a
// subdomain-takeover) is untrusted.
var trustedHosts = []string{"hub.innfactory.cloud", "hub.innfactory.ai"}

// validateEndpoint accepts exactly https://<trusted host> (default port, no
// credentials, path, query or fragment). The insecure opt-out accepts one
// more host, the one named in allowedHost (both are needed): over https, or
// over plain http when it is the local machine (local dev, a tunnel to
// staging) — never plain http across the network.
func validateEndpoint(raw string, insecure bool, allowedHost string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errNotAUrl
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errNotAnOrigin
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme == "https" && isTrustedHost(host) && (u.Port() == "" || u.Port() == "443") {
		return nil
	}
	switch {
	case !insecure && u.Scheme != "https":
		return errNotHttps
	case !insecure:
		return errNotInnfactoryHost
	case strings.TrimSpace(allowedHost) == "":
		return errOptOutWithoutHost
	case !strings.EqualFold(host, strings.TrimSpace(allowedHost)):
		return errNotAllowedHost
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopback(host) {
			return nil
		}
		return errHttpNotLocal
	}
	return errNotHttps
}

func isTrustedHost(host string) bool { return slices.Contains(trustedHosts, host) }

// isLoopback reports localhost or a loopback IP — never a name that merely
// starts with "localhost".
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
	errNotAnOrigin       = errEndpoint("endpoint must be the bare origin, e.g. https://hub.innfactory.ai — no credentials, path, query or fragment")
	errNotHttps          = errEndpoint("endpoint must use https:// — set INNFACTORYHUB_INSECURE_ENDPOINT=1 to override for local dev")
	errNotInnfactoryHost = errEndpoint("endpoint must be https://hub.innfactory.ai or https://hub.innfactory.cloud — for staging-via-tunnel or local dev set INNFACTORYHUB_INSECURE_ENDPOINT=1 and INNFACTORYHUB_INSECURE_ENDPOINT_HOST=<host>")
	errHttpNotLocal      = errEndpoint("plain http is only allowed to localhost or a loopback address, even with INNFACTORYHUB_INSECURE_ENDPOINT=1 — use https://")
	errOptOutWithoutHost = errEndpoint("INNFACTORYHUB_INSECURE_ENDPOINT=1 also needs INNFACTORYHUB_INSECURE_ENDPOINT_HOST=<host> naming the endpoint's host")
	errNotAllowedHost    = errEndpoint("the endpoint's host is not the one named in INNFACTORYHUB_INSECURE_ENDPOINT_HOST")
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
