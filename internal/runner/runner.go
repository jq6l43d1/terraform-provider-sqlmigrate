// Package runner is the program that runs inside the Cloud Run job. It
// downloads a migration bundle from GCS, runs Flyway against the database,
// and writes what happened back to GCS for the Terraform provider to read.
package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/oauth2/google"

	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/manifest"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/migrations"
)

// ObjectStore is the subset of Cloud Storage the runner needs.
type ObjectStore interface {
	Get(ctx context.Context, bucket, object string) ([]byte, bool, error)
	Put(ctx context.Context, bucket, object string, data []byte, contentType string) error
}

// DB authentication modes.
const (
	AuthPassword    = "password"
	AuthAccessToken = "google-access-token"
)

// Config is read from the environment. The job definition sets where the
// bucket and database are; each run sets the run id, bundle, command and
// placeholders through Cloud Run overrides.
type Config struct {
	Bucket         string
	Prefix         string
	DBURL          string
	DBUser         string
	DBPassword     string
	DBPasswordFile string
	DBAuth         string
	FlywayPath     string
	Command        string
	RunID          string
	BundleID       string
	Placeholders   map[string]string
}

// Environment variable names.
const (
	EnvBucket          = "SQLMIGRATE_BUCKET"
	EnvPrefix          = "SQLMIGRATE_PREFIX"
	EnvDBURL           = "SQLMIGRATE_DB_URL"
	EnvDBUser          = "SQLMIGRATE_DB_USER"
	EnvDBPassword      = "SQLMIGRATE_DB_PASSWORD"
	EnvDBPasswordFile  = "SQLMIGRATE_DB_PASSWORD_FILE"
	EnvDBAuth          = "SQLMIGRATE_DB_AUTH"
	EnvFlyway          = "SQLMIGRATE_FLYWAY"
	EnvCommand         = "SQLMIGRATE_COMMAND"
	EnvRunID           = "SQLMIGRATE_RUN_ID"
	EnvBundle          = "SQLMIGRATE_BUNDLE"
	EnvPlaceholders    = "SQLMIGRATE_PLACEHOLDERS"
	EnvStorageEndpoint = "SQLMIGRATE_STORAGE_ENDPOINT"
	EnvInsecureNoAuth  = "SQLMIGRATE_INSECURE_NO_AUTH"
)

// ConfigFromEnv reads and checks the configuration.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := Config{
		Bucket:         getenv(EnvBucket),
		DBURL:          getenv(EnvDBURL),
		DBUser:         getenv(EnvDBUser),
		DBPassword:     getenv(EnvDBPassword),
		DBPasswordFile: getenv(EnvDBPasswordFile),
		DBAuth:         getenv(EnvDBAuth),
		FlywayPath:     getenv(EnvFlyway),
		Command:        getenv(EnvCommand),
		RunID:          getenv(EnvRunID),
		BundleID:       getenv(EnvBundle),
	}
	var problems []string
	if c.Bucket == "" {
		problems = append(problems, EnvBucket+" is not set")
	}
	prefix, err := manifest.CleanPrefix(getenv(EnvPrefix))
	if err != nil {
		problems = append(problems, EnvPrefix+": "+err.Error())
	}
	c.Prefix = prefix
	if !strings.HasPrefix(c.DBURL, "jdbc:") {
		problems = append(problems, EnvDBURL+" must be a JDBC URL (jdbc:...)")
	}
	if c.DBAuth == "" {
		c.DBAuth = AuthPassword
	}
	if c.DBAuth != AuthPassword && c.DBAuth != AuthAccessToken {
		problems = append(problems, fmt.Sprintf("%s must be %q or %q", EnvDBAuth, AuthPassword, AuthAccessToken))
	}
	if c.FlywayPath == "" {
		c.FlywayPath = "flyway"
	}
	if c.Command == "" {
		c.Command = manifest.CommandMigrate
	}
	switch c.Command {
	case manifest.CommandMigrate, manifest.CommandInfo, manifest.CommandValidate, manifest.CommandRepair:
	default:
		problems = append(problems, fmt.Sprintf("%s must be migrate, info, validate or repair, not %q", EnvCommand, c.Command))
	}
	if c.RunID == "" {
		c.RunID = "manual-" + time.Now().UTC().Format("20060102t150405") + "-" + randomHex(4)
	}
	if !manifest.ValidRunID(c.RunID) {
		problems = append(problems, EnvRunID+" must be 8-64 letters, digits or dashes")
	}
	if c.BundleID != "" && !manifest.ValidBundleID(c.BundleID) {
		problems = append(problems, EnvBundle+" must be a 64-character hex bundle id")
	}
	if raw := getenv(EnvPlaceholders); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.Placeholders); err != nil {
			problems = append(problems, EnvPlaceholders+" must be a JSON object of strings: "+err.Error())
		}
		for k := range c.Placeholders {
			if !validPlaceholder(k) {
				problems = append(problems, fmt.Sprintf("placeholder name %q may only use letters, digits, '_', '.' and '-'", k))
			}
		}
	}
	if len(problems) > 0 {
		return c, fmt.Errorf("invalid runner configuration:\n  %s", strings.Join(problems, "\n  "))
	}
	return c, nil
}

