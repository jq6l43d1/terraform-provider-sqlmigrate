package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/option"

	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/bundle"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/fakegcp"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/gcp"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/manifest"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/migrations"
)

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{
		EnvBucket: "b", EnvPrefix: "/app/dev/", EnvDBURL: "jdbc:postgresql://db:5432/app",
		EnvRunID: "run-12345678", EnvPlaceholders: `{"app_role":"app_rw"}`,
	}
	c, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Prefix != "app/dev" || c.Command != manifest.CommandMigrate || c.DBAuth != AuthPassword || c.Placeholders["app_role"] != "app_rw" {
		t.Fatalf("got %+v", c)
	}

	bad := map[string]string{EnvPrefix: "../x", EnvDBURL: "postgres://x", EnvCommand: "clean", EnvRunID: "x/y", EnvBundle: "nope", EnvPlaceholders: `{"a b":"1"}`}
	_, err = ConfigFromEnv(func(k string) string { return bad[k] })
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{EnvBucket, EnvPrefix, EnvDBURL, EnvCommand, EnvRunID, EnvBundle, "placeholder name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

func TestFlywayEnvReplacesConnection(t *testing.T) {
	env := flywayEnv([]string{"PATH=/bin", "FLYWAY_URL=jdbc:evil", "FLYWAY_SCHEMAS=app", EnvDBPassword + "=secret"}, "jdbc:good", "u", "p")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "evil") || strings.Contains(joined, "secret") {
		t.Fatalf("env kept overridden values:\n%s", joined)
	}
	for _, want := range []string{"PATH=/bin", "FLYWAY_SCHEMAS=app", "FLYWAY_URL=jdbc:good", "FLYWAY_USER=u", "FLYWAY_PASSWORD=p"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestBuildApplied(t *testing.T) {
	fw := &flyway{locationDir: "/tmp/w/bundle"}
	files := map[string][]byte{"V1__a.sql": []byte("SELECT 1;\n"), "R__v.sql": []byte("SELECT 2;\n")}
	info := &flywayOutput{Migrations: []flywayMigration{
		{Category: "Versioned", Version: "1", Description: "a", Type: "SQL", State: "Success", InstalledOnUTC: "t1", Filepath: "/tmp/w/bundle/V1__a.sql"},
		{Category: "Repeatable", Description: "v", Type: "SQL", State: "Superseded", InstalledOnUTC: "t2", Filepath: "/tmp/w/bundle/R__v.sql"},
		{Category: "Repeatable", Description: "v", Type: "SQL", State: "Success", InstalledOnUTC: "t3", Filepath: "/tmp/w/bundle/R__v.sql"},
		{Category: "Versioned", Version: "2", Description: "b", Type: "SQL", State: "Pending", Filepath: "/tmp/w/bundle/V2__b.sql"},
		{Category: "Versioned", Version: "0", Description: "old", Type: "SQL", State: "Missing", InstalledOnUTC: "t0"},
	}}
	old := int32(42)
	prev := &manifest.Record{Applied: []manifest.Applied{{Category: "Versioned", Version: "0", Description: "old", InstalledOn: "t0", Checksum: &old}}}

	got := buildApplied(info, fw, files, true, prev)
	if len(got) != 3 {
		t.Fatalf("got %d rows: %+v", len(got), got)
	}
	if got[0].Script != "V1__a.sql" || got[0].Checksum == nil || *got[0].Checksum != migrations.Checksum(files["V1__a.sql"]) {
		t.Errorf("row 0: %+v", got[0])
	}
	if got[1].InstalledOn != "t3" || got[1].Checksum == nil {
		t.Errorf("row 1 should be the current repeatable with a checksum: %+v", got[1])
	}
	if got[2].Checksum == nil || *got[2].Checksum != 42 {
		t.Errorf("row 2 should carry its checksum forward: %+v", got[2])
	}

	untrusted := buildApplied(info, fw, files, false, nil)
	for _, a := range untrusted {
		if a.Checksum != nil {
			t.Errorf("untrusted run recorded a checksum: %+v", a)
		}
	}
}

// The tests below run the real Flyway CLI against SQLite. Point
// SQLMIGRATE_TEST_FLYWAY at a Flyway executable to run them.

type harness struct {
	t      *testing.T
	srv    *fakegcp.Server
	store  *gcp.Storage
	srcDir string
	dbURL  string
	flyway string
	extra  map[string]string // added to the runner environment
	logs   bytes.Buffer
}

const testBucket, testPrefix = "migrations-bucket", "app/dev"

func newHarness(t *testing.T) *harness {
	t.Helper()
	fw := os.Getenv("SQLMIGRATE_TEST_FLYWAY")
	if fw == "" {
		t.Skip("set SQLMIGRATE_TEST_FLYWAY to a Flyway executable to run this test")
	}
	srv := fakegcp.New()
	t.Cleanup(srv.Close)
	store, err := gcp.NewStorage(context.Background(), option.WithEndpoint(srv.StorageEndpoint()), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return &harness{t: t, srv: srv, store: store, srcDir: filepath.Join(dir, "sql"), dbURL: "jdbc:sqlite:" + filepath.Join(dir, "db.sqlite"), flyway: fw}
}

func (h *harness) write(rel, content string) {
	h.t.Helper()
	p := filepath.Join(h.srcDir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// run uploads the current source as a bundle and runs the runner once.
func (h *harness) run(command, runID string, withBundle bool) (int, *manifest.Record) {
	h.t.Helper()
	ctx := context.Background()
	src, err := migrations.Load(h.srcDir)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := bundle.Upload(ctx, h.store, testBucket, testPrefix, src, time.Now()); err != nil {
		h.t.Fatal(err)
	}
	env := map[string]string{
		EnvBucket: testBucket, EnvPrefix: testPrefix, EnvDBURL: h.dbURL,
		EnvFlyway: h.flyway, EnvCommand: command, EnvRunID: runID,
	}
	for k, v := range h.extra {
		env[k] = v
	}
	if withBundle {
		env[EnvBundle] = src.BundleID
	}
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		h.t.Fatal(err)
	}
	h.logs.Reset()
	r := &Runner{Config: cfg, Store: h.store, Log: NewLogger(&h.logs), Stderr: &h.logs}
	r.Environ = func() []string {
		env := os.Environ()
		for k, v := range h.extra {
			if strings.HasPrefix(k, "FLYWAY_") {
				env = append(env, k+"="+v)
			}
		}
		return env
	}
	code := r.Run(ctx)
	data, found := h.srv.Object(testBucket, manifest.RunObject(testPrefix, runID))
	if !found {
		h.t.Fatalf("no run record written; logs:\n%s", h.logs.String())
	}
	rec, err := manifest.Decode(data)
	if err != nil {
		h.t.Fatal(err)
	}
	return code, rec
}

func (h *harness) manifest() *manifest.Record {
	h.t.Helper()
	data, found := h.srv.Object(testBucket, manifest.ManifestObject(testPrefix))
	if !found {
		h.t.Fatal("manifest.json not written")
	}
	rec, err := manifest.Decode(data)
	if err != nil {
		h.t.Fatal(err)
	}
	return rec
}

func checksumOf(rec *manifest.Record, version, desc string) *int32 {
	for _, a := range rec.Applied {
		if a.Version == version && a.Description == desc {
			return a.Checksum
		}
	}
	return nil
}

func TestRunnerWithFlyway(t *testing.T) {
	h := newHarness(t)
	h.write("V1__create_a.sql", "CREATE TABLE a (id INT);\n")
	h.write("sub/V1_1__create_b.sql", "CREATE TABLE b (id INT);\n")
	h.write("R__view_v.sql", "DROP VIEW IF EXISTS v;\nCREATE VIEW v AS SELECT * FROM a;\n")

	code, rec := h.run(manifest.CommandMigrate, "run-00000001", true)
	if code != 0 || rec.Status != manifest.StatusSucceeded {
		t.Fatalf("first run failed (%d): %+v\n%s", code, rec.Error, h.logs.String())
	}
	if len(rec.Executed) != 3 || !rec.HistoryRead || len(rec.Applied) != 3 || rec.SchemaVersion != "1.1" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	for _, a := range rec.Applied {
		if a.Checksum == nil {
			t.Errorf("no checksum for %+v", a)
		}
	}
	if rec.Applied[1].Script != "sub/V1_1__create_b.sql" {
		t.Errorf("script path = %q", rec.Applied[1].Script)
	}

	// New migration plus a changed repeatable.
	h.write("V2__create_c.sql", "CREATE TABLE c (id INT);\n")
	h.write("R__view_v.sql", "DROP VIEW IF EXISTS v;\nCREATE VIEW v AS SELECT id FROM a;\n")
	code, rec = h.run(manifest.CommandMigrate, "run-00000002", true)
	if code != 0 || len(rec.Executed) != 2 || rec.SchemaVersion != "2" {
		t.Fatalf("second run: code=%d %+v", code, rec)
	}
	repeatables := 0
	for _, a := range rec.Applied {
		if a.Category == manifest.CategoryRepeatable {
			repeatables++
		}
	}
	if repeatables != 1 {
		t.Fatalf("want one current repeatable row, got %d", repeatables)
	}
	v1 := checksumOf(rec, "1", "create a")

	// A failing script: the run fails, the record says why, and the history
	// is still recorded.
	h.write("V3__bad.sql", "CREATE TABLE d (id INT);\nTHIS IS NOT SQL;\n")
	code, rec = h.run(manifest.CommandMigrate, "run-00000003", true)
	if code == 0 || rec.Status != manifest.StatusFailed || rec.Error == nil {
		t.Fatalf("bad script should fail: %+v", rec)
	}
	if rec.Error.Code != "FAILED_VERSIONED_MIGRATION" || rec.Error.Script != "V3__bad.sql" || !strings.Contains(rec.Error.Message, "syntax error") {
		t.Fatalf("error = %+v", rec.Error)
	}
	if strings.Contains(rec.Error.Message, h.srcDir) || strings.Contains(rec.Error.Message, os.TempDir()+"/sqlmigrate-") {
		t.Errorf("error message leaks temp paths: %s", rec.Error.Message)
	}
	if !rec.HistoryRead || rec.SchemaVersion != "2" {
		t.Fatalf("history after failure: %+v", rec)
	}
	if h.manifest().RunID != "run-00000003" {
		t.Fatal("manifest.json should reflect the failed run's history")
	}

	// Editing an applied script fails validation; checksums carry forward
	// unchanged instead of being taken from the edited file.
	h.write("V3__bad.sql", "CREATE TABLE d (id INT);\n")
	h.write("V1__create_a.sql", "CREATE TABLE a (id INT, x INT);\n")
	code, rec = h.run(manifest.CommandMigrate, "run-00000004", true)
	if code == 0 || rec.Error == nil || rec.Error.Code != "VALIDATE_ERROR" {
		t.Fatalf("edited script should fail validation: %+v", rec.Error)
	}
	if got := checksumOf(rec, "1", "create a"); got == nil || *got != *v1 {
		t.Fatalf("checksum for V1 = %v, want carried %d", got, *v1)
	}

	// info without a bundle reuses the last bundle.
	h.write("V1__create_a.sql", "CREATE TABLE a (id INT);\n")
	code, rec = h.run(manifest.CommandInfo, "run-00000005", false)
	if code != 0 || !rec.HistoryRead || rec.BundleID == "" {
		t.Fatalf("info: code=%d %+v\n%s", code, rec, h.logs.String())
	}

	// Fix and finish.
	code, rec = h.run(manifest.CommandMigrate, "run-00000006", true)
	if code != 0 || rec.SchemaVersion != "3" {
		t.Fatalf("final run: code=%d %+v", code, rec.Error)
	}
}

func TestRunnerRejectsTamperedBundle(t *testing.T) {
	h := newHarness(t)
	h.write("V1__a.sql", "CREATE TABLE a (id INT);\n")
	src, err := migrations.Load(h.srcDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.Upload(context.Background(), h.store, testBucket, testPrefix, src, time.Now()); err != nil {
		t.Fatal(err)
	}
	h.srv.PutObject(testBucket, manifest.BundleFileObject(testPrefix, src.BundleID, "V1__a.sql"), []byte("DROP TABLE a;\n"))
	code, rec := h.run(manifest.CommandMigrate, "run-tamper01", true)
	if code == 0 || rec.Error == nil || !strings.Contains(rec.Error.Message, "does not match its recorded hash") {
		t.Fatalf("want hash error, got %+v", rec.Error)
	}
	if _, found := h.srv.Object(testBucket, manifest.ManifestObject(testPrefix)); found {
		t.Fatal("a run that never reached the database must not write manifest.json")
	}
}

// TestRunnerPostgresExample runs examples/cloudsql-postgres-cdc/migrations against a real
// PostgreSQL. Set SQLMIGRATE_TEST_POSTGRES_URL to a JDBC URL for a throwaway
// database whose user can create roles, e.g.
// jdbc:postgresql://127.0.0.1:5432/postgres?user=postgres
func TestRunnerPostgresExample(t *testing.T) {
	url := os.Getenv("SQLMIGRATE_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set SQLMIGRATE_TEST_POSTGRES_URL to run this test")
	}
	h := newHarness(t)
	h.dbURL = url
	// A fresh history schema and publication per run, so the test can run
	// again against the same database.
	suffix := randomHex(4)
	publication := "cdc_publication_" + suffix
	h.extra = map[string]string{
		EnvPlaceholders:  `{"cdc_user":"datastream_reader","publication":"` + publication + `"}`,
		"FLYWAY_SCHEMAS": "sqlm_test_" + suffix,
	}

	examples := filepath.Join("..", "..", "examples", "cloudsql-postgres-cdc", "migrations")
	entries, err := os.ReadDir(examples)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(examples, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		h.write(e.Name(), string(b))
	}
	// Terraform creates the Datastream login before the migrations run.
	h.write("V0_1__test_login.sql", `DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'datastream_reader') THEN
    CREATE ROLE datastream_reader LOGIN;
  END IF;
END $$;
`)
	// Fails the run unless the example SQL did what it promises.
	h.write("V9__check.sql", `DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_publication WHERE pubname = '${publication}') THEN
    RAISE EXCEPTION 'publication missing';
  END IF;
  IF NOT (SELECT rolreplication FROM pg_roles WHERE rolname = 'datastream_reader') THEN
    RAISE EXCEPTION 'REPLICATION not granted';
  END IF;
  IF (SELECT count(*) FROM pg_publication_namespace pn JOIN pg_publication p ON p.oid = pn.pnpubid
      WHERE p.pubname = '${publication}') <> 2 THEN
    RAISE EXCEPTION 'publication does not cover both schemas';
  END IF;
END $$;
`)

	code, rec := h.run(manifest.CommandMigrate, "run-pg000001", true)
	if code != 0 {
		t.Fatalf("run failed: %+v\n%s", rec.Error, h.logs.String())
	}
	if rec.DatabaseType != "PostgreSQL" || rec.SchemaVersion != "9" || len(rec.Executed) != 4 {
		t.Fatalf("unexpected record: type=%q version=%q executed=%d", rec.DatabaseType, rec.SchemaVersion, len(rec.Executed))
	}

	// Re-running with nothing new does nothing.
	code, rec = h.run(manifest.CommandMigrate, "run-pg000002", true)
	if code != 0 || len(rec.Executed) != 0 {
		t.Fatalf("second run: code=%d executed=%d", code, len(rec.Executed))
	}
}
