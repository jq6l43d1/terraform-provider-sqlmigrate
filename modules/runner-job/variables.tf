variable "project_id" {
  description = "Project that hosts the job, bucket and runner service account."
  type        = string
}

variable "region" {
  description = "Region for the Cloud Run job."
  type        = string
}

variable "name" {
  description = "Job name. Also used for the runner service account (sqlm-<name>, so keep it to 25 characters or fewer)."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,23}[a-z0-9]$", var.name))
    error_message = "name must be 2-25 lowercase letters, digits or dashes, starting with a letter."
  }
}

variable "image" {
  description = "Runner image built from this repo's Dockerfile, ideally pinned by digest (…/sqlmigrate-runner@sha256:…)."
  type        = string
}

variable "network" {
  description = "VPC network the job egresses into (name or self link). It must have a route to the database and DNS for its hostname."
  type        = string
}

variable "subnetwork" {
  description = "Subnetwork for Direct VPC egress. It needs Private Google Access, because the job reaches Cloud Storage and Secret Manager through the VPC."
  type        = string
}

variable "db_url" {
  description = "JDBC URL of the database, e.g. jdbc:postgresql://db.example.internal:5432/app?sslmode=require."
  type        = string

  validation {
    condition     = startswith(var.db_url, "jdbc:")
    error_message = "db_url must be a JDBC URL starting with jdbc:."
  }
}

variable "db_auth" {
  description = <<-EOT
    How the runner logs in to the database:
    "password" uses db_user and the password in db_password_secret;
    "google-access-token" logs in as the runner service account with its access token as the password
    (Cloud SQL or AlloyDB for PostgreSQL with IAM database authentication). db_user is then set for you.
  EOT
  type        = string
  default     = "password"

  validation {
    condition     = contains(["password", "google-access-token"], var.db_auth)
    error_message = "db_auth must be \"password\" or \"google-access-token\"."
  }
}

variable "db_user" {
  description = "Database login for db_auth = \"password\"."
  type        = string
  default     = ""
}

variable "db_password_secret" {
  description = "Secret Manager secret id (in project_id) holding the password, for db_auth = \"password\"."
  type        = string
  default     = null
}

variable "db_password_secret_version" {
  description = "Secret version to read at each execution."
  type        = string
  default     = "latest"
}

variable "bucket_name" {
  description = "Bucket for bundles, run records and the manifest. Created unless create_bucket is false."
  type        = string
}

variable "create_bucket" {
  description = "Create bucket_name. Set false to use an existing bucket."
  type        = bool
  default     = true
}

variable "bucket_location" {
  description = "Location for a created bucket. Defaults to region."
  type        = string
  default     = null
}

variable "prefix" {
  description = "Object prefix for this database's files in the bucket."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$", var.prefix)) && !can(regex("(^|/)\\.\\.?(/|$)", var.prefix))
    error_message = "prefix must be slash-separated segments of letters, digits, '.', '_' or '-', without '.' or '..' segments."
  }
}

variable "flyway_env" {
  description = "Extra FLYWAY_* settings for every run, e.g. { FLYWAY_SCHEMAS = \"app\", FLYWAY_BASELINE_ON_MIGRATE = \"true\" }. Connection settings are always taken from the db_* variables."
  type        = map(string)
  default     = {}

  validation {
    condition     = alltrue([for k in keys(var.flyway_env) : startswith(k, "FLYWAY_") && !contains(["FLYWAY_URL", "FLYWAY_USER", "FLYWAY_PASSWORD", "FLYWAY_LOCATIONS"], k)])
    error_message = "flyway_env keys must start with FLYWAY_ and must not set the URL, user, password or locations."
  }
}

variable "deployer_members" {
  description = <<-EOT
    Principals that run `terraform plan/apply` with sqlmigrate_migrations, e.g. ["serviceAccount:tf@….iam.gserviceaccount.com"].
    They get roles/run.jobsExecutorWithOverrides and roles/run.viewer on this job, and roles/storage.objectUser on the bucket.
  EOT
  type        = list(string)
  default     = []
}

variable "task_timeout" {
  description = "Maximum time for one run."
  type        = string
  default     = "1800s"
}

variable "cpu" {
  description = "CPU limit for the runner container."
  type        = string
  default     = "1"
}

variable "memory" {
  description = "Memory limit for the runner container (Flyway runs on the JVM)."
  type        = string
  default     = "2Gi"
}

variable "labels" {
  description = "Labels for the job and bucket."
  type        = map(string)
  default     = {}
}

variable "deletion_protection" {
  description = "Protect the job from deletion by Terraform."
  type        = bool
  default     = true
}