func validPlaceholder(k string) bool {
	if k == "" {
		return false
	}
	for _, r := range k {
		if !(r == '_' || r == '.' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// Runner executes one run.
type Runner struct {
	Config Config
	Store  ObjectStore
	Log    *Logger
	// Stderr receives Flyway's own log output.
	Stderr io.Writer
	// Environ returns the environment passed to Flyway (os.Environ by
	// default); connection settings are always replaced by the runner's.
	Environ func() []string
	// Token returns an access token for AuthAccessToken (Google ADC by
	// default).
	Token func(ctx context.Context) (string, error)
	Now   func() time.Time
}

// Run performs the run and returns the process exit code.
func (r *Runner) Run(ctx context.Context) int {
	c := r.Config
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Stderr == nil {
		r.Stderr = os.Stderr
	}
	if r.Environ == nil {
		r.Environ = os.Environ
	}
	rec := &manifest.Record{
		Schema:    manifest.Schema,
		RunID:     c.RunID,
		Command:   c.Command,
		StartedAt: r.Now().UTC(),
		Executed:  []manifest.Executed{},
		Applied:   []manifest.Applied{},
	}
	r.Log.Info("starting", "run_id", c.RunID, "command", c.Command, "bundle", c.BundleID)

	prev, err := r.readManifest(ctx)
	if err != nil {
		r.Log.Warn("could not read the previous manifest; checksums will not be carried forward", "error", err.Error())
	}

	work, err := os.MkdirTemp("", "sqlmigrate-")
	if err != nil {
		return r.finish(ctx, rec, &manifest.RunError{Code: "RUNNER", Message: err.Error()})
	}
	defer os.RemoveAll(work)
	bundleDir := filepath.Join(work, "bundle")
	if err := os.MkdirAll(bundleDir, 0o700); err != nil {
		return r.finish(ctx, rec, &manifest.RunError{Code: "RUNNER", Message: err.Error()})
	}

	if c.BundleID == "" && prev != nil {
		c.BundleID = prev.BundleID
	}
	rec.BundleID = c.BundleID
	files := map[string][]byte{}
	if c.BundleID != "" {
		files, err = r.fetchBundle(ctx, c.BundleID, bundleDir)
		if err != nil {
			return r.finish(ctx, rec, &manifest.RunError{Code: "RUNNER_BUNDLE", Message: err.Error()})
		}
		r.Log.Info("bundle downloaded", "bundle", c.BundleID, "files", fmt.Sprint(len(files)))
	} else if c.Command != manifest.CommandInfo {
		return r.finish(ctx, rec, &manifest.RunError{Code: "RUNNER_BUNDLE",
			Message: EnvBundle + " is not set and no earlier run recorded a bundle"})
	}

	password, err := r.password(ctx)
	if err != nil {
		return r.finish(ctx, rec, &manifest.RunError{Code: "RUNNER_CREDENTIALS", Message: err.Error()})
	}
	fw := &flyway{
		path:         c.FlywayPath,
		locationDir:  bundleDir,
		placeholders: c.Placeholders,
		env:          flywayEnv(r.Environ(), c.DBURL, c.DBUser, password),
		stderr:       r.Stderr,
	}

	var runErr *manifest.RunError
	trusted := false
	if c.Command != manifest.CommandInfo {
		res := fw.run(ctx, c.Command)
		runErr = res.err
		if res.out != nil {
			rec.FlywayVersion = res.out.FlywayVersion
			rec.DatabaseType = res.out.DatabaseType
			rec.Warnings = append(rec.Warnings, res.out.warnings()...)
			if c.Command == manifest.CommandMigrate {
				for _, m := range res.out.Migrations {
					rec.Executed = append(rec.Executed, manifest.Executed{
						Category: m.Category, Version: m.Version, Description: m.Description, ExecutionTimeMS: m.ExecutionTime})
				}
			}
		}
		trusted = checksumsTrusted(c.Command, res)
		if runErr != nil {
			r.Log.Error(c.Command+" failed", "code", runErr.Code, "message", runErr.Message)
		} else {
			r.Log.Info(c.Command+" succeeded", "executed", fmt.Sprint(len(rec.Executed)))
		}
	}

	info := fw.run(ctx, manifest.CommandInfo)
	if info.ok {
		rec.HistoryRead = true
		if info.out.FlywayVersion != "" {
			rec.FlywayVersion = info.out.FlywayVersion
		}
		if info.out.SchemaVersion != nil {
			rec.SchemaVersion = *info.out.SchemaVersion
		}
		rec.Applied = buildApplied(info.out, fw, files, trusted, prev)
	} else {
		msg := "could not read the schema history"
		if info.err != nil {
			msg += ": " + info.err.Message
		}
		rec.Warnings = append(rec.Warnings, msg)
		r.Log.Warn(msg)
		if c.Command == manifest.CommandInfo {
			runErr = info.err
		}
	}
	return r.finish(ctx, rec, runErr)
}

// finish writes the run record (and manifest.json when the history was read)
// and returns the exit code.
func (r *Runner) finish(ctx context.Context, rec *manifest.Record, runErr *manifest.RunError) int {
	rec.FinishedAt = r.Now().UTC()
	rec.Status = manifest.StatusSucceeded
	if runErr != nil {
		rec.Status = manifest.StatusFailed
		rec.Error = runErr
	}
	data, err := manifest.Encode(rec)
	if err != nil {
		r.Log.Error("encoding run record", "error", err.Error())
		return 1
	}
	c := r.Config
	if err := r.Store.Put(ctx, c.Bucket, manifest.RunObject(c.Prefix, rec.RunID), data, "application/json"); err != nil {
		r.Log.Error("writing run record", "error", err.Error())
		return 1
	}
	if rec.HistoryRead {
		if err := r.Store.Put(ctx, c.Bucket, manifest.ManifestObject(c.Prefix), data, "application/json"); err != nil {
			r.Log.Error("writing manifest", "error", err.Error())
			return 1
		}
	}
	r.Log.Info("finished", "run_id", rec.RunID, "status", rec.Status, "schema_version", rec.SchemaVersion)
	if runErr != nil {
		return 1
	}
	return 0
}

func (r *Runner) readManifest(ctx context.Context) (*manifest.Record, error) {
	data, found, err := r.Store.Get(ctx, r.Config.Bucket, manifest.ManifestObject(r.Config.Prefix))
	if err != nil || !found {
		return nil, err
	}
	return manifest.Decode(data)
}

// fetchBundle downloads a bundle into dir and checks every file against the
// index and the bundle id.
func (r *Runner) fetchBundle(ctx context.Context, id, dir string) (map[string][]byte, error) {
	c := r.Config
	raw, found, err := r.Store.Get(ctx, c.Bucket, manifest.BundleIndexObject(c.Prefix, id))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("bundle %s not found in gs://%s/%s", id, c.Bucket, manifest.BundleIndexObject(c.Prefix, id))
	}
	var idx manifest.BundleIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("parsing bundle index: %w", err)
	}
	if idx.BundleID != id {
		return nil, fmt.Errorf("bundle index names bundle %s, expected %s", idx.BundleID, id)
	}
	files := map[string][]byte{}
	var listed []migrations.File
	for _, f := range idx.Files {
		rel, err := manifest.CleanRelPath(f.Path)
		if err != nil {
			return nil, err
		}
		data, found, err := r.Store.Get(ctx, c.Bucket, manifest.BundleFileObject(c.Prefix, id, rel))
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("bundle file %s is missing", rel)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			return nil, fmt.Errorf("bundle file %s does not match its recorded hash", rel)
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return nil, err
		}
		files[rel] = data
		listed = append(listed, migrations.File{Path: rel, SHA256: f.SHA256})
	}
	sort.Slice(listed, func(i, j int) bool { return listed[i].Path < listed[j].Path })
	if got := migrations.BundleID(listed); got != id {
		return nil, fmt.Errorf("bundle contents hash to %s, expected %s", got, id)
	}
	return files, nil
}

