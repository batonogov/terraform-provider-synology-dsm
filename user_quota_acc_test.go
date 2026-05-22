package main

import (
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestAccUserQuota_basic(t *testing.T) {
	t.Skip("skipped: DSM in first-login setup state, resource creation blocked")
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

	resource "dsm_user_quota" "test" {
	  share_name = dsm_shared_folder.test.name
	  username   = "admin"
	  quota_size = 1073741824
	}
	`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_user_quota.test", "share_name", "tfacctestfolder"),
					resource.TestCheckResourceAttr("dsm_user_quota.test", "username", "admin"),
					resource.TestCheckResourceAttr("dsm_user_quota.test", "quota_size", "1073741824"),
					resource.TestCheckResourceAttrSet("dsm_user_quota.test", "id"),
					resource.TestCheckResourceAttrSet("dsm_user_quota.test", "quota_used"),
				),
			},
		},
	})
}

func TestAccUserQuota_import(t *testing.T) {
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

	resource "dsm_user_quota" "test" {
	  share_name = dsm_shared_folder.test.name
	  username   = "admin"
	  quota_size = 0
	}
	`),
				ResourceName:      "dsm_user_quota.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccDataSourceUserQuota_basic(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
	data "dsm_user_quota" "test" {
	  share_name = "homes"
	  username   = "admin"
	}
	`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.dsm_user_quota.test", "share_name", "homes"),
					resource.TestCheckResourceAttr("data.dsm_user_quota.test", "username", "admin"),
					resource.TestCheckResourceAttrSet("data.dsm_user_quota.test", "quota_size"),
					resource.TestCheckResourceAttrSet("data.dsm_user_quota.test", "id"),
				),
			},
		},
	})
}
