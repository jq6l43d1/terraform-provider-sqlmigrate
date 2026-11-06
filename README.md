# terraform-provider-sqlmigrate

Run SQL migrations from Terraform, in the same apply as the infrastructure
they depend on, against databases Terraform cannot reach.

- **Terraform never connects to the database.** A Cloud Run job inside the
  database's VPC runs [Flyway](https://github.com/flyway/flyway) (open source,
  Apache 2.0). Terraform only calls Google APIs, so hosted agents work.
- **`terraform plan` shows pending migrations.** The job records the schema
  history in a Cloud Storage manifest, and the plan compares it with the
  local SQL files. No database access is needed.
- **The apply blocks.** The resource starts the job, waits for it, and fails
  with Flyway's error if a migration fails. Ordinary `depends_on` puts SQL
  between Terraform resources: for example, create a login, grant it with
  SQL, then create the Datastream stream that needs the grants.
- **Any database Flyway supports**, including PostgreSQL, MySQL, MariaDB, SQL
  Server and Oracle, reached over any network path the VPC has: PSC, private
  IP, or a VPN to on-prem.

## How it works

```
 terraform plan                                 terraform apply
 ──────────────                                 ───────────────
 read migrations/ ─┐                            upload bundle ──► gs://bucket/prefix/bundles/<sha>/
                   ├─► diff ─► "applied" list   run job (overrides: run id, bundle) ──► Cloud Run job (in VPC)
 read manifest ────┘     shows pending          wait for execution                       │ download bundle
 (gs://…/manifest.json)                         read runs/<id>.json ◄────────────────────┤ flyway migrate ──► database
                                                state = what is applied                  │ flyway info
                                                                                         └ write runs/<id>.json, manifest.json
```

| Object in the bucket | Written by | Purpose |
|---|---|---|
| `<prefix>/bundles/<sha256>/files/…` and `bundle.json` | provider | The SQL files for a run. Named by content hash, so each source version is uploaded once, and the runner checks every file's hash. |
| `<prefix>/runs/<run id>.json` | runner | The record of one run: status, Flyway error, migrations executed, and the schema history afterwards. |
| `<prefix>/manifest.json` | runner | The latest record that could read the schema history. The plan reads this file. |

The database's `flyway_schema_history` table stays the source of truth. The
manifest is a copy of it that Terraform can read. If the manifest is lost,
the next plan shows everything as pending, and the run then executes nothing
because Flyway sees it is all applied. The manifest is then rewritten.

## What it looks like

These outputs come from a real run: Terraform, the provider, the runner and
the Flyway CLI against SQLite through the local fake (`make demo`).

A new migration plus a changed repeatable view:

```
  # sqlmigrate_migrations.app will be updated in-place
  ~ resource "sqlmigrate_migrations" "app" {
      ~ applied        = [
            # (1 unchanged element hidden)
            "2: create orders",
          - "R: order counts (checksum 789192752)",
          + "3: add customer email",
          + "R: order counts (checksum -1432729234)",
        ]
        id             = "demo-bucket/demo/dev"
      ~ last_run_id    = "tf-20261106t152052-0787dce3" -> (known after apply)
      ~ schema_version = "2" -> (known after apply)
        # (5 unchanged attributes hidden)
    }
```

Nothing pending gives `No changes.`, and no job runs.

Editing a migration that already ran fails at plan time, before anything
touches the database:

```
Error: Migration cannot be applied

V1__create_customers.sql was changed after it was applied (checksum in the
database 1328984518, local -54305310). Applied versioned migrations must not
change; revert the edit and add a new migration instead.
```

A failing migration fails the apply. State keeps what did get applied, so the
next plan shows only the fix:

```
Error: Migration run failed

Flyway migrate failed in V4__index_orders.sql at line 2
(FAILED_VERSIONED_MIGRATION):

  Message    : [SQLITE_ERROR] SQL error or missing database (no such table: nowhere)
  Location   : V4__index_orders.sql (V4__index_orders.sql)
  Line       : 2

Execution: projects/demo/locations/us-central1/jobs/migrate/executions/migrate-3
Logs: https://console.cloud.google.com/logs/…
Run record: gs://demo-bucket/demo/dev/runs/tf-20261106t152103-242e482d.json
```

## Setting it up

### 1. Build the runner image

```sh
docker build -t us-central1-docker.pkg.dev/PROJECT/REPO/sqlmigrate-runner:0.1.0 .
docker push us-central1-docker.pkg.dev/PROJECT/REPO/sqlmigrate-runner:0.1.0
```