func (r *Runner) password(ctx context.Context) (string, error) {
	c := r.Config
	if c.DBAuth == AuthAccessToken {
		if r.Token != nil {
			return r.Token(ctx)
		}
		ts, err := google.DefaultTokenSource(ctx,
			"https://www.googleapis.com/auth/sqlservice.login",
			"https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			return "", fmt.Errorf("getting Google credentials for database login: %w", err)
		}
		tok, err := ts.Token()
		if err != nil {
			return "", fmt.Errorf("getting an access token for database login: %w", err)
		}
		return tok.AccessToken, nil
	}
	if c.DBPassword != "" {
		return c.DBPassword, nil
	}
	if c.DBPasswordFile != "" {
		b, err := os.ReadFile(c.DBPasswordFile)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", EnvDBPasswordFile, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return "", nil
}

// flywayEnv passes the job's environment through so FLYWAY_* settings on the
// job (schemas, out-of-order and so on) apply, but always sets the connection
// from the runner's own variables and drops the raw password variable.
func flywayEnv(base []string, url, user, password string) []string {
	drop := map[string]bool{
		"FLYWAY_URL": true, "FLYWAY_USER": true, "FLYWAY_PASSWORD": true, "FLYWAY_LOCATIONS": true,
		EnvDBPassword: true,
	}
	var env []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			env = append(env, kv)
		}
	}
	env = append(env, "FLYWAY_URL="+url)
	if user != "" {
		env = append(env, "FLYWAY_USER="+user)
	}
	if password != "" {
		env = append(env, "FLYWAY_PASSWORD="+password)
	}
	return env
}

