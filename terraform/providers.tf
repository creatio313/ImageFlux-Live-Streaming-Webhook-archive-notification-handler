provider "sakura" {
  default_zone = var.zone
  zone         = var.zone
  token        = var.access_token
  secret       = var.access_token_secret
}