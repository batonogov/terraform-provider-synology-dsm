package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func containerProjectAccConfig(running bool) string {
	return fmt.Sprintf(`
resource "dsm_package" "container_manager" {
  name                 = "ContainerManager"
  running              = true
  uninstall_on_destroy = false
}

resource "dsm_shared_folder" "container_projects" {
  name     = "tfacctest-container-projects"
  vol_path = "/volume1"
}

resource "dsm_container_project" "test" {
  name       = "tfacctest-container-project"
  share_path = "/${dsm_shared_folder.container_projects.name}/project"
  compose_yaml = <<-YAML
    services:
      test:
        image: %s
        command: ["sh", "-c", "while true; do sleep 3600; done"]
        restart: unless-stopped
  YAML

  running           = %t
  delete_on_destroy = true
  depends_on        = [dsm_package.container_manager]
}
`, os.Getenv("DSM_ACC_CONTAINER_IMAGE"), running)
}

func TestAccContainerProject_basic(t *testing.T) {
	acctest.TestAccPreCheckContainerProject(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(containerProjectAccConfig(true)),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrSet("dsm_container_project.test", "id"),
					resource.TestCheckResourceAttr("dsm_container_project.test", "name", "tfacctest-container-project"),
					resource.TestCheckResourceAttr("dsm_container_project.test", "share_path", "/tfacctest-container-projects/project"),
					resource.TestCheckResourceAttr("dsm_container_project.test", "running", "true"),
					resource.TestCheckResourceAttr("dsm_container_project.test", "delete_on_destroy", "true"),
					resource.TestCheckResourceAttrSet("dsm_container_project.test", "status"),
				),
			},
			{
				Config: acctest.ComposeTestResourceConfig(containerProjectAccConfig(false)),
				Check:  resource.TestCheckResourceAttr("dsm_container_project.test", "running", "false"),
			},
			{
				Config: acctest.ComposeTestResourceConfig(containerProjectAccConfig(true)),
				Check:  resource.TestCheckResourceAttr("dsm_container_project.test", "running", "true"),
			},
		},
	})
}

func TestAccContainerProject_import(t *testing.T) {
	acctest.TestAccPreCheckContainerProject(t)
	config := acctest.ComposeTestResourceConfig(containerProjectAccConfig(true))
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  resource.TestCheckResourceAttrSet("dsm_container_project.test", "id"),
			},
			{
				ResourceName:            "dsm_container_project.test",
				ImportState:             true,
				ImportStateId:           "tfacctest-container-project",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"delete_on_destroy"},
			},
			{
				// Restore the destructive test-only cleanup flag after import;
				// provider-only flags correctly default to false in imported state.
				Config: config,
				Check:  resource.TestCheckResourceAttr("dsm_container_project.test", "delete_on_destroy", "true"),
			},
		},
	})
}

func TestAccDataSourceContainerProject_basic(t *testing.T) {
	acctest.TestAccPreCheckContainerProject(t)
	config := containerProjectAccConfig(true) + `
data "dsm_container_project" "test" {
  name       = dsm_container_project.test.name
  depends_on = [dsm_container_project.test]
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(config),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrSet("data.dsm_container_project.test", "id"),
					resource.TestCheckResourceAttr("data.dsm_container_project.test", "name", "tfacctest-container-project"),
					resource.TestCheckResourceAttr("data.dsm_container_project.test", "share_path", "/tfacctest-container-projects/project"),
					resource.TestCheckResourceAttr("data.dsm_container_project.test", "running", "true"),
					resource.TestCheckResourceAttrSet("data.dsm_container_project.test", "status"),
				),
			},
		},
	})
}
