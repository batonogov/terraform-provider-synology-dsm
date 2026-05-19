package main

import (
	"context"
	"flag"
	"log"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "Enable debug mode for provider")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New, providerserver.ServeOpts{
		Address: "registry.terraform.io/batonogov/dsm",
		Debug:   debug,
	})
	if err != nil {
		log.Fatalf("provider error: %s", err)
	}
}
