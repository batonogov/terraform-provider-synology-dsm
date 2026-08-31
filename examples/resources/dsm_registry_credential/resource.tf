# Registry credential for a private registry the NAS pulls images from.
# Password never enters state; rotate it by bumping password_wo_version.
resource "dsm_registry_credential" "harbor" {
  url                      = "https://registry.example.com"
  name                     = "registry.example.com"
  username                 = "robot$terraform"
  password_wo              = var.registry_password
  password_wo_version      = 1
  enable_trust_self_signed = false
}
