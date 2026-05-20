package main

import (
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestAccUser_basic(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_user" "test" {
  name        = "tfacctestuser"
  password    = "TestPass123!"
  description = "Acceptance test user"
  email       = "test@example.com"
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_user.test", "name", "tfacctestuser"),
					resource.TestCheckResourceAttr("dsm_user.test", "description", "Acceptance test user"),
					resource.TestCheckResourceAttr("dsm_user.test", "email", "test@example.com"),
					resource.TestCheckResourceAttrSet("dsm_user.test", "id"),
					resource.TestCheckResourceAttrSet("dsm_user.test", "uid"),
				),
			},
		},
	})
}

func TestAccUser_import(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_user" "test" {
  name     = "tfacctestuserimp"
  password = "TestPass123!"
}
`),
				ResourceName:            "dsm_user.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"password"},
			},
		},
	})
}
