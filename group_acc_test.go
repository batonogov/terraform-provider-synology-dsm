package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestAccGroup_basic(t *testing.T) {
	t.Skip("skipped: DSM in first-login setup state, resource creation blocked")
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_group" "test" {
  name        = "tf-acc-test-group"
  description = "Acceptance test group"
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_group.test", "name", "tf-acc-test-group"),
					resource.TestCheckResourceAttr("dsm_group.test", "description", "Acceptance test group"),
					resource.TestCheckResourceAttrSet("dsm_group.test", "id"),
					resource.TestCheckResourceAttrSet("dsm_group.test", "gid"),
				),
			},
		},
	})
}

func TestAccGroup_import(t *testing.T) {
	t.Skip("skipped: DSM in first-login setup state, resource creation blocked")
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_group" "test" {
  name        = "tf-acc-test-group-imp"
  description = "Import test group"
}
`),
				ResourceName:      "dsm_group.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccDataSourceGroup_basic(t *testing.T) {
	acctest.TestAccPreCheck(t)
	groupName := os.Getenv("DSM_ACC_GROUP_NAME")
	if groupName == "" {
		groupName = "administrators"
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(fmt.Sprintf(`
data "dsm_group" "test" {
  name = %q
}
`, groupName)),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.dsm_group.test", "name", groupName),
					resource.TestCheckResourceAttrSet("data.dsm_group.test", "id"),
					resource.TestCheckResourceAttrSet("data.dsm_group.test", "gid"),
				),
			},
		},
	})
}
