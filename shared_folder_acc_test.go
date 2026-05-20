package main

import (
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestAccSharedFolder_basic(t *testing.T) {
	t.Skip("skipped: DSM in first-login setup state, resource creation blocked")
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_shared_folder" "test" {
  name        = "tfacctestfolder"
  vol_path    = "/volume1"
  description = "Acceptance test folder"
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "name", "tfacctestfolder"),
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "vol_path", "/volume1"),
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "description", "Acceptance test folder"),
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "hidden", "false"),
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "enable_recycle_bin", "true"),
					resource.TestCheckResourceAttrSet("dsm_shared_folder.test", "id"),
					resource.TestCheckResourceAttrSet("dsm_shared_folder.test", "uuid"),
				),
			},
		},
	})
}

func TestAccSharedFolder_import(t *testing.T) {
	t.Skip("skipped: DSM in first-login setup state, resource creation blocked")
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_shared_folder" "test" {
  name     = "tfacctestfolderimp"
  vol_path = "/volume1"
}
`),
				ResourceName:      "dsm_shared_folder.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}
