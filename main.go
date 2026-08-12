package main

import (
	"context"
	"flag"
	"log"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "Enable debug mode for provider")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/batonogov/synology-dsm",
		Debug:   debug,
	})
	if err != nil {
		log.Fatalf("provider error: %s", err)
	}
}
