package provider

import (
	"context"
	"fmt"
	"regexp"
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
	factory := providerserver.NewProtocol6(New("test")())
	server, err := factory().(tfprotov6.ProviderServer), error(nil)
	if err != nil {
		t.Fatalf("unexpected factory error: %v", err)
	}
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
	for _, name := range []string{"innfactoryhub_api_key", "innfactoryhub_environment"} {
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
		"endpoint":  tftypes.String,
		"admin_key": tftypes.String,
	}}, map[string]tftypes.Value{
		"endpoint":  str(endpoint),
		"admin_key": str(adminKey),
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
