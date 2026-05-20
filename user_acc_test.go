package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestAccUser_basic(t *testing.T) {
	t.Skip("skipped: DSM in first-login setup state, resource creation blocked")
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
	t.Skip("skipped: DSM in first-login setup state, resource creation blocked")
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

func TestAccDataSourceUser_basic(t *testing.T) {
	acctest.TestAccPreCheck(t)
	userName := os.Getenv("DSM_ACC_USER_NAME")
	if userName == "" {
		userName = "admin"
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
%s
data "dsm_user" "test" {
  name = "%s"
}
`, acctest.ProviderConfig(), userName),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.dsm_user.test", "name", userName),
					resource.TestCheckResourceAttrSet("data.dsm_user.test", "id"),
					resource.TestCheckResourceAttrSet("data.dsm_user.test", "uid"),
				),
			},
		},
	})
}
