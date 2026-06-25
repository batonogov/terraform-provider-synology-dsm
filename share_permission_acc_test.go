package main

import (
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestAccSharePermission_basic(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_shared_folder" "test" {
		  name     = "tfacctestfolder"
		  vol_path = "/volume1"
		}

		resource "dsm_share_permission" "test" {
		  share_name      = dsm_shared_folder.test.name
		  user_group_type = "local_group"
		  principal_name  = "administrators"
		  permission      = "read_write"
		}
		`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_share_permission.test", "user_group_type", "local_group"),
					resource.TestCheckResourceAttr("dsm_share_permission.test", "principal_name", "administrators"),
					resource.TestCheckResourceAttr("dsm_share_permission.test", "permission", "read_write"),
					resource.TestCheckResourceAttrSet("dsm_share_permission.test", "id"),
				),
			},
		},
	})
}

func TestAccSharePermission_import(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			// Step 1: create the shared folder + permission.
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_shared_folder" "test" {
		  name     = "tfacctestfolderimp"
		  vol_path = "/volume1"
		}

		resource "dsm_share_permission" "test" {
		  share_name      = dsm_shared_folder.test.name
		  user_group_type = "local_group"
		  principal_name  = "administrators"
		  permission      = "read_only"
		}
		`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_share_permission.test", "permission", "read_only"),
				),
			},
			// Step 2: import and verify only the permission resource.
			{
				ResourceName:      "dsm_share_permission.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccDataSourceSharePermission_basic(t *testing.T) {
	acctest.TestAccPreCheck(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_shared_folder" "test" {
		  name     = "tfacctestfolderds"
		  vol_path = "/volume1"
		}

		data "dsm_share_permission" "test" {
		  share_name      = dsm_shared_folder.test.name
		  user_group_type = "local_group"
		  principal_name  = "administrators"
		}
		`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.dsm_share_permission.test", "share_name", "tfacctestfolderds"),
					resource.TestCheckResourceAttr("data.dsm_share_permission.test", "principal_name", "administrators"),
					resource.TestCheckResourceAttrSet("data.dsm_share_permission.test", "permission"),
					resource.TestCheckResourceAttrSet("data.dsm_share_permission.test", "id"),
				),
			},
		},
	})
}
