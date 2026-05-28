variable "access_token" {
  type        = string
  description = "Access token of the project."
  sensitive   = true
}
variable "access_token_secret" {
  type        = string
  description = "Access token secret of the project."
  sensitive   = true
}
variable "container_registry_image" {
  type        = string
  description = "Container registry image."
  default     = "creatio313-live-streaming.sakuracr.jp/archive-notifier:v0"
}
variable "email_address" {
  type        = string
  description = "Email address to receive notifications."
  sensitive   = true
}
variable "service_principal_key_kid" {
  type        = string
  description = "KID of the service principal key used for JWT header."
  sensitive   = true
}
variable "service_principal_resource_id" {
  type        = string
  description = "Resource ID of the service principal used for JWT iss/sub."
  sensitive   = true
}
variable "service_principal_private_key_pem_path" {
  type        = string
  description = "Path to the private key PEM file. The file content is injected into AppRun env var."
  sensitive   = true
}
variable "service_principal_private_key_b64_chunk_size" {
  type        = number
  description = "Chunk size for split private key base64 env vars. Lower this if AppRun rejects long env var values."
  default     = 500
  validation {
    condition     = var.service_principal_private_key_b64_chunk_size > 0
    error_message = "service_principal_private_key_b64_chunk_size must be greater than 0."
  }
}
variable "zone" {
  type        = string
  description = "Zone to build resources."
  default     = "is1c"
}