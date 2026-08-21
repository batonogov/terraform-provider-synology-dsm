package acctest

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/batonogov/terraform-provider-synology-dsm/internal/provider"
)

func TestAccPreCheck(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set, skipping acceptance test")
	}
	if v := os.Getenv("SYNOLOGY_DSM_HOST"); v == "" {
		t.Fatal("SYNOLOGY_DSM_HOST must be set for acceptance tests")
	}
	if v := os.Getenv("SYNOLOGY_DSM_USERNAME"); v == "" {
		t.Fatal("SYNOLOGY_DSM_USERNAME must be set for acceptance tests")
	}
	if _, ok := os.LookupEnv("SYNOLOGY_DSM_PASSWORD"); !ok {
		t.Fatal("SYNOLOGY_DSM_PASSWORD must be set for acceptance tests (use empty value for first-login setup)")
	}
}

// TestAccPreCheckQuota gates the user-quota acceptance tests. The
// SYNO.Core.Share.Quota API returns error 102 ("not supported") on the
// virtual DSM (vdsm/virtual-dsm) used for local testing; it only works on real
// Synology hardware. Set DSM_ACC_QUOTA=1 to opt in (e.g. against a physical NAS).
func TestAccPreCheckQuota(t *testing.T) {
	TestAccPreCheck(t)
	if os.Getenv("DSM_ACC_QUOTA") != "1" {
		t.Skip("skipping user quota test: SYNO.Core.Share.Quota is not available on virtual DSM; set DSM_ACC_QUOTA=1 against real hardware")
	}
}

// TestAccPreCheckContainerProject gates tests that build and run a real
// Container Manager workload. Container Manager is unavailable on virtual DSM,
// and requiring an explicit image keeps the test independent of public-registry
// availability on a particular NAS.
func TestAccPreCheckContainerProject(t *testing.T) {
	TestAccPreCheck(t)
	if os.Getenv("DSM_ACC_CONTAINER_PROJECT") != "1" {
		t.Skip("skipping Container Manager project test: set DSM_ACC_CONTAINER_PROJECT=1 against compatible physical hardware")
	}
	if os.Getenv("DSM_ACC_CONTAINER_IMAGE") == "" {
		t.Skip("skipping Container Manager project test: set DSM_ACC_CONTAINER_IMAGE to an image the NAS can pull")
	}
}

// TestAccPreCheckSystemSettings gates the tests that WRITE the NAS date and
// time settings. Two reasons to make this opt-in:
//
//   - the SYNO.Core.Region.NTP `set` parameter set is inferred, not documented;
//     a firmware that wants a different shape answers 5701 and would otherwise
//     break the whole suite rather than one test;
//   - changing the time zone or NTP server of a NAS is disruptive to anything
//     else running on it, so it should never happen because someone ran the
//     default test target.
//
// Set DSM_ACC_SYSTEM_SETTINGS=1 to opt in. Reading the settings is safe and is
// not gated.
func TestAccPreCheckSystemSettings(t *testing.T) {
	TestAccPreCheck(t)
	if os.Getenv("DSM_ACC_SYSTEM_SETTINGS") != "1" {
		t.Skip("skipping system settings write test: it changes the NAS clock configuration and the SYNO.Core.Region.NTP set contract is unverified; set DSM_ACC_SYSTEM_SETTINGS=1 to opt in")
	}
}

// TestAccPreCheckReverseProxy gates the reverse proxy acceptance tests.
//
// SYNO.Core.AppPortal.ReverseProxy is undocumented and this provider's contract
// for it was reconstructed rather than captured first-hand, so these tests are
// opt-in: they write real nginx configuration and restart the DSM web service.
// Set DSM_ACC_REVERSE_PROXY=1 against a NAS where that is acceptable.
func TestAccPreCheckReverseProxy(t *testing.T) {
	TestAccPreCheck(t)
	if os.Getenv("DSM_ACC_REVERSE_PROXY") != "1" {
		t.Skip("skipping reverse proxy test: it rewrites the DSM reverse proxy configuration; set DSM_ACC_REVERSE_PROXY=1 to opt in")
	}
}

// TestAccPreCheckFirewall gates the firewall acceptance tests.
//
// These are the most dangerous tests in the suite, and the most heavily gated.
// dsm_firewall writes the global enable switch, which profile is in force, and
// that profile's default policy — every one of which can cut the test runner off
// from the NAS it is testing, with physical access as the only way back. On top
// of that the write side of SYNO.Core.Security.Firewall is reconstructed rather
// than captured, so an unexpected firmware fails these tests loudly instead of
// the rest of the suite quietly.
//
// Set DSM_ACC_FIREWALL=1 only against a NAS you can reach by console.
func TestAccPreCheckFirewall(t *testing.T) {
	TestAccPreCheck(t)
	if os.Getenv("DSM_ACC_FIREWALL") != "1" {
		t.Skip("skipping firewall test: it reconfigures the live DSM firewall and can lock the runner out of the NAS; set DSM_ACC_FIREWALL=1 to opt in")
	}
}

// NewTestClient builds a logged-in DSM client from the acceptance-test
// environment. Use it in CheckDestroy hooks that need to inspect DSM directly,
// beyond what the Terraform state can tell.
func NewTestClient(t *testing.T) *client.Client {
	t.Helper()

	c := client.NewClient(
		os.Getenv("SYNOLOGY_DSM_HOST"),
		os.Getenv("SYNOLOGY_DSM_USERNAME"),
		os.Getenv("SYNOLOGY_DSM_PASSWORD"),
		true,
	)
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("acceptance test client login: %v", err)
	}
	return c
}

func TestAccProviderFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"dsm": func() (tfprotov6.ProviderServer, error) {
			return providerserver.NewProtocol6(provider.New("test")())(), nil
		},
	}
}

func ProviderConfig() string {
	host := os.Getenv("SYNOLOGY_DSM_HOST")
	username := os.Getenv("SYNOLOGY_DSM_USERNAME")
	password := os.Getenv("SYNOLOGY_DSM_PASSWORD")

	return fmt.Sprintf(`
provider "dsm" {
  host     = %q
  username = %q
  password = %q
  insecure = true
}
`, host, username, password)
}

func ComposeTestResourceConfig(config string) string {
	return ProviderConfig() + config
}
