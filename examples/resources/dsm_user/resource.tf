variable "user_password" {
  type      = string
  sensitive = true
}

resource "dsm_user" "example" {
  name        = "john.doe"
  password    = var.user_password
  description = "John Doe - Engineering"
  email       = "john.doe@example.com"
  groups      = ["users"]
}
