// Command terraform-provider-sqlmigrate is the sqlmigrate Terraform provider.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/provider"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// address is the provider's source address. To install a fork under another
// namespace (a private registry or mirror), change it here and in the Makefile.
const address = "registry.terraform.io/jq6l43d1/sqlmigrate"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run with support for debuggers such as delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: address,
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