// checksumsTrusted reports whether every applied migration that is also in
// the bundle is known to match its file: Flyway validated them during this
// command. A migrate that failed while executing a script got past
// validation; one that failed validation did not.
func checksumsTrusted(command string, res result) bool {
	if res.out == nil {
		return false
	}
	switch command {
	case manifest.CommandMigrate:
		if res.ok {
			return true
		}
		return res.err != nil && (res.err.Code == "FAILED_VERSIONED_MIGRATION" || res.err.Code == "FAILED_REPEATABLE_MIGRATION")
	case manifest.CommandValidate, manifest.CommandRepair:
		return res.ok
	}
	return false
}

// buildApplied turns `flyway info` output into the manifest's applied list.
// Superseded repeatable rows and deleted rows are dropped, so each
// repeatable appears once with its latest state.
func buildApplied(info *flywayOutput, fw *flyway, files map[string][]byte, trusted bool, prev *manifest.Record) []manifest.Applied {
	out := []manifest.Applied{}
	for _, m := range info.Migrations {
		if m.InstalledOnUTC == "" {
			continue
		}
		switch m.State {
		case "Superseded", "Deleted", "Undone":
			continue
		}
		a := manifest.Applied{
			Category:    m.Category,
			Version:     m.Version,
			Description: m.Description,
			Type:        m.Type,
			State:       m.State,
			InstalledOn: m.InstalledOnUTC,
			Script:      fw.rel(m.Filepath),
		}
		if trusted && a.Script != "" && (m.State == "Success" || m.State == "Out of Order") {
			if content, ok := files[a.Script]; ok {
				sum := migrations.Checksum(content)
				a.Checksum = &sum
			}
		}
		if a.Checksum == nil && prev != nil {
			for _, p := range prev.Applied {
				if p.Checksum != nil && p.Category == a.Category && p.Version == a.Version &&
					p.Description == a.Description && p.InstalledOn == a.InstalledOn {
					a.Checksum = p.Checksum
					break
				}
			}
		}
		out = append(out, a)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
