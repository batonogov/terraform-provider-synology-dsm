package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const registryCredentialConfig = `
resource "dsm_registry_credential" "test" {
  url                      = "https://registry.example.com"
  name                     = "registry.example.com"
  username                 = "robot$terraform"
  password_wo              = "throwaway-password"
  password_wo_version      = 1
  enable_trust_self_signed = false
}
`

// TestAccRegistryCredential_basic creates a registry entry, checks it back
// (the password must never surface), imports it by URL, and verifies the
// destroy removed it from the registry list.
func TestAccRegistryCredential_basic(t *testing.T) {
	acctest.TestAccPreCheckRegistry(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		CheckDestroy:             registryCredentialDestroyed(t),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(registryCredentialConfig),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_registry_credential.test", "id", "https://registry.example.com"),
					resource.TestCheckResourceAttr("dsm_registry_credential.test", "url", "https://registry.example.com"),
					resource.TestCheckResourceAttr("dsm_registry_credential.test", "name", "registry.example.com"),
					resource.TestCheckResourceAttr("dsm_registry_credential.test", "username", "robot$terraform"),
				),
			},
			{
				ResourceName:      "dsm_registry_credential.test",
				ImportState:       true,
				ImportStateId:     "https://registry.example.com",
				ImportStateVerify: true,
			},
		},
	})
}

// registryCredentialDestroyed asserts the entry is gone from the registry
// list. The credentials themselves are never pulled: only existence matters.
func registryCredentialDestroyed(t *testing.T) resource.TestCheckFunc {
	t.Helper()
	return func(s *terraform.State) error {
		c := acctest.NewTestClient(t)
		ctx := context.Background()
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "dsm_registry_credential" {
				continue
			}
			if _, err := c.GetRegistryByURL(ctx, rs.Primary.Attributes["url"]); err == nil {
				return fmt.Errorf("registry %q still exists", rs.Primary.Attributes["url"])
			} else if !errors.Is(err, client.ErrRegistryNotFound) {
				return fmt.Errorf("read registry after destroy: %w", err)
			}
		}
		return nil
	}
}
