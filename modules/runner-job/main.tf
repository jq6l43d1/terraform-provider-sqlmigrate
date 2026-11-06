# The Cloud Run job that sqlmigrate_migrations starts, plus its bucket,
# service account and IAM. The job sits in the database's VPC, so it is the
# only thing that needs a network path to the database; Terraform itself only
# talks to Google APIs.

locals {
  bucket  = var.create_bucket ? google_storage_bucket.this[0].name : var.bucket_name
  db_user = var.db_auth == "google-access-token" ? trimsuffix(google_service_account.runner.email, ".gserviceaccount.com") : var.db_user

  env = merge(var.flyway_env, {
    SQLMIGRATE_BUCKET  = local.bucket
    SQLMIGRATE_PREFIX  = var.prefix
    SQLMIGRATE_DB_URL  = var.db_url
    SQLMIGRATE_DB_USER = local.db_user
    SQLMIGRATE_DB_AUTH = var.db_auth
  })
}

resource "google_service_account" "runner" {
  project      = var.project_id
  account_id   = "sqlm-${var.name}"
  display_name = "sqlmigrate runner (${var.name})"
  description  = "Runs Flyway migrations for ${var.prefix}. Reads bundles and writes the manifest in gs://${var.bucket_name}."
}

resource "google_storage_bucket" "this" {
  count = var.create_bucket ? 1 : 0

  project                     = var.project_id
  name                        = var.bucket_name
  location                    = coalesce(var.bucket_location, var.region)
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = false
  labels                      = var.labels

  # Run records and manifests are small and worth keeping; old versions are
  # pruned.
  versioning {
    enabled = true
  }

  lifecycle_rule {
    condition {
      days_since_noncurrent_time = 90
    }
    action {
      type = "Delete"
    }
  }
}

resource "google_storage_bucket_iam_member" "runner" {
  bucket = local.bucket
  role   = "roles/storage.objectUser"
  member = google_service_account.runner.member
}

resource "google_storage_bucket_iam_member" "deployers" {
  for_each = toset(var.deployer_members)

  bucket = local.bucket
  role   = "roles/storage.objectUser"
  member = each.value
}

resource "google_secret_manager_secret_iam_member" "runner" {
  count = var.db_password_secret == null ? 0 : 1

  project   = var.project_id
  secret_id = var.db_password_secret
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.runner.member
}

resource "google_cloud_run_v2_job" "this" {
  project             = var.project_id
  location            = var.region
  name                = var.name
  labels              = var.labels
  deletion_protection = var.deletion_protection

  template {
    task_count  = 1
    parallelism = 1

    template {
      service_account       = google_service_account.runner.email
      max_retries           = 0
      timeout               = var.task_timeout
      execution_environment = "EXECUTION_ENVIRONMENT_GEN2"

      vpc_access {
        egress = "ALL_TRAFFIC"
        network_interfaces {
          network    = var.network
          subnetwork = var.subnetwork
        }
      }

      containers {
        image = var.image

        resources {
          limits = {
            cpu    = var.cpu
            memory = var.memory
          }
        }

        dynamic "env" {
          for_each = local.env
          content {
            name  = env.key
            value = env.value
          }
        }

        dynamic "env" {
          for_each = var.db_password_secret == null ? [] : [var.db_password_secret]
          content {
            name = "SQLMIGRATE_DB_PASSWORD"
            value_source {
              secret_key_ref {
                secret  = env.value
                version = var.db_password_secret_version
              }
            }
          }
        }
      }
    }
  }

  depends_on = [
    google_storage_bucket_iam_member.runner,
    google_secret_manager_secret_iam_member.runner,
  ]

  lifecycle {
    precondition {
      condition     = var.db_auth != "password" || var.db_password_secret != null
      error_message = "db_auth = \"password\" needs db_password_secret."
    }
  }
}

# Deployers start runs with per-run overrides (bundle, run id, placeholders)
# and poll the execution. Both roles are scoped to this job.
resource "google_cloud_run_v2_job_iam_member" "deployers_run" {
  for_each = toset(var.deployer_members)

  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_job.this.name
  role     = "roles/run.jobsExecutorWithOverrides"
  member   = each.value
}

resource "google_cloud_run_v2_job_iam_member" "deployers_view" {
  for_each = toset(var.deployer_members)

  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_job.this.name
  role     = "roles/run.viewer"
  member   = each.value
}