The image is the runner binary plus the Flyway 13.10.3 open-source CLI, with
its own Java runtime and JDBC drivers. The Dockerfile checks the Flyway
download against the SHA-256 published on GitHub. The image runs as a
non-root user. Pin the job to the pushed digest.

### 2. Create the job with `modules/runner-job`

```hcl
module "db_migrations" {
  source = "github.com/jq6l43d1/terraform-provider-sqlmigrate//modules/runner-job"

  project_id = "my-project"
  region     = "us-central1"
  name       = "app-dev-db-migrate"
  image      = "us-central1-docker.pkg.dev/…/sqlmigrate-runner@sha256:…"
  network    = "projects/host/global/networks/vpc"
  subnetwork = "projects/host/regions/us-central1/subnetworks/run-egress"

  db_url             = "jdbc:postgresql://db.app.internal:5432/app?sslmode=require"
  db_user            = "migrator"
  db_password_secret = "app-db-migrator-password"

  bucket_name      = "my-project-db-migrations"
  prefix           = "app-dev/app"
  deployer_members = ["serviceAccount:terraform@my-project.iam.gserviceaccount.com"]
}
```

It creates:
- the job, with Direct VPC egress (`ALL_TRAFFIC`), no retries and one task
- a runner service account
- the bucket: uniform access, public access prevention, versioning
- IAM, all scoped to the job, bucket and secret:

| Who | Role | Where |
|---|---|---|
| Runner service account | `roles/storage.objectUser` | the bucket |
| Runner service account | `roles/secretmanager.secretAccessor` | the password secret |
| Deployers | `roles/run.jobsExecutorWithOverrides`, to start runs with per-run settings | the job |
| Deployers | `roles/run.viewer`, to follow the execution | the job |
| Deployers | `roles/storage.objectUser`, to upload bundles and read the manifest | the bucket |

For Cloud SQL or AlloyDB PostgreSQL with IAM database authentication, use
`db_auth = "google-access-token"` and no password. The runner then logs in as
its service account. Add that account to the instance as an IAM user with the
roles it needs, as `examples/cloudsql-postgres-cdc` does.

The subnetwork needs Private Google Access, because the job reaches Cloud
Storage and Secret Manager through the VPC. It also needs DNS for the
database's hostname. On a Shared VPC, the Cloud Run service agent needs
`compute.networkUser` on the subnet.

### 3. Add the resource

```hcl
resource "sqlmigrate_migrations" "app" {
  source_dir = "${path.module}/migrations"
  bucket     = module.db_migrations.bucket
  prefix     = module.db_migrations.prefix
  job        = module.db_migrations.job_id

  placeholders = { app_role = "app_rw" }   # ${app_role} in SQL

  depends_on = [google_sql_user.app]        # things the SQL needs
}

# Things that need the SQL to have run.
resource "google_datastream_stream" "cdc" {
  # ...
  depends_on = [sqlmigrate_migrations.app]
}
```

`examples/cloudsql-postgres-cdc` shows the whole ordering for Cloud SQL
feeding Datastream: the login, then the grants and publication, then the
stream. It has passed `terraform validate`, and its migrations run against
PostgreSQL 15 in the test suite.

### 4. Install the provider

There is no registry release yet. The source address is
`jq6l43d1/sqlmigrate`. Build it (`make build`, or `make mirror` for
linux_amd64) and install it through a filesystem mirror. In a pipeline, a
step before `terraform init` can put the binary into the root module's
`terraform.d/plugins/` directory, which Terraform checks automatically:

```
terraform.d/plugins/registry.terraform.io/jq6l43d1/sqlmigrate/0.1.0/linux_amd64/terraform-provider-sqlmigrate_v0.1.0
```

Fetch it from somewhere like an Artifact Registry generic repository and
check its SHA-256. Commit the lock file entry, so `terraform init
-lockfile=readonly` works:

```sh
terraform providers lock -fs-mirror=terraform.d/plugins -platform=linux_amd64 jq6l43d1/sqlmigrate
```

## Reference

### `sqlmigrate_migrations`

| Argument | | |
|---|---|---|
| `source_dir` | required | Directory of Flyway migrations. Every `.sql` and `.sql.conf` file under it is shipped. |
| `bucket`, `prefix` | required | Where the job keeps bundles and the manifest. They must match the job's settings (the module's outputs). |
| `job` | required | `projects/P/locations/L/jobs/NAME`, or `NAME` with the provider's project and region. |
| `container` | optional | Container to override if the job has more than one. |
| `placeholders` | optional | Flyway placeholders for each run. Changing them does not re-run applied migrations. Don't put secrets here: they end up in state and in the run's settings. |
| `triggers` | optional | Any change runs the job even with nothing pending, for example after a manual repair. |
| `timeouts` | optional | `create` and `update`, default 30m. |

