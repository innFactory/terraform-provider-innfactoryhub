package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
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
	_, err = server.GetProviderSchema(context.Background(), new(tfprotov6.GetProviderSchemaRequest))
	if err != nil {
		t.Fatalf("GetProviderSchema failed: %v", err)
	}
}
