package main

import (
	"context"
	"regexp"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
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
					// Read-only, from File Station (issue #94). The value itself
					// differs per DSM — virtual DSM reports 777 where a DS1525+ in
					// ACL mode reports 000 — so only its presence is asserted.
					// These three need the File Station package on the target NAS;
					// without it the attributes are null by design and this step
					// fails, which is the one thing to check before blaming the
					// shared folder itself.
					resource.TestCheckResourceAttrSet("dsm_shared_folder.test", "posix_mode"),
					resource.TestCheckResourceAttrSet("dsm_shared_folder.test", "posix_owner"),
					resource.TestCheckResourceAttrSet("dsm_shared_folder.test", "acl_mode"),
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

// TestAccSharedFolder_adoptExisting covers #53: a shared folder that already
// exists on the NAS — created by hand, left by a failed apply, or made by DSM
// itself — can be brought under management instead of failing with 3301.
//
// The folder is created outside Terraform in PreConfig, which is the situation
// being fixed; creating it through a first Terraform step would put it in state
// and never exercise adoption.
func TestAccSharedFolder_adoptExisting(t *testing.T) {
	acctest.TestAccPreCheck(t)

	const name = "tfacctestadopt"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					c, err := sweeperClient()
					if err != nil {
						t.Fatalf("build DSM client: %v", err)
					}
					if _, err := c.CreateShare(context.Background(), client.CreateShareRequest{
						Name:             name,
						VolPath:          "/volume1",
						Description:      "Created outside Terraform",
						EnableRecycleBin: true,
					}); err != nil {
						t.Fatalf("pre-create share outside Terraform: %v", err)
					}
				},
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_shared_folder" "adopted" {
  name               = "` + name + `"
  vol_path           = "/volume1"
  description        = "Adopted by Terraform"
  adopt_existing     = true
  enable_recycle_bin = false
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_shared_folder.adopted", "name", name),
					resource.TestCheckResourceAttr("dsm_shared_folder.adopted", "adopt_existing", "true"),
					// The configuration must have been applied to the adopted folder,
					// not merely recorded: both of these differ from how it was created.
					resource.TestCheckResourceAttr("dsm_shared_folder.adopted", "description", "Adopted by Terraform"),
					resource.TestCheckResourceAttr("dsm_shared_folder.adopted", "enable_recycle_bin", "false"),
					resource.TestCheckResourceAttrSet("dsm_shared_folder.adopted", "uuid"),
				),
			},
			// An adopted folder must behave like any other managed one afterwards.
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_shared_folder" "adopted" {
  name               = "` + name + `"
  vol_path           = "/volume1"
  description        = "Adopted by Terraform"
  adopt_existing     = true
  enable_recycle_bin = false
}
`),
				PlanOnly: true,
			},
		},
	})
}

// TestAccSharedFolder_existingWithoutAdoptFails is the other half of #53: with
// adopt_existing left at its default, a collision must still fail — and say how
// to resolve it rather than reporting a bare 3301.
func TestAccSharedFolder_existingWithoutAdoptFails(t *testing.T) {
	acctest.TestAccPreCheck(t)

	const name = "tfacctestnoadopt"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					c, err := sweeperClient()
					if err != nil {
						t.Fatalf("build DSM client: %v", err)
					}
					if _, err := c.CreateShare(context.Background(), client.CreateShareRequest{
						Name: name, VolPath: "/volume1",
					}); err != nil {
						t.Fatalf("pre-create share outside Terraform: %v", err)
					}
				},
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_shared_folder" "strict" {
  name     = "` + name + `"
  vol_path = "/volume1"
}
`),
				ExpectError: regexp.MustCompile(`already exists[\s\S]*terraform import`),
			},
		},
	})
}