| Attribute | |
|---|---|
| `applied` | Source migrations that are applied, in run order: `"<version>: <description>"` or `"R: <description> (checksum N)"`. The plan shows pending migrations as additions. |
| `schema_version` | Highest applied version. |
| `last_run_id`, `last_execution` | The last run this resource started, and its Cloud Run execution. |

Import with `terraform import sqlmigrate_migrations.x <bucket>/<prefix>`. The
next apply runs the job once to align state; Flyway skips what is already
applied. Destroying the resource leaves the database and the bucket alone.

### `data "sqlmigrate_manifest"`

Reads `manifest.json` for a `bucket` and `prefix`. It returns `exists`,
`run_id`, `status`, `error_message`, `finished_at`, `flyway_version`,
`database_type`, `schema_version`, and `applied`, with one object per schema
history row.

### Provider

```hcl
provider "sqlmigrate" {
  project = "my-project"    # for short job names; else GOOGLE_PROJECT
  region  = "us-central1"   # for short job names; else GOOGLE_REGION
}
```

Credentials are checked in this order:
1. `access_token`, or `GOOGLE_OAUTH_ACCESS_TOKEN`
2. `credentials` (a path or JSON for a service account key or an
   external_account/WIF config), or `GOOGLE_CREDENTIALS`
3. Application Default Credentials (`GOOGLE_APPLICATION_CREDENTIALS`)

This is the same order the google provider uses, so in a pipeline it picks
up the same login, including workload identity federation, with no
settings. `storage_endpoint` and
`run_endpoint` override the API URLs, and `poll_interval` sets how often the
execution is checked (default 5s).

## Writing migrations

Migrations follow Flyway's default naming:

- `V<version>__<description>.sql` runs once. Underscores in the version become
  dots, so `V1_1__x.sql` is version 1.1. **Never edit one after it has run;**
  the plan rejects that. Add a new version instead.
- `R__<description>.sql` re-runs whenever its content changes. Use it for
  views, functions and grants written to be safe to re-run.
- Flyway callbacks such as `afterMigrate.sql` and per-script `.sql.conf`
  files are shipped too.

Things to know:
- **An existing database with no Flyway history:** set
  `FLYWAY_BASELINE_ON_MIGRATE=true` and `FLYWAY_BASELINE_VERSION=0` in the
  module's `flyway_env`. Without them the first run stops with
  `NON_EMPTY_SCHEMA_WITHOUT_SCHEMA_HISTORY_TABLE`.
- **Transactions:** Flyway runs each migration in its own transaction where
  the database allows it. PostgreSQL rolls a failed migration back cleanly.
  MySQL doesn't have transactional DDL, so a failed migration can be half
  applied. Fix the database by hand, then run a `repair`.
- **Other Flyway settings** (schemas, out-of-order and so on) go in the
  module's `flyway_env`. The connection always comes from the module's
  `db_*` variables.

## Running the job by hand

The runner's command comes from `SQLMIGRATE_COMMAND`: `migrate` (default),
`info`, `validate` or `repair`. Without `SQLMIGRATE_BUNDLE` it uses the last
bundle recorded in the manifest.

```sh
# Refresh the manifest from the database
gcloud run jobs execute app-dev-db-migrate --region us-central1 --wait \
  --update-env-vars SQLMIGRATE_COMMAND=info

# After fixing a failed MySQL migration by hand
gcloud run jobs execute app-dev-db-migrate --region us-central1 --wait \
  --update-env-vars SQLMIGRATE_COMMAND=repair
```

Runner environment, set by the module:
- `SQLMIGRATE_BUCKET`, `SQLMIGRATE_PREFIX`
- `SQLMIGRATE_DB_URL`, `SQLMIGRATE_DB_USER`
- `SQLMIGRATE_DB_PASSWORD` or `SQLMIGRATE_DB_PASSWORD_FILE`
- `SQLMIGRATE_DB_AUTH`: `password` or `google-access-token`
- `SQLMIGRATE_FLYWAY`: the Flyway path; the image sets it

Set per run by the provider:
- `SQLMIGRATE_RUN_ID`, `SQLMIGRATE_BUNDLE`, `SQLMIGRATE_COMMAND`
- `SQLMIGRATE_PLACEHOLDERS`: a JSON object

