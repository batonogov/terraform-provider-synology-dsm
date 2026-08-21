package main

import (
	"fmt"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// The firewall acceptance tests are gated behind DSM_ACC_FIREWALL=1 and are the
// only tests in this suite that can make the NAS under test unreachable.
//
// They are deliberately conservative about what they touch. Every step leaves
// the global switch OFF: turning a real firewall on from a test run is the one
// operation whose failure mode is "drive to the building". What is exercised
// instead is everything that can be exercised safely — that the resource can be
// created, imported and read back, that the reconstructed write reaches DSM at
// all, and that the default policy round-trips through adapterPolicyMap.
//
// Switching the firewall on is covered by the client's unit tests, where the
// lockout guard can be given a rule set that proves it fires.

func firewallAccConfig(policy string) string {
	return fmt.Sprintf(`
resource "dsm_firewall" "test" {
  profile = "default"
  enabled = false

  default_policy = {
    global = %q
  }
}
`, policy)
}

func TestAccFirewall_basic(t *testing.T) {
	acctest.TestAccPreCheckFirewall(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(firewallAccConfig("none")),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_firewall.test", "id", "default"),
					resource.TestCheckResourceAttr("dsm_firewall.test", "profile", "default"),
					resource.TestCheckResourceAttr("dsm_firewall.test", "enabled", "false"),
					resource.TestCheckResourceAttr("dsm_firewall.test", "active_profile", "default"),
					resource.TestCheckResourceAttr("dsm_firewall.test", "default_policy.global", "none"),
					// The computed map reports every adapter, not only the managed one.
					resource.TestCheckResourceAttrSet("dsm_firewall.test", "default_policy_effective.%"),
					resource.TestCheckResourceAttr("dsm_firewall.test", "disable_on_destroy", "false"),
				),
			},
		},
	})
}

// The default policy is what issue #123 is about: it has to survive a write and
// come back as the same value, through DSM's adapterPolicyMap.
func TestAccFirewall_defaultPolicy(t *testing.T) {
	acctest.TestAccPreCheckFirewall(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(firewallAccConfig("allow")),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_firewall.test", "default_policy.global", "allow"),
					resource.TestCheckResourceAttr("dsm_firewall.test", "default_policy_effective.global", "allow"),
				),
			},
			{
				// An in-place change of the fall-through, with the firewall still off
				// so nothing about reachability can change underneath the test.
				Config: acctest.ComposeTestResourceConfig(firewallAccConfig("none")),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_firewall.test", "default_policy.global", "none"),
					resource.TestCheckResourceAttr("dsm_firewall.test", "default_policy_effective.global", "none"),
				),
			},
		},
	})
}

func TestAccFirewall_import(t *testing.T) {
	acctest.TestAccPreCheckFirewall(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(firewallAccConfig("none")),
			},
			{
				ResourceName:      "dsm_firewall.test",
				ImportState:       true,
				ImportStateId:     "default",
				ImportStateVerify: true,
				// default_policy is the configuration's chosen subset of adapters and
				// is deliberately left null by an import: there is no way to know from
				// DSM which adapters a configuration meant to manage, and adopting all
				// of them would put unmanaged adapters into state.
				ImportStateVerifyIgnore: []string{"default_policy"},
			},
		},
	})
}

// Reading the firewall is safe and needs no opt-in beyond the ordinary
// acceptance environment, so the data source half of #123 is covered here.
func TestAccDataSourceFirewallRules_defaultPolicy(t *testing.T) {
	acctest.TestAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(`
data "dsm_firewall_rules" "test" {
  profile = "default"
}
`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrSet("data.dsm_firewall_rules.test", "firewall_enabled"),
					resource.TestCheckResourceAttrSet("data.dsm_firewall_rules.test", "default_policy.%"),
				),
			},
		},
	})
}
