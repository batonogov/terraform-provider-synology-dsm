package main

import (
	"context"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccDataSourceSystemSettings_basic only reads, so it runs in the default
// suite. It is also the cheapest check that SYNO.Core.Region.NTP `get` behaves
// as the client expects on whatever DSM is under test.
func TestAccDataSourceSystemSettings_basic(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
		data "dsm_system_settings" "test" {}
		`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.dsm_system_settings.test", "id", "system_settings"),
					// Every DSM has a time zone; the exact value depends on the host.
					resource.TestCheckResourceAttrSet("data.dsm_system_settings.test", "timezone"),
					resource.TestCheckResourceAttrSet("data.dsm_system_settings.test", "ntp_enabled"),
					resource.TestCheckResourceAttrSet("data.dsm_system_settings.test", "current_date"),
					resource.TestCheckResourceAttrSet("data.dsm_system_settings.test", "current_time"),
					resource.TestCheckResourceAttrSet("data.dsm_system_settings.test", "timestamp"),
				),
			},
		},
	})
}

// TestAccSystemSettings_basic writes the settings and reads them back. It
// restores whatever the NAS had before, so a run leaves the host as it found it.
func TestAccSystemSettings_basic(t *testing.T) {
	acctest.TestAccPreCheckSystemSettings(t)

	original := currentSystemSettings(t)
	t.Cleanup(func() { restoreSystemSettings(t, original) })

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_system_settings" "test" {
		  timezone    = "Moscow"
		  ntp_enabled = true
		  ntp_server  = "time.google.com"
		}
		`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_system_settings.test", "id", "system_settings"),
					resource.TestCheckResourceAttr("dsm_system_settings.test", "timezone", "Moscow"),
					resource.TestCheckResourceAttr("dsm_system_settings.test", "ntp_enabled", "true"),
					resource.TestCheckResourceAttr("dsm_system_settings.test", "ntp_server", "time.google.com"),
				),
			},
			// An in-place update of a single field: the others must survive,
			// which is what the read-modify-write in the client is for.
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_system_settings" "test" {
		  timezone    = "Amsterdam"
		  ntp_enabled = true
		  ntp_server  = "time.google.com"
		}
		`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_system_settings.test", "timezone", "Amsterdam"),
					resource.TestCheckResourceAttr("dsm_system_settings.test", "ntp_server", "time.google.com"),
				),
			},
		},
	})
}

// TestAccSystemSettings_timezoneOnly is the partial-management case: a config
// that names only the time zone must not disturb the NTP configuration.
func TestAccSystemSettings_timezoneOnly(t *testing.T) {
	acctest.TestAccPreCheckSystemSettings(t)

	original := currentSystemSettings(t)
	t.Cleanup(func() { restoreSystemSettings(t, original) })

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_system_settings" "test" {
		  timezone = "Moscow"
		}
		`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_system_settings.test", "timezone", "Moscow"),
					// Computed from DSM rather than from the config.
					resource.TestCheckResourceAttrSet("dsm_system_settings.test", "ntp_server"),
					resource.TestCheckResourceAttr("dsm_system_settings.test", "ntp_server", original.NTPServer),
				),
			},
		},
	})
}

func TestAccSystemSettings_import(t *testing.T) {
	acctest.TestAccPreCheckSystemSettings(t)

	original := currentSystemSettings(t)
	t.Cleanup(func() { restoreSystemSettings(t, original) })

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_system_settings" "test" {
		  timezone = "Moscow"
		}
		`),
			},
			{
				Config: acctest.ComposeTestResourceConfig(`
		resource "dsm_system_settings" "test" {
		  timezone = "Moscow"
		}
		`),
				ResourceName:      "dsm_system_settings.test",
				ImportState:       true,
				ImportStateId:     "system_settings",
				ImportStateVerify: true,
			},
		},
	})
}

// currentSystemSettings snapshots the NAS clock configuration so a test can put
// it back. Changing a NAS's time zone out from under whatever else uses it is
// not acceptable collateral damage from running a test suite.
func currentSystemSettings(t *testing.T) systemSettingsSnapshot {
	t.Helper()

	c := acctest.NewTestClient(t)
	settings, err := c.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("snapshot system settings: %v", err)
	}

	return systemSettingsSnapshot{
		Timezone:   settings.Timezone,
		NTPEnabled: settings.NTPEnabled,
		NTPServer:  settings.NTPServer,
	}
}

type systemSettingsSnapshot struct {
	Timezone   string
	NTPEnabled bool
	NTPServer  string
}

func restoreSystemSettings(t *testing.T, s systemSettingsSnapshot) {
	t.Helper()

	c := acctest.NewTestClient(t)
	_, err := c.SetSystemSettings(context.Background(), client.SetSystemSettingsRequest{
		Timezone:   &s.Timezone,
		NTPEnabled: &s.NTPEnabled,
		NTPServer:  &s.NTPServer,
	})
	if err != nil {
		t.Errorf("restore system settings to %+v: %v", s, err)
	}
}
