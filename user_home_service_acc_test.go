package main

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// regexpVolumePathHint matches the guidance the provider appends to DSM error
// 3101, which is what a bare volume name produces.
var regexpVolumePathHint = regexp.MustCompile(`3101`)

// TestAccUserHomeService_basic enables the service and verifies DSM reports it
// back. disable_on_destroy is left at its default (false), so the teardown of
// this test leaves the service on — which is what the other tests here assume.
func TestAccUserHomeService_basic(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_user_home_service" "test" {
		  location = "/volume1"
		}
		`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_user_home_service.test", "id", "user_home_service"),
					resource.TestCheckResourceAttr("dsm_user_home_service.test", "location", "/volume1"),
					resource.TestCheckResourceAttr("dsm_user_home_service.test", "enable", "true"),
					resource.TestCheckResourceAttr("dsm_user_home_service.test", "enable_recycle_bin", "false"),
					resource.TestCheckResourceAttr("dsm_user_home_service.test", "force", "false"),
					resource.TestCheckResourceAttr("dsm_user_home_service.test", "disable_on_destroy", "false"),
				),
			},
		},
	})
}

// TestAccUserHomeService_recycleBin covers an in-place update: flipping the
// recycle bin must not require replacement and must survive a refresh.
func TestAccUserHomeService_recycleBin(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_user_home_service" "test" {
		  location           = "/volume1"
		  enable_recycle_bin = false
		}
		`),
				Check: resource.TestCheckResourceAttr("dsm_user_home_service.test", "enable_recycle_bin", "false"),
			},
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_user_home_service" "test" {
		  location           = "/volume1"
		  enable_recycle_bin = true
		}
		`),
				Check: resource.TestCheckResourceAttr("dsm_user_home_service.test", "enable_recycle_bin", "true"),
			},
			// Back to the default so the stand is left as the other tests expect.
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_user_home_service" "test" {
		  location           = "/volume1"
		  enable_recycle_bin = false
		}
		`),
				Check: resource.TestCheckResourceAttr("dsm_user_home_service.test", "enable_recycle_bin", "false"),
			},
		},
	})
}

// TestAccUserHomeService_import verifies that an already-enabled service can be
// adopted into state. The singleton takes any import ID and normalises it.
func TestAccUserHomeService_import(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_user_home_service" "test" {
		  location = "/volume1"
		}
		`),
				Check: resource.TestCheckResourceAttrSet("dsm_user_home_service.test", "id"),
			},
			{
				ResourceName:      "dsm_user_home_service.test",
				ImportState:       true,
				ImportStateId:     "user_home_service",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccUserHomeService_homesShareCreated pins the side effect that makes this
// resource useful: enabling the service materialises the `homes` shared folder,
// which is what per-user home directories live in.
func TestAccUserHomeService_homesShareCreated(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_user_home_service" "test" {
		  location = "/volume1"
		}

		data "dsm_shared_folder" "homes" {
		  name       = "homes"
		  depends_on = [dsm_user_home_service.test]
		}
		`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.dsm_shared_folder.homes", "name", "homes"),
					resource.TestCheckResourceAttr("data.dsm_shared_folder.homes", "vol_path", "/volume1"),
				),
			},
		},
	})
}

func TestAccDataSourceUserHomeService_basic(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_user_home_service" "test" {
		  location = "/volume1"
		}

		data "dsm_user_home_service" "test" {
		  depends_on = [dsm_user_home_service.test]
		}
		`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.dsm_user_home_service.test", "id", "user_home_service"),
					resource.TestCheckResourceAttr("data.dsm_user_home_service.test", "enable", "true"),
					resource.TestCheckResourceAttr("data.dsm_user_home_service.test", "location", "/volume1"),
					resource.TestCheckResourceAttrSet("data.dsm_user_home_service.test", "enable_recycle_bin"),
					resource.TestCheckResourceAttrSet("data.dsm_user_home_service.test", "enable_domain"),
					resource.TestCheckResourceAttrSet("data.dsm_user_home_service.test", "enable_ldap"),
					resource.TestCheckResourceAttrSet("data.dsm_user_home_service.test", "encryption"),
					resource.TestCheckResourceAttrSet("data.dsm_user_home_service.test", "personal_photo_enable"),
				),
			},
		},
	})
}

// TestAccUserHomeService_destroyKeepsServiceEnabled pins the safe default: with
// disable_on_destroy unset, destroying the resource must leave DSM untouched.
func TestAccUserHomeService_destroyKeepsServiceEnabled(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		CheckDestroy:             checkUserHomeServiceEnabled(t, true),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_user_home_service" "test" {
		  location = "/volume1"
		}
		`),
				Check: resource.TestCheckResourceAttr("dsm_user_home_service.test", "enable", "true"),
			},
		},
	})
}

// TestAccUserHomeService_disableOnDestroy covers the opt-in path: with the flag
// set, destroy actually turns the service off. The service is switched back on
// afterwards so the stand is left as the other tests expect.
func TestAccUserHomeService_disableOnDestroy(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		CheckDestroy:             checkUserHomeServiceEnabled(t, false),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_user_home_service" "test" {
		  location           = "/volume1"
		  disable_on_destroy = true
		}
		`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_user_home_service.test", "enable", "true"),
					resource.TestCheckResourceAttr("dsm_user_home_service.test", "disable_on_destroy", "true"),
				),
			},
		},
	})
}

// checkUserHomeServiceEnabled asserts the post-destroy state of the service
// directly against DSM, then restores it to enabled so test order does not
// matter.
func checkUserHomeServiceEnabled(t *testing.T, want bool) resource.TestCheckFunc {
	t.Helper()
	return func(_ *terraform.State) error {
		c := acctest.NewTestClient(t)
		ctx := context.Background()

		svc, err := c.GetUserHomeService(ctx)
		if err != nil {
			return fmt.Errorf("read user home service after destroy: %w", err)
		}

		defer func() {
			if _, err := c.SetUserHomeService(ctx, client.SetUserHomeServiceRequest{
				Enable:   true,
				Location: "/volume1",
			}); err != nil {
				t.Logf("warning: could not restore the user home service: %v", err)
			}
		}()

		if svc.Enable != want {
			return fmt.Errorf("after destroy: enable = %v, want %v", svc.Enable, want)
		}
		return nil
	}
}

// TestAccUserHomeService_badLocation asserts the diagnostic for the most likely
// user mistake: a bare volume name instead of a path.
func TestAccUserHomeService_badLocation(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_user_home_service" "test" {
		  location = "volume1"
		}
		`),
				ExpectError: regexpVolumePathHint,
			},
		},
	})
}
