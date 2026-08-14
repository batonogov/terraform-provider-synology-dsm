package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
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

// captureAttr records an attribute value so a later step can assert it changed.
// A write-only resource has no other way to prove that a write happened.
func captureAttr(resourceName, attribute string, into *string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		rs, ok := state.RootModule().Resources[resourceName]
		if !ok {
			return fmt.Errorf("resource %s not found in state", resourceName)
		}
		*into = rs.Primary.Attributes[attribute]
		if *into == "" {
			return fmt.Errorf("%s.%s is empty", resourceName, attribute)
		}
		return nil
	}
}

func containerProjectWriteOnlyAccConfig(version int) string {
	return fmt.Sprintf(`
resource "dsm_package" "container_manager" {
  name                 = "ContainerManager"
  running              = true
  uninstall_on_destroy = false
}

resource "dsm_shared_folder" "container_projects_wo" {
  name     = "tfacctest-container-projects-wo"
  vol_path = "/volume1"
}

resource "dsm_container_project" "secret" {
  name       = "tfacctest-container-project-wo"
  share_path = "/${dsm_shared_folder.container_projects_wo.name}/project"

  compose_yaml_wo = <<-YAML
    services:
      test:
        image: %s
        command: ["sh", "-c", "while true; do sleep 3600; done"]
        environment:
          SECRET: revision-%d
        restart: unless-stopped
  YAML

  compose_yaml_wo_version = %d
  delete_on_destroy       = true
  depends_on              = [dsm_package.container_manager]
}
`, os.Getenv("DSM_ACC_CONTAINER_IMAGE"), version, version)
}

// TestAccContainerProject_writeOnlyCompose is the compose side of issue #104:
// the document reaches Container Manager, state keeps only its checksum, and
// the version counter is what asks for a rebuild.
func TestAccContainerProject_writeOnlyCompose(t *testing.T) {
	acctest.TestAccPreCheckContainerProject(t)
	// The compose document is not in state, so the checksum recorded by the first
	// step is the only thing the second one can compare against.
	var deployedChecksum string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		// Write-only arguments are rejected by older Terraform CLIs.
		TerraformVersionChecks: []tfversion.TerraformVersionCheck{
			tfversion.SkipBelow(tfversion.Version1_11_0),
		},
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(containerProjectWriteOnlyAccConfig(1)),
				Check: resource.ComposeTestCheckFunc(
					// compose_yaml is an ordinary attribute, so its absence is a real
					// assertion — unlike compose_yaml_wo, which the framework nulls
					// out in every response whatever the provider does.
					resource.TestCheckNoResourceAttr("dsm_container_project.secret", "compose_yaml"),
					resource.TestCheckResourceAttr("dsm_container_project.secret", "compose_yaml_wo_version", "1"),
					captureAttr("dsm_container_project.secret", "compose_yaml_checksum", &deployedChecksum),
				),
			},
			{
				// A bumped counter is the only way to send an edited document — and
				// the checksum is what proves the write reached Container Manager.
				Config: acctest.ComposeTestResourceConfig(containerProjectWriteOnlyAccConfig(2)),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_container_project.secret", "compose_yaml_wo_version", "2"),
					resource.TestCheckNoResourceAttr("dsm_container_project.secret", "compose_yaml"),
					resource.TestCheckResourceAttrWith("dsm_container_project.secret", "compose_yaml_checksum", func(value string) error {
						if value == deployedChecksum {
							return fmt.Errorf("compose_yaml_checksum is still %s: the edited document never reached Container Manager", value)
						}
						return nil
					}),
				),
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
