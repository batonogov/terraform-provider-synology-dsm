package main

import (
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// FileStation is a system package present on both virtual DSM and physical DSM.
// These tests only adopt/read it; uninstall_on_destroy remains false, so the
// acceptance suite never attempts to remove a system component.
const packageAccConfig = `
resource "dsm_package" "test" {
  name                 = "FileStation"
  running              = true
  uninstall_on_destroy = false
}
`

func TestAccPackage_adoptExisting(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(packageAccConfig),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_package.test", "id", "FileStation"),
					resource.TestCheckResourceAttr("dsm_package.test", "name", "FileStation"),
					resource.TestCheckResourceAttr("dsm_package.test", "running", "true"),
					resource.TestCheckResourceAttr("dsm_package.test", "volume", "/volume1"),
					resource.TestCheckResourceAttr("dsm_package.test", "uninstall_on_destroy", "false"),
					resource.TestCheckResourceAttrSet("dsm_package.test", "version"),
					resource.TestCheckResourceAttrSet("dsm_package.test", "status"),
				),
			},
		},
	})
}

func TestAccPackage_import(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(packageAccConfig),
				Check:  resource.TestCheckResourceAttrSet("dsm_package.test", "version"),
			},
			{
				ResourceName:      "dsm_package.test",
				ImportState:       true,
				ImportStateId:     "FileStation",
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccDataSourcePackage_basic(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
data "dsm_package" "test" {
  name = "FileStation"
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.dsm_package.test", "id", "FileStation"),
					resource.TestCheckResourceAttr("data.dsm_package.test", "name", "FileStation"),
					resource.TestCheckResourceAttr("data.dsm_package.test", "running", "true"),
					resource.TestCheckResourceAttrSet("data.dsm_package.test", "version"),
					resource.TestCheckResourceAttrSet("data.dsm_package.test", "status"),
				),
			},
		},
	})
}
