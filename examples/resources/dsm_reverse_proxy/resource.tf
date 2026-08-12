resource "dsm_reverse_proxy" "nextcloud" {
  description = "Nextcloud"

  source {
    protocol = "HTTPS"
    hostname = "cloud.example.com"
    port     = 443
  }

  destination {
    protocol = "HTTP"
    hostname = "localhost"
    port     = 8080
  }

  websocket = true
  http2     = true
  hsts      = true

  # Nextcloud and most single-page apps need the forwarded headers to build
  # correct absolute URLs behind the proxy.
  custom_headers = {
    "X-Forwarded-Proto" = "$scheme"
    "X-Forwarded-Host"  = "$host"
    "X-Forwarded-For"   = "$proxy_add_x_forwarded_for"
    "X-Real-IP"         = "$remote_addr"
  }

  # Optional: an access control profile that already exists in
  # Control Panel -> Login Portal -> Advanced -> Access Control Profile.
  access_control_profile = "internal-only"
}

# A minimal entry: no websocket, no custom headers, default timeouts.
resource "dsm_reverse_proxy" "metrics" {
  description = "Prometheus"

  source {
    protocol = "HTTPS"
    hostname = "metrics.example.com"
    port     = 443
  }

  destination {
    protocol = "HTTP"
    hostname = "127.0.0.1"
    port     = 9090
  }

  hsts = true
}
