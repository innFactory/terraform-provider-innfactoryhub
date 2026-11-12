// Terraform provider entrypoint. Registered with the Plugin Framework's
// serve loop. Address must match the registry: registry.terraform.io/
// innfactory/innfactoryhub. The `debug` flag runs the provider as a debug
// server; terraform then attaches via TF_REATTACH_PROVIDERS.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/innFactory/terraform-provider-innfactoryhub/internal/provider"
)

// Set by GoReleaser via -ldflags at build time. Surfaced in the User-Agent
// every request sends so the Hub can see which provider version called.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "Run as a debug server (for terraform-plugin-debugging)")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/innfactory/innfactoryhub",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}
