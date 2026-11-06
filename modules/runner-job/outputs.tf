output "job_id" {
  description = "Full job name, for sqlmigrate_migrations.job."
  value       = google_cloud_run_v2_job.this.id
}

output "bucket" {
  description = "Bucket for sqlmigrate_migrations.bucket."
  value       = local.bucket
}

output "prefix" {
  description = "Prefix for sqlmigrate_migrations.prefix."
  value       = var.prefix
}

output "service_account_email" {
  description = "Runner service account. With db_auth = \"google-access-token\", add it to the database as an IAM user."
  value       = google_service_account.runner.email
}

output "db_user" {
  description = "Database login the runner uses."
  value       = local.db_user
}
