package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
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
					// Read-only, from File Station (issue #94): the mode a
					// container bind-mounting this path would be subject to.
					resource.TestCheckResourceAttrSet("dsm_file.test", "posix_mode"),
					resource.TestCheckResourceAttrSet("dsm_file.test", "posix_owner"),
					resource.TestCheckResourceAttrSet("dsm_file.test", "posix_uid"),
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

func fileWriteOnlyAccConfig(content string, version int) string {
	return fmt.Sprintf(`
resource "dsm_shared_folder" "files" {
  name     = "tfacctestfileswo"
  vol_path = "/volume1"
}

resource "dsm_file" "secret" {
  share_path = "/${dsm_shared_folder.files.name}/conf"
  name       = "credentials"

  content_wo         = %q
  content_wo_version = %d
}
`, content, version)
}

// TestAccFile_writeOnlyContent covers issue #104 end to end: the secret reaches
// DSM, state never holds it, and the version counter is what decides when an
// edited value is written again.
func TestAccFile_writeOnlyContent(t *testing.T) {
	acctest.TestAccPreCheck(t)
	const initial = "first-secret\n"
	const rotated = "second-secret\n"
	checksum := func(content string) string {
		sum := sha256.Sum256([]byte(content))
		return hex.EncodeToString(sum[:])
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		// Write-only arguments are rejected by older Terraform CLIs.
		TerraformVersionChecks: []tfversion.TerraformVersionCheck{
			tfversion.SkipBelow(tfversion.Version1_11_0),
		},
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(fileWriteOnlyAccConfig(initial, 1)),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_file.secret", "checksum", checksum(initial)),
					resource.TestCheckResourceAttr("dsm_file.secret", "size", fmt.Sprint(len(initial))),
					resource.TestCheckNoResourceAttr("dsm_file.secret", "content"),
					resource.TestCheckNoResourceAttr("dsm_file.secret", "content_wo"),
					resource.TestCheckResourceAttr("dsm_file.secret", "content_wo_version", "1"),
				),
			},
			{
				// Same version, different value: Terraform has nothing to diff, so
				// the file is deliberately left as it is. This is the trade-off the
				// documentation warns about, and it must not change silently.
				Config: acctest.ComposeTestResourceConfig(fileWriteOnlyAccConfig(rotated, 1)),
				Check: resource.TestCheckResourceAttr(
					"dsm_file.secret", "checksum", checksum(initial),
				),
			},
			{
				Config: acctest.ComposeTestResourceConfig(fileWriteOnlyAccConfig(rotated, 2)),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_file.secret", "checksum", checksum(rotated)),
					resource.TestCheckNoResourceAttr("dsm_file.secret", "content"),
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
