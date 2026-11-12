package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Smoke test: the provider compiles into a server that the framework can
// hand to terraform-plugin-go without panicking. Catches obviously broken
// schemas (duplicate attribute names, malformed types) at build time.
func TestProviderServerSmoke(t *testing.T) {
	t.Parallel()
	server := providerserver.NewProtocol6(New("test")())()
	if server == nil {
		t.Fatal("expected non-nil provider server")
	}
	// Calling GetProviderSchema exercises the provider Schema, every
	// resource Schema, and every data source Schema.
	resp, err := server.GetProviderSchema(context.Background(), new(tfprotov6.GetProviderSchemaRequest))
	if err != nil {
		t.Fatalf("GetProviderSchema failed: %v", err)
	}
	for _, d := range resp.Diagnostics {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			t.Fatalf("schema diagnostic: %s: %s", d.Summary, d.Detail)
		}
	}
	for _, name := range []string{"innfactoryhub_api_key", "innfactoryhub_environment", "innfactoryhub_license"} {
		if _, ok := resp.ResourceSchemas[name]; !ok {
			t.Errorf("resource %s is not registered", name)
		}
	}
	if _, ok := resp.DataSourceSchemas["innfactoryhub_tenant"]; !ok {
		t.Error("data source innfactoryhub_tenant is not registered")
	}
}

// configureProvider runs the provider's Configure with the given attribute
// values (nil = attribute not set in HCL).
func configureProvider(t *testing.T, endpoint, adminKey *string) fwprovider.ConfigureResponse {
	t.Helper()
	ctx := context.Background()
	p := New("test")()

	var schemaResp fwprovider.SchemaResponse
	p.Schema(ctx, fwprovider.SchemaRequest{}, &schemaResp)

	str := func(v *string) tftypes.Value {
		if v == nil {
			return tftypes.NewValue(tftypes.String, nil)
		}
		return tftypes.NewValue(tftypes.String, *v)
	}
	raw := tftypes.NewValue(tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"endpoint":      tftypes.String,
		"admin_key":     tftypes.String,
		"audit_context": tftypes.String,
	}}, map[string]tftypes.Value{
		"endpoint":      str(endpoint),
		"admin_key":     str(adminKey),
		"audit_context": str(nil),
	})

	var resp fwprovider.ConfigureResponse
	p.Configure(ctx, fwprovider.ConfigureRequest{
		Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: raw},
	}, &resp)
	return resp
}

// A missing admin key must not fail provider configuration: CompanyGPT
// configures the AI Hub provider in every plan but reads the key only when
// the licence feature is on (count-gated). The check moves to the first
// operation that actually talks to the Hub.
func TestConfigureWithoutAdminKeySucceeds(t *testing.T) {
	t.Setenv("INNFACTORYHUB_ADMIN_KEY", "")
	t.Setenv("INNFACTORYHUB_ENDPOINT", "")
	t.Setenv("INNFACTORYHUB_INSECURE_ENDPOINT", "")

	cases := []struct {
		name     string
		endpoint *string
	}{
		{name: "default endpoint", endpoint: nil},
		{name: "ai hub endpoint", endpoint: ptr("https://hub.innfactory.ai")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := configureProvider(t, tc.endpoint, nil)
			if resp.Diagnostics.HasError() {
				t.Fatalf("Configure without admin key returned errors: %v", resp.Diagnostics)
			}
			if resp.ResourceData == nil || resp.DataSourceData == nil {
				t.Fatal("Configure must still hand a client to resources and data sources")
			}
		})
	}
}

// The endpoint pin stays at configure time — it is the exfil boundary and
// must not wait for an operation.
func TestConfigureRefusesUntrustedEndpoint(t *testing.T) {
	t.Setenv("INNFACTORYHUB_INSECURE_ENDPOINT", "")
	resp := configureProvider(t, ptr("https://innfactory.ai.evil.com"), ptr(testAdminKey))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected configure to refuse innfactory.ai.evil.com")
	}
}

func TestMissingAdminKeyFailsOnOperationWithoutContactingHub(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{{
			Config: hub.providerBlockWithoutKey() + fmt.Sprintf(`
data "innfactoryhub_tenant" "self" {
  azure_tenant_id = %q
}
`, alphaAzureTenantID),
			ExpectError: regexp.MustCompile(`(?i)missing admin key`),
		}},
	})
	if n := hub.callCount(); n != 0 {
		t.Fatalf("provider sent %d request(s) without an admin key", n)
	}
}

