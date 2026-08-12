package main

import (
	"fmt"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// The source hostname must not collide with anything real on the target NAS:
// DSM rejects a duplicate frontend FQDN, and a live entry would shadow a
// working service.
const reverseProxyAccHost = "tfacctest-reverse-proxy.invalid"

func reverseProxyAccConfig(description string, destinationPort int, websocket bool) string {
	return fmt.Sprintf(`
resource "dsm_reverse_proxy" "test" {
  description = %q

  source {
    protocol = "HTTPS"
    hostname = %q
    port     = 4443
  }

  destination {
    protocol = "HTTP"
    hostname = "localhost"
    port     = %d
  }

  websocket = %t
  hsts      = true

  custom_headers = {
    "X-Forwarded-Proto" = "$scheme"
  }
}
`, description, reverseProxyAccHost, destinationPort, websocket)
}

func TestAccReverseProxy_basic(t *testing.T) {
	acctest.TestAccPreCheckReverseProxy(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: acctest.ComposeTestResourceConfig(reverseProxyAccConfig("tfacctest-reverse-proxy", 18080, true)),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrSet("dsm_reverse_proxy.test", "id"),
					resource.TestCheckResourceAttr("dsm_reverse_proxy.test", "description", "tfacctest-reverse-proxy"),
					resource.TestCheckResourceAttr("dsm_reverse_proxy.test", "source.protocol", "HTTPS"),
					resource.TestCheckResourceAttr("dsm_reverse_proxy.test", "source.hostname", reverseProxyAccHost),
					resource.TestCheckResourceAttr("dsm_reverse_proxy.test", "source.port", "4443"),
					resource.TestCheckResourceAttr("dsm_reverse_proxy.test", "destination.protocol", "HTTP"),
					resource.TestCheckResourceAttr("dsm_reverse_proxy.test", "destination.port", "18080"),
					resource.TestCheckResourceAttr("dsm_reverse_proxy.test", "websocket", "true"),
					resource.TestCheckResourceAttr("dsm_reverse_proxy.test", "hsts", "true"),
					resource.TestCheckResourceAttr("dsm_reverse_proxy.test", "custom_headers.X-Forwarded-Proto", "$scheme"),
					resource.TestCheckResourceAttr("dsm_reverse_proxy.test", "proxy_connect_timeout", "60"),
				),
			},
			{
				// In-place update: a new destination port and the websocket headers off.
				Config: acctest.ComposeTestResourceConfig(reverseProxyAccConfig("tfacctest-reverse-proxy", 18081, false)),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("dsm_reverse_proxy.test", "destination.port", "18081"),
					resource.TestCheckResourceAttr("dsm_reverse_proxy.test", "websocket", "false"),
					resource.TestCheckResourceAttr("dsm_reverse_proxy.test", "custom_headers.X-Forwarded-Proto", "$scheme"),
				),
			},
		},
	})
}

func TestAccReverseProxy_import(t *testing.T) {
	acctest.TestAccPreCheckReverseProxy(t)
	config := acctest.ComposeTestResourceConfig(reverseProxyAccConfig("tfacctest-reverse-proxy-import", 18082, true))
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  resource.TestCheckResourceAttrSet("dsm_reverse_proxy.test", "id"),
			},
			{
				Config:            config,
				ResourceName:      "dsm_reverse_proxy.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccDataSourceReverseProxy_basic(t *testing.T) {
	acctest.TestAccPreCheckReverseProxy(t)
	config := acctest.ComposeTestResourceConfig(reverseProxyAccConfig("tfacctest-reverse-proxy-ds", 18083, true) + `
data "dsm_reverse_proxy" "test" {
  description = dsm_reverse_proxy.test.description
}
`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.TestAccProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.dsm_reverse_proxy.test", "id", "dsm_reverse_proxy.test", "id"),
					resource.TestCheckResourceAttr("data.dsm_reverse_proxy.test", "source_hostname", reverseProxyAccHost),
					resource.TestCheckResourceAttr("data.dsm_reverse_proxy.test", "destination_port", "18083"),
					resource.TestCheckResourceAttr("data.dsm_reverse_proxy.test", "websocket", "true"),
					resource.TestCheckResourceAttr("data.dsm_reverse_proxy.test", "custom_headers.X-Forwarded-Proto", "$scheme"),
				),
			},
		},
	})
}
