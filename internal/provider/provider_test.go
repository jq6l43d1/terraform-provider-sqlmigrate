package provider

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"google.golang.org/api/option"

	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/fakegcp"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/gcp"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/manifest"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/runner"
)

var factories = map[string]func() (tfprotov6.ProviderServer, error){
	"sqlmigrate": providerserver.NewProtocol6WithError(New("test")()),
}

func TestJobName(t *testing.T) {
	c := &client{project: "p", region: "r"}
	if got, err := c.jobName("migrate"); err != nil || got != "projects/p/locations/r/jobs/migrate" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := c.jobName("projects/p/jobs/x"); err == nil {
		t.Fatal("want error for malformed full name")
	}
	if _, err := (&client{}).jobName("migrate"); err == nil {
		t.Fatal("want error without project and region")
	}
}

// env is a test environment: a fake GCS + Cloud Run whose job runs the real
// runner (and so the real Flyway CLI) against a SQLite file.
type env struct {
	t      *testing.T
	srv    *fakegcp.Server
	sqlDir string
	mu     sync.Mutex
	logs   bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	fw := os.Getenv("SQLMIGRATE_TEST_FLYWAY")
	if fw == "" || os.Getenv("TF_ACC") == "" {
		t.Skip("set TF_ACC=1 and SQLMIGRATE_TEST_FLYWAY to a Flyway executable to run this test")
	}
	e := &env{t: t, srv: fakegcp.New()}
	t.Cleanup(e.srv.Close)
	dir := t.TempDir()
	e.sqlDir = filepath.Join(dir, "sql")
	if err := os.MkdirAll(e.sqlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := gcp.NewStorage(context.Background(), option.WithEndpoint(e.srv.StorageEndpoint()), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]string{
		runner.EnvBucket: "bkt", runner.EnvPrefix: "app/dev",
		runner.EnvDBURL: "jdbc:sqlite:" + filepath.Join(dir, "db.sqlite"), runner.EnvFlyway: fw,
	}
	e.srv.AddJob("projects/p/locations/us-central1/jobs/migrate", func(ctx context.Context, overrides map[string]string) error {
		merged := map[string]string{}
		for k, v := range base {
			merged[k] = v
		}
		for k, v := range overrides {
			merged[k] = v
		}
		cfg, err := runner.ConfigFromEnv(func(k string) string { return merged[k] })
		if err != nil {
			return err
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		r := &runner.Runner{Config: cfg, Store: store, Log: runner.NewLogger(&e.logs), Stderr: &e.logs}
		if code := r.Run(ctx); code != 0 {
			return fmt.Errorf("runner exited %d", code)
		}
		return nil
	})
	return e
}

func (e *env) write(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.sqlDir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) config(extra string) string {
	return fmt.Sprintf(`
provider "sqlmigrate" {
  project          = "p"
  region           = "us-central1"
  access_token     = "test-token"
  storage_endpoint = %q
  run_endpoint     = %q
  poll_interval    = "50ms"
}

resource "sqlmigrate_migrations" "db" {
  source_dir = %q
  bucket     = "bkt"
  prefix     = "app/dev"
  job        = "migrate"
%s
}

data "sqlmigrate_manifest" "db" {
  bucket = "bkt"
  prefix = "app/dev"

  depends_on = [sqlmigrate_migrations.db]
}
`, e.srv.StorageEndpoint(), e.srv.RunEndpoint(), e.sqlDir, extra)
}

func (e *env) runs(want int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := e.srv.RunCount(); got != want {
			return fmt.Errorf("job ran %d times, want %d\nrunner logs:\n%s", got, want, e.logs.String())
		}
		return nil
	}
}

const res = "sqlmigrate_migrations.db"

func strs(vals ...string) knownvalue.Check {
	checks := make([]knownvalue.Check, len(vals))
	for i, v := range vals {
		checks[i] = knownvalue.StringExact(v)
	}
	return knownvalue.ListExact(checks)
}

func TestAccMigrations(t *testing.T) {
	e := newEnv(t)
	e.write("V1__create_a.sql", "CREATE TABLE a (id INT);\n")
	e.write("V2__create_b.sql", "CREATE TABLE b (owner TEXT DEFAULT '${owner}');\n")
	e.write("R__view_v.sql", "DROP VIEW IF EXISTS v;\nCREATE VIEW v AS SELECT * FROM a;\n")
	placeholders := `  placeholders = { owner = "app_owner" }`
	var firstRun string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				// Create: everything is pending and the plan says so.
				Config: e.config(placeholders),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(res, plancheck.ResourceActionCreate),
					plancheck.ExpectKnownValue(res, tfjsonpath.New("applied"),
						strs("1: create a", "2: create b", "R: view v (checksum -365248715)")),
					plancheck.ExpectUnknownValue(res, tfjsonpath.New("last_run_id")),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(res, "applied.#", "3"),
					resource.TestCheckResourceAttr(res, "schema_version", "2"),
					resource.TestCheckResourceAttr(res, "id", "bkt/app/dev"),
					resource.TestMatchResourceAttr(res, "last_run_id", regexp.MustCompile(`^tf-`)),
					resource.TestCheckResourceAttrWith(res, "last_run_id", func(v string) error { firstRun = v; return nil }),
					resource.TestCheckResourceAttr("data.sqlmigrate_manifest.db", "exists", "true"),
					resource.TestCheckResourceAttr("data.sqlmigrate_manifest.db", "status", "succeeded"),
					resource.TestCheckResourceAttr("data.sqlmigrate_manifest.db", "applied.#", "3"),
					e.runs(1),
				),
			},
			{
				// Nothing pending: an empty plan and no run.
				Config:   e.config(placeholders),
				PlanOnly: true,
			},
			{
				// A new migration and a changed repeatable show as a diff of
				// `applied`, and only they run.
				PreConfig: func() {
					e.write("V3__create_c.sql", "CREATE TABLE c (id INT);\n")
					e.write("R__view_v.sql", "DROP VIEW IF EXISTS v;\nCREATE VIEW v AS SELECT id FROM a;\n")
				},
				Config: e.config(placeholders),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(res, plancheck.ResourceActionUpdate),
					plancheck.ExpectKnownValue(res, tfjsonpath.New("applied"),
						strs("1: create a", "2: create b", "3: create c", "R: view v (checksum 2071869614)")),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(res, "applied.#", "4"),
					resource.TestCheckResourceAttr(res, "schema_version", "3"),
					resource.TestCheckResourceAttrWith(res, "last_run_id", func(v string) error {
						if v == firstRun {
							return fmt.Errorf("last_run_id did not change")
						}
						return nil
					}),
					e.runs(2),
				),
			},
			{
				// Editing an applied migration is caught at plan time, with
				// no database access.
				PreConfig:   func() { e.write("V1__create_a.sql", "CREATE TABLE a (id INT, x INT);\n") },
				Config:      e.config(placeholders),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`V1__create_a.sql was changed after it was applied`),
			},
			{
				// A failing migration fails the apply with Flyway's message.
				PreConfig: func() {
					e.write("V1__create_a.sql", "CREATE TABLE a (id INT);\n")
					e.write("V4__create_d.sql", "CREATE TABLE d (id INT);\n")
					e.write("V5__bad.sql", "THIS IS NOT SQL;\n")
				},
				Config:      e.config(placeholders),
				ExpectError: regexp.MustCompile(`(?s)Flyway migrate failed in V5__bad.sql at line 1.*syntax error.*4: create d`),
			},
			{
				// After the failure, state holds what really got applied, so
				// only the fixed migration is pending.
				PreConfig: func() { e.write("V5__bad.sql", "CREATE TABLE e (id INT);\n") },
				Config:    e.config(placeholders),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(res, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(res, "applied.#", "6"),
					resource.TestCheckResourceAttr(res, "schema_version", "5"),
					e.runs(4),
				),
			},
			{
				// Changing only placeholders updates state without a run.
				Config: e.config(`  placeholders = { owner = "someone_else" }`),
				Check:  e.runs(4),
			},
			{
				// Changing triggers forces a run even with nothing pending.
				Config: e.config(`  triggers = { repair = "1" }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectUnknownValue(res, tfjsonpath.New("last_run_id")),
				}},
				Check: e.runs(5),
			},
			{
				ResourceName:      res,
				ImportState:       true,
				ImportStateId:     "bkt/app/dev",
				ImportStateVerify: false,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 || states[0].Attributes["bucket"] != "bkt" || states[0].Attributes["prefix"] != "app/dev" {
						return fmt.Errorf("unexpected import state: %+v", states)
					}
					return nil
				},
			},
		},
	})

	// The run records and bundles are in the bucket for later inspection.
	if names := e.srv.ObjectNames("bkt", "app/dev/runs/"); len(names) != 5 {
		t.Errorf("run records = %v", names)
	}
	if _, ok := e.srv.Object("bkt", manifest.ManifestObject("app/dev")); !ok {
		t.Error("manifest.json missing")
	}
}

func TestAccMissingJobReportsPermissionsHint(t *testing.T) {
	e := newEnv(t)
	e.write("V1__a.sql", "CREATE TABLE a (id INT);\n")
	cfg := e.config("") + "\n"
	cfg = regexp.MustCompile(`job        = "migrate"`).ReplaceAllString(cfg, `job        = "does-not-exist"`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config:      cfg,
			ExpectError: regexp.MustCompile(`(?s)Cannot start the migration job.*runWithOverrides`),
		}},
	})
}
