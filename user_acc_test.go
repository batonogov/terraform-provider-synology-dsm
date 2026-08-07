package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestAccUser_basic(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_user" "test" {
  name        = "tfacctestuser"
  password    = "TestPass123!"
  description = "Acceptance test user"
  email       = "test@example.com"
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_user.test", "name", "tfacctestuser"),
					resource.TestCheckResourceAttr("dsm_user.test", "description", "Acceptance test user"),
					resource.TestCheckResourceAttr("dsm_user.test", "email", "test@example.com"),
					resource.TestCheckResourceAttrSet("dsm_user.test", "id"),
					resource.TestCheckResourceAttrSet("dsm_user.test", "uid"),
				),
			},
		},
	})
}

// TestAccUser_update is the regression test for the update path: SYNO.Core.User
// has no "update" method (it answers 103), so changes built that way silently
// did nothing. No acceptance test covered updates before, which is why it went
// unnoticed.
func TestAccUser_update(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_user" "test" {
  name        = "tfacctestuserupd"
  password    = "TestPass123!"
  description = "Before update"
  email       = "before@example.com"
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_user.test", "description", "Before update"),
					resource.TestCheckResourceAttr("dsm_user.test", "email", "before@example.com"),
					resource.TestCheckResourceAttr("dsm_user.test", "disabled", "false"),
				),
			},
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_user" "test" {
  name        = "tfacctestuserupd"
  password    = "TestPass123!"
  description = "After update"
  email       = "after@example.com"
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_user.test", "description", "After update"),
					resource.TestCheckResourceAttr("dsm_user.test", "email", "after@example.com"),
				),
			},
		},
	})
}

// TestAccUser_disabled covers the account state, which used to be sent as a
// "disabled" parameter that DSM 7 accepts and ignores — so disabled users were
// created active. It now travels in "expired".
func TestAccUser_disabled(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_user" "test" {
  name     = "tfacctestuserdis"
  password = "TestPass123!"
  disabled = true
}
`),
				Check: resource.TestCheckResourceAttr("dsm_user.test", "disabled", "true"),
			},
			// Re-enable in place.
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_user" "test" {
  name     = "tfacctestuserdis"
  password = "TestPass123!"
  disabled = false
}
`),
				Check: resource.TestCheckResourceAttr("dsm_user.test", "disabled", "false"),
			},
		},
	})
}

// TestAccUser_expireDate covers the expiry date, including the normalisation
// that keeps a configured YYYY-MM-DD from drifting against the YYYY/M/D DSM
// answers with.
func TestAccUser_expireDate(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_user" "test" {
  name        = "tfacctestuserexp"
  password    = "TestPass123!"
  expire_date = "2027-03-05"
}
`),
				Check: resource.ComposeTestCheckFunc(
					// A padded date must survive the round trip unchanged.
					resource.TestCheckResourceAttr("dsm_user.test", "expire_date", "2027-03-05"),
					resource.TestCheckResourceAttr("dsm_user.test", "disabled", "false"),
					resource.TestCheckResourceAttrSet("dsm_user.test", "two_factor_enabled"),
				),
			},
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_user" "test" {
  name        = "tfacctestuserexp"
  password    = "TestPass123!"
  expire_date = "2028-12-31"
}
`),
				Check: resource.TestCheckResourceAttr("dsm_user.test", "expire_date", "2028-12-31"),
			},
			// Dropping the date returns the account to never-expiring.
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_user" "test" {
  name     = "tfacctestuserexp"
  password = "TestPass123!"
}
`),
				Check: resource.TestCheckNoResourceAttr("dsm_user.test", "expire_date"),
			},
		},
	})
}

func TestAccUser_import(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			// Step 1: create the user so it exists and is in state.
			{
				Config: acctest.ComposeTestResourceConfig(`
resource "dsm_user" "test" {
  name     = "tfacctestuserimp"
  password = "TestPass123!"
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_user.test", "name", "tfacctestuserimp"),
				),
			},
			// Step 2: import and verify. password is write-only / not returned by
			// DSM, so it must be excluded from the import verification.
			{
				ResourceName:            "dsm_user.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"password"},
			},
		},
	})
}

func TestAccDataSourceUser_basic(t *testing.T) {
	acctest.TestAccPreCheck(t)
	userName := os.Getenv("DSM_ACC_USER_NAME")
	if userName == "" {
		userName = "admin"
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(fmt.Sprintf(`
data "dsm_user" "test" {
  name = %q
}
`, userName)),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.dsm_user.test", "name", userName),
					resource.TestCheckResourceAttrSet("data.dsm_user.test", "id"),
					resource.TestCheckResourceAttrSet("data.dsm_user.test", "uid"),
					resource.TestCheckResourceAttrSet("data.dsm_user.test", "two_factor_enabled"),
				),
			},
		},
	})
}
