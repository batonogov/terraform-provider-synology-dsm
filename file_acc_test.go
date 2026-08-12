package main

import (
	"fmt"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// fileAccConfig puts the file in a subdirectory of a freshly created share, so
// the test also exercises create_parents: DSM creates `conf`, while the shared
// folder itself must already exist.
func fileAccConfig(content string) string {
	return fmt.Sprintf(`
resource "dsm_shared_folder" "files" {
  name     = "tfacctestfiles"
  vol_path = "/volume1"
}

resource "dsm_file" "test" {
  share_path = "/${dsm_shared_folder.files.name}/conf"
  name       = "s3.json"
  content    = %q
}
`, content)
}

func TestAccFile_basic(t *testing.T) {
	acctest.TestAccPreCheck(t)
	const initial = "{\"identities\":[]}\n"
	const updated = "{\"identities\":[{\"name\":\"nextcloud\"}]}\n"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(fileAccConfig(initial)),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_file.test", "id", "/tfacctestfiles/conf/s3.json"),
					resource.TestCheckResourceAttr("dsm_file.test", "share_path", "/tfacctestfiles/conf"),
					resource.TestCheckResourceAttr("dsm_file.test", "name", "s3.json"),
					resource.TestCheckResourceAttr("dsm_file.test", "content", initial),
					resource.TestCheckResourceAttr("dsm_file.test", "size", fmt.Sprint(len(initial))),
					resource.TestCheckResourceAttrSet("dsm_file.test", "checksum"),
				),
			},
			{
				Config: acctest.ComposeTestResourceConfig(fileAccConfig(updated)),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_file.test", "content", updated),
					resource.TestCheckResourceAttr("dsm_file.test", "size", fmt.Sprint(len(updated))),
				),
			},
		},
	})
}

func TestAccFile_base64(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_shared_folder" "files" {
  name     = "tfacctestfilesbin"
  vol_path = "/volume1"
}

resource "dsm_file" "binary" {
  share_path     = "/${dsm_shared_folder.files.name}"
  name           = "payload.bin"
  content_base64 = "AAH/"
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_file.binary", "content_base64", "AAH/"),
					resource.TestCheckResourceAttr("dsm_file.binary", "size", "3"),
					resource.TestCheckNoResourceAttr("dsm_file.binary", "content"),
				),
			},
		},
	})
}

func TestAccFile_import(t *testing.T) {
	acctest.TestAccPreCheck(t)
	config := acctest.ComposeTestResourceConfig(fileAccConfig("imported\n"))

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  resource.TestCheckResourceAttrSet("dsm_file.test", "id"),
			},
			{
				Config:            config,
				ResourceName:      "dsm_file.test",
				ImportState:       true,
				ImportStateId:     "/tfacctestfiles/conf/s3.json",
				ImportStateVerify: true,
			},
		},
	})
}