// count = 0 means no instance ever talks to the Hub, so an unset key must
// not break plan or apply.
func TestMissingAdminKeyIgnoredWhenProviderUnused(t *testing.T) {
	hub := newFakeHub(t)
	useFakeHubEnv(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{{
			Config: hub.providerBlockWithoutKey() + `
resource "innfactoryhub_api_key" "unused" {
  count       = 0
  name        = "unused"
  scope       = "system"
  permissions = ["read"]
}
`,
		}},
	})
	if n := hub.callCount(); n != 0 {
		t.Fatalf("expected no Hub traffic, got %d request(s)", n)
	}
}

func ptr[T any](v T) *T { return &v }

// The audit context reaches the Hub on every request, from the provider
// attribute or else from INNFACTORYHUB_AUDIT_CONTEXT, sanitised to one line.
func TestProviderSendsAuditContext(t *testing.T) {
	cases := []struct {
		name string
		env  string
		attr string
		want string
	}{
		{name: "from the environment", env: "run=https://ci.example/runs/42\ncommit=abc tenant=tnt_alpha",
			want: "run=https://ci.example/runs/42 commit=abc tenant=tnt_alpha"},
		{name: "attribute wins over the environment", env: "from-env", attr: "from-attribute", want: "from-attribute"},
		{name: "unset", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeHub(t)
			useFakeHubEnv(t)
			t.Setenv("INNFACTORYHUB_AUDIT_CONTEXT", tc.env)
			attr := ""
			if tc.attr != "" {
				attr = fmt.Sprintf("  audit_context = %q\n", tc.attr)
			}
			cfg := fmt.Sprintf(`
provider "innfactoryhub" {
  endpoint  = %q
  admin_key = %q
%s}

data "innfactoryhub_tenant" "self" {
  tenant_id = %q
}
`, hub.URL(), testAdminKey, attr, alphaTenantID)

			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testProviderFactories(),
				Steps:                    []resource.TestStep{{Config: cfg}},
			})
			got := hub.auditContexts()
			if len(got) == 0 {
				t.Fatal("no request reached the Hub")
			}
			for i, v := range got {
				if v != tc.want {
					t.Fatalf("request %d sent X-Audit-Context %q, want %q", i, v, tc.want)
				}
			}
		})
	}
}

// The opt-out is loud: every configuration with it warns, and it needs the
// host named in INNFACTORYHUB_INSECURE_ENDPOINT_HOST.
func TestConfigureInsecureEndpointOptOut(t *testing.T) {
	cases := []struct {
		name     string
		insecure string
		host     string
		endpoint string
		wantErr  bool
		wantWarn bool
	}{
		{name: "opt-out with matching host warns", insecure: "1", host: "staging.example.com", endpoint: "https://staging.example.com", wantWarn: true},
		{name: "opt-out without host is refused", insecure: "1", endpoint: "https://staging.example.com", wantErr: true},
		{name: "host without opt-out is refused", host: "staging.example.com", endpoint: "https://staging.example.com", wantErr: true},
		{name: "opt-out set for a trusted host still warns", insecure: "1", host: "localhost", endpoint: "https://hub.innfactory.ai", wantWarn: true},
		{name: "no opt-out, trusted host, no warning", endpoint: "https://hub.innfactory.ai"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("INNFACTORYHUB_INSECURE_ENDPOINT", tc.insecure)
			t.Setenv("INNFACTORYHUB_INSECURE_ENDPOINT_HOST", tc.host)
			t.Setenv("INNFACTORYHUB_ENDPOINT", "")
			resp := configureProvider(t, ptr(tc.endpoint), ptr(testAdminKey))
			if got := resp.Diagnostics.HasError(); got != tc.wantErr {
				t.Fatalf("error = %v, want %v: %v", got, tc.wantErr, resp.Diagnostics)
			}
			var warned bool
			for _, d := range resp.Diagnostics.Warnings() {
				warned = warned || strings.Contains(d.Detail(), "INNFACTORYHUB_INSECURE_ENDPOINT")
			}
			if warned != tc.wantWarn {
				t.Fatalf("warning = %v, want %v: %v", warned, tc.wantWarn, resp.Diagnostics)
			}
		})
	}
}
