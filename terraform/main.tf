resource "sakura_simple_notification_destination" "notification_target_for_imageflux_live_streaming_archive_notifier" {
  name        = "通知先メールアドレス"
  description = "ImageFlux Live Streamingアーカイブ完了時の通知先メールアドレス"
  type        = "email"
  value       = var.email_address
}

resource "sakura_simple_notification_group" "notification_group_for_imageflux_live_streaming_archive_notifier" {
  name         = "ImageFlux Live Streamingアーカイブ通知グループ"
  description  = "ImageFlux Live Streamingアーカイブ完了時の通知グループ"
  destinations = [sakura_simple_notification_destination.notification_target_for_imageflux_live_streaming_archive_notifier.id]
}
//1変数で秘密鍵の値を保持できないため、分割
locals {
  service_principal_private_key_pem_content     = file(var.service_principal_private_key_pem_path)
  service_principal_private_key_pem_b64         = base64encode(local.service_principal_private_key_pem_content)
  service_principal_private_key_b64_chunk_count = ceil(length(local.service_principal_private_key_pem_b64) / var.service_principal_private_key_b64_chunk_size)
  service_principal_private_key_b64_chunk_envs = [for i in range(local.service_principal_private_key_b64_chunk_count) : {
    key   = format("SERVICE_PRINCIPAL_PRIVATE_KEY_PEM_B64_%03d", i)
    value = substr(local.service_principal_private_key_pem_b64, i * var.service_principal_private_key_b64_chunk_size, var.service_principal_private_key_b64_chunk_size)
  }]
}

resource "sakura_apprun_shared" "imageflux_live_streaming_archive_notifier" {
  name = "ImageFlux Live Streamingアーカイブ通知機能"

  max_scale       = 3
  min_scale       = 1
  port            = 8080
  timeout_seconds = 60

  components = [{
    name       = "ImageFlux Live Streamingアーカイブ通知コンテナ"
    max_cpu    = "0.5"
    max_memory = "1Gi"
    deploy_source = {
      container_registry = {
        image = var.container_registry_image
      }
    }
    //あとで決める環境変数
    env = concat([
      {
        key   = "SERVICE_PRINCIPAL_KEY_KID"
        value = var.service_principal_key_kid
      },
      {
        key   = "SERVICE_PRINCIPAL_RESOURCE_ID"
        value = var.service_principal_resource_id
      },
      {
        key   = "SERVICE_PRINCIPAL_PRIVATE_KEY_PEM_B64_CHUNK_COUNT"
        value = tostring(local.service_principal_private_key_b64_chunk_count)
      },
      {
        key   = "NOTIFICATION_GROUP_ID"
        value = sakura_simple_notification_group.notification_group_for_imageflux_live_streaming_archive_notifier.id
      }
    ], local.service_principal_private_key_b64_chunk_envs)
  }]
  traffics = [{
    version_index = 0
    percent       = 100
  }]
}
