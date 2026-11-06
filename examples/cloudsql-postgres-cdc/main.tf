# Cloud SQL for PostgreSQL feeding Datastream: the stream can only be created
# after SQL has granted the CDC login access and created a publication, and
# that SQL can only run after Terraform has created the login. One apply:
#
#   google_sql_user (CDC login)
#   sqlmigrate_migrations.app     (job in the VPC runs migrations/)
#   google_datastream_stream      (depends_on the migrations)

terraform {
  required_version = ">= 1.9"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 7.24"
    }
    sqlmigrate = {
      source  = "jq6l43d1/sqlmigrate"
      version = "~> 0.1"
    }
  }
}

variable "project_id" {
  type = string
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "instance_name" {
  description = "Cloud SQL instance, with cloudsql.iam_authentication and cloudsql.logical_decoding on."
  type        = string
}

variable "db_host" {
  description = "Private hostname or IP of the instance (PSC endpoint or private IP) as seen from the VPC."
  type        = string
}

variable "db_name" {
  type    = string
  default = "app"
}

variable "network" {
  description = "VPC that can reach db_host and resolve its name."
  type        = string
}

variable "subnetwork" {
  description = "Subnet for the job's Direct VPC egress, with Private Google Access on."
  type        = string
}

variable "runner_image" {
  description = "Runner image in Artifact Registry, pinned by digest."
  type        = string
}

variable "terraform_service_account" {
  description = "Service account that runs Terraform."
  type        = string
}

provider "google" {
  project = var.project_id
  region  = var.region
}

# No credentials here: like the google provider, sqlmigrate uses Application
# Default Credentials (GOOGLE_APPLICATION_CREDENTIALS, including workload
# identity federation configs).
provider "sqlmigrate" {
  project = var.project_id
  region  = var.region
}

module "db_migrations" {
  source = "../../modules/runner-job"

  project_id = var.project_id
  region     = var.region
  name       = "app-db-migrate"
  image      = var.runner_image
  network    = var.network
  subnetwork = var.subnetwork

  # With IAM database authentication the runner logs in as its own service
  # account, using an access token as the password. No secret to manage.
  db_url  = "jdbc:postgresql://${var.db_host}:5432/${var.db_name}?sslmode=require"
  db_auth = "google-access-token"

  bucket_name = "${var.project_id}-db-migrations"
  prefix      = "${var.instance_name}/${var.db_name}"

  # For a database that already has objects but no Flyway history: the first
  # run records a baseline at version 0, then applies V1 onwards.
  flyway_env = {
    FLYWAY_BASELINE_ON_MIGRATE = "true"
    FLYWAY_BASELINE_VERSION    = "0"
  }

  deployer_members = ["serviceAccount:${var.terraform_service_account}"]

  labels = { app = "example" }
}

# The runner's login. cloudsqlsuperuser covers CREATE ROLE, ALTER ROLE ...
# REPLICATION and CREATE PUBLICATION.
resource "google_sql_user" "migrator" {
  project        = var.project_id
  instance       = var.instance_name
  name           = module.db_migrations.db_user
  type           = "CLOUD_IAM_SERVICE_ACCOUNT"
  database_roles = ["cloudsqlsuperuser"]
}

resource "sqlmigrate_migrations" "app" {
  source_dir = "${path.module}/migrations"
  bucket     = module.db_migrations.bucket
  prefix     = module.db_migrations.prefix
  job        = module.db_migrations.job_id

  placeholders = {
    cdc_user    = "datastream_reader"
    publication = "cdc_publication"
  }

  # Also depend on the CDC login (google_sql_user "datastream_reader") so it
  # exists before the GRANTs run.
  depends_on = [google_sql_user.migrator]
}

# The stream waits for the publication:
#
# resource "google_datastream_stream" "cdc" {
#   ...
#   depends_on = [sqlmigrate_migrations.app]
# }

output "applied" {
  value = sqlmigrate_migrations.app.applied
}