The runner logs one JSON object per line, so Cloud Logging can filter on
`severity`, `run_id` and the other fields.

## Using it in a CI pipeline

- **Plan approval:** the planned `applied` list depends only on the SQL files
  and the manifest. A pipeline that re-plans after approval, and checks the
  changes match what was reviewed, gets the same plan. This was checked by
  hashing the `terraform show -json` resource changes of two separate plans
  (identical), and a saved plan then applied cleanly.
- **Credentials:** the provider reads `GOOGLE_APPLICATION_CREDENTIALS`, as
  the google provider does. With short-lived federated tokens, the job's
  runtime counts toward the apply step's token lifetime, so keep migrations
  short.
- **Who runs it:** the plan needs only `storage.objects.get` on the bucket.
  The apply needs the job and bucket roles above.

## Security notes

- Terraform never holds database credentials. The password stays in Secret
  Manager, injected into the job, or there is no password at all with
  `google-access-token`.
- The runner drops any `FLYWAY_URL`, `FLYWAY_USER`, `FLYWAY_PASSWORD` or
  `FLYWAY_LOCATIONS` it inherits, and sets the connection from its own
  variables. It also verifies every bundle file against the index hash and
  the bundle id before running anything.
- Anyone who can run the job with overrides can change its environment for
  that run, including `SQLMIGRATE_DB_URL`. Grant
  `roles/run.jobsExecutorWithOverrides` only to principals that could already
  change the job, as `deployer_members` does.
- Runs skip Flyway's update check (`-skipCheckForUpdate`), so nothing calls
  out to Maven Central from inside the VPC. The open-source build's jars
  contain only a no-op telemetry manager.

## Status and limits

Tested here:
- **Unit tests:** naming, version ordering, plan logic and runner output
  parsing. The checksum code matches eight values Flyway 13.10.3 produced.
- **End-to-end:** real Terraform 1.11.3, the provider, the runner and the
  Flyway CLI against SQLite through an in-process fake of the GCS and Cloud
  Run APIs. This covers create, no-op, new and changed migrations, edit
  detection, a failing migration and recovery, placeholders, triggers,
  import and the data source.
- **PostgreSQL 15:** the example's migrations (placeholders, `ALTER ROLE
  … REPLICATION`, `CREATE PUBLICATION … FOR TABLES IN SCHEMA`) run through
  the runner, and a second run is a no-op.
- **Flyway** works from a read-only install with no writable home, as in the
  image.
- **Against real GCP:** the image built from this Dockerfile, `modules/runner-job`
  and the provider, run from a CI pipeline with workload identity federation
  credentials. The job reached Cloud SQL for PostgreSQL 18 through a Private
  Service Connect endpoint and logged in with IAM database authentication
  (`db_auth = "google-access-token"`). One apply ran two versioned migrations
  and a repeatable one, and the next plan showed `No changes`.
- **Checkov** on `modules/runner-job`: the only finding is CKV_GCP_62 (no
  access logging on the bucket).

Not yet tested:
- **MySQL and SQL Server.** Flyway handles them, but they haven't been tested
  here.

Limits:
- Flyway's open-source edition: no undo migrations, no script (`.sh`)
  migrations, no dry run.
- One run at a time per database: Flyway locks the history table, and the
  job runs a single task.

## Development

```sh
make test                               # unit tests
make testacc FLYWAY=/path/to/flyway     # end-to-end with Terraform + Flyway (SQLite)
SQLMIGRATE_TEST_POSTGRES_URL='jdbc:postgresql://127.0.0.1:5432/postgres?user=postgres' \
  make testacc FLYWAY=/path/to/flyway   # adds the PostgreSQL example test
make demo FLYWAY=/path/to/flyway        # fake GCP on 127.0.0.1:8099 for trying it by hand
```

Get Flyway from the
[GitHub release](https://github.com/flyway/flyway/releases/tag/flyway-13.10.3)
(`flyway-commandline-13.10.3-linux-x64.tar.gz`, which includes Java).

Layout:
- `main.go`: the provider
- `cmd/sqlmigrate-runner`: the job binary
- `internal/provider`: the resource and data source
- `internal/reconcile`: the plan-time comparison
- `internal/runner`: runs Flyway and writes records
- `internal/migrations`: file parsing and checksums
- `internal/fakegcp`: the test fake
- `modules/runner-job`: the job and its IAM
- `examples/cloudsql-postgres-cdc`: Cloud SQL → Datastream example
