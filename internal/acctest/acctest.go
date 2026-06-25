package acctest

import (
	"fmt"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

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

func TestAccProviderFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"dsm": func() (tfprotov6.ProviderServer, error) {
			return providerserver.NewProtocol6(provider.New())(), nil
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
