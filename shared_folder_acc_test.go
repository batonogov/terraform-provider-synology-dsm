package main

import (
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestAccSharedFolder_basic(t *testing.T) {
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

// TestAccSharedFolder_extendedAttributes covers the settings beyond name and
// description: compression, copy-on-write, the share-wide quota and the
// admin-only recycle bin.
func TestAccSharedFolder_extendedAttributes(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_shared_folder" "test" {
  name                   = "tfacctestfolderext"
  vol_path               = "/volume1"
  description            = "Extended attributes"
  hidden                 = true
  enable_recycle_bin     = true
  recycle_bin_admin_only = false
  enable_share_compress  = true
  enable_share_cow       = true
  share_quota            = 5
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "hidden", "true"),
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "recycle_bin_admin_only", "false"),
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "enable_share_compress", "true"),
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "enable_share_cow", "true"),
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "share_quota", "5"),
				),
			},
		},
	})
}

// TestAccSharedFolder_update is the regression test for the update path. It
// never existed before, which is why updates going through the wrong API method
// (create + name_org, rejected by DSM with 3301) went unnoticed.
//
// Only settings DSM accepts after creation are exercised here.
// enable_share_compress and enable_share_cow are creation-time only and force
// replacement when switched on — see TestAccSharedFolder_compressForcesReplace.
func TestAccSharedFolder_update(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_shared_folder" "test" {
  name                  = "tfacctestfolderupd"
  vol_path              = "/volume1"
  description           = "Before update"
  hidden                = false
  enable_share_compress = false
  share_quota           = 0
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "description", "Before update"),
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "hidden", "false"),
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "share_quota", "0"),
				),
			},
			// In-place update of everything DSM accepts after creation.
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_shared_folder" "test" {
  name                   = "tfacctestfolderupd"
  vol_path               = "/volume1"
  description            = "After update"
  hidden                 = true
  recycle_bin_admin_only = false
  enable_share_compress  = false
  share_quota            = 8
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "description", "After update"),
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "hidden", "true"),
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "recycle_bin_admin_only", "false"),
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "share_quota", "8"),
				),
			},
			// And back off again: clearing must stick as well as setting.
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_shared_folder" "test" {
  name                   = "tfacctestfolderupd"
  vol_path               = "/volume1"
  description            = "After update"
  hidden                 = false
  recycle_bin_admin_only = true
  enable_share_compress  = false
  share_quota            = 0
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "hidden", "false"),
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "recycle_bin_admin_only", "true"),
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "share_quota", "0"),
				),
			},
		},
	})
}

func TestAccSharedFolder_import(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			// Step 1: create the shared folder.
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_shared_folder" "test" {
  name     = "tfacctestfolderimp"
  vol_path = "/volume1"
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_shared_folder.test", "name", "tfacctestfolderimp"),
				),
			},
			// Step 2: import and verify.
			{
				ResourceName:      "dsm_shared_folder.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccDataSourceSharedFolder_basic covers the data source, which had no
// acceptance coverage at all, including the extended computed attributes.
func TestAccDataSourceSharedFolder_basic(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_shared_folder" "test" {
  name                  = "tfacctestfolderds"
  vol_path              = "/volume1"
  description           = "Data source test"
  enable_share_compress = true
  enable_share_cow      = true
  share_quota           = 3
}

data "dsm_shared_folder" "test" {
  name       = dsm_shared_folder.test.name
  depends_on = [dsm_shared_folder.test]
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.dsm_shared_folder.test", "name", "tfacctestfolderds"),
					resource.TestCheckResourceAttr("data.dsm_shared_folder.test", "vol_path", "/volume1"),
					resource.TestCheckResourceAttr("data.dsm_shared_folder.test", "description", "Data source test"),
					resource.TestCheckResourceAttr("data.dsm_shared_folder.test", "enable_share_compress", "true"),
					resource.TestCheckResourceAttr("data.dsm_shared_folder.test", "share_quota", "3"),
					resource.TestCheckResourceAttrSet("data.dsm_shared_folder.test", "uuid"),
					resource.TestCheckResourceAttrSet("data.dsm_shared_folder.test", "enable_recycle_bin"),
					resource.TestCheckResourceAttrSet("data.dsm_shared_folder.test", "enable_share_cow"),
				),
			},
		},
	})
}

// TestAccSharedFolder_compressForcesReplace pins the plan behaviour for the
// creation-time-only flags: switching enable_share_compress on must be planned
// as a replacement, because DSM would otherwise report success while leaving
// the value untouched.
func TestAccSharedFolder_compressForcesReplace(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_shared_folder" "test" {
  name                  = "tfacctestfoldercmp"
  vol_path              = "/volume1"
  enable_share_compress = false
}
`),
				Check: resource.TestCheckResourceAttr("dsm_shared_folder.test", "enable_share_compress", "false"),
			},
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_shared_folder" "test" {
  name                  = "tfacctestfoldercmp"
  vol_path              = "/volume1"
  enable_share_compress = true
  enable_share_cow      = true
}
`),
				// The folder is destroyed and recreated, so the new value sticks.
				Check: resource.TestCheckResourceAttr("dsm_shared_folder.test", "enable_share_compress", "true"),
			},
		},
	})
}
