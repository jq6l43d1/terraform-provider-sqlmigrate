// Package manifest defines the files the runner and the provider exchange in
// the GCS bucket, and where they live:
//
//	<prefix>/manifest.json                       latest Record (what plan reads)
//	<prefix>/runs/<run id>.json                  Record for each run
//	<prefix>/bundles/<bundle id>/bundle.json     BundleIndex, written last
//	<prefix>/bundles/<bundle id>/files/<path>    the SQL files
//
// The database's own schema history table stays the source of truth. The
// manifest is a copy of it that Terraform can read without a network path to
// the database.
package manifest

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// Schema is the version of the JSON layout.
const Schema = 1

// Run statuses.
const (
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// Runner commands.
const (
	CommandMigrate  = "migrate"
	CommandInfo     = "info"
	CommandValidate = "validate"
	CommandRepair   = "repair"
)

// Record describes one runner execution. manifest.json is the Record of the
// most recent run that managed to read the schema history.
type Record struct {
	Schema        int        `json:"schema"`
	RunID         string     `json:"run_id"`
	Command       string     `json:"command"`
	Status        string     `json:"status"`
	Error         *RunError  `json:"error,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    time.Time  `json:"finished_at"`
	BundleID      string     `json:"bundle_id,omitempty"`
	FlywayVersion string     `json:"flyway_version,omitempty"`
	DatabaseType  string     `json:"database_type,omitempty"`
	SchemaVersion string     `json:"schema_version,omitempty"`
	Executed      []Executed `json:"executed"`
	// HistoryRead is false when the runner could not read the schema history
	// (for example, it could not connect). Applied is then empty and must not
	// be trusted.
	HistoryRead bool      `json:"history_read"`
	Applied     []Applied `json:"applied"`
	Warnings    []string  `json:"warnings,omitempty"`
}

// RunError is the failure reported by Flyway or the runner.
type RunError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	Script  string `json:"script,omitempty"`
	Line    int    `json:"line,omitempty"`
}

// Executed is a migration that ran during this execution.
type Executed struct {
	Category        string `json:"category"`
	Version         string `json:"version,omitempty"`
	Description     string `json:"description"`
	ExecutionTimeMS int64  `json:"execution_time_ms"`
}

// Applied is one current row of the schema history.
type Applied struct {
	Category    string `json:"category"` // Flyway's "Versioned" or "Repeatable"
	Version     string `json:"version,omitempty"`
	Description string `json:"description"`
	Type        string `json:"type"`  // "SQL", "BASELINE", ...
	State       string `json:"state"` // Flyway's state, e.g. "Success", "Failed", "Missing"
	InstalledOn string `json:"installed_on,omitempty"`
	Script      string `json:"script,omitempty"`
	// Checksum is the Flyway checksum of the script, recorded only when
	// Flyway has confirmed the bundle file matches the database (a migrate,
	// validate or repair that got past validation). Nil means unknown.
	Checksum *int32 `json:"checksum,omitempty"`
}

// Flyway categories as reported in JSON output.
const (
	CategoryVersioned  = "Versioned"
	CategoryRepeatable = "Repeatable"
)

// IsBaseline reports whether a is a baseline marker row.
func (a Applied) IsBaseline() bool {
	return strings.EqualFold(a.Type, "BASELINE") || strings.EqualFold(a.State, "Baseline")
}

// IsFailed reports whether a is a failed row that needs a repair.
func (a Applied) IsFailed() bool { return strings.HasPrefix(a.State, "Failed") }

// BundleIndex lists a bundle's files. The provider writes it after every file
// so its presence means the bundle is complete.
type BundleIndex struct {
	Schema    int          `json:"schema"`
	BundleID  string       `json:"bundle_id"`
	CreatedAt time.Time    `json:"created_at"`
	Files     []BundleFile `json:"files"`
}

// BundleFile is one entry in a BundleIndex.
type BundleFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

var (
	runIDRe    = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)
	bundleIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidRunID reports whether id is safe to use in an object name.
func ValidRunID(id string) bool { return runIDRe.MatchString(id) }

// ValidBundleID reports whether id is a bundle hash.
func ValidBundleID(id string) bool { return bundleIDRe.MatchString(id) }

// CleanPrefix trims slashes from a prefix and rejects ones that could escape
// it.
func CleanPrefix(prefix string) (string, error) {
	p := strings.Trim(prefix, "/")
	if p == "" {
		return "", fmt.Errorf("prefix must not be empty")
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("prefix %q has an empty, '.' or '..' segment", prefix)
		}
	}
	return p, nil
}

// CleanRelPath checks a bundle file path from an index before it is used on
// the local filesystem.
func CleanRelPath(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("invalid bundle path %q", p)
	}
	c := path.Clean(p)
	if c != p || c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("invalid bundle path %q", p)
	}
	return c, nil
}

// ManifestObject is the object holding the latest Record.
func ManifestObject(prefix string) string { return prefix + "/manifest.json" }

// RunObject is the object holding the Record of one run.
func RunObject(prefix, runID string) string { return prefix + "/runs/" + runID + ".json" }

// BundleIndexObject is the object holding a bundle's index.
func BundleIndexObject(prefix, bundleID string) string {
	return prefix + "/bundles/" + bundleID + "/bundle.json"
}

// BundleFileObject is the object holding one file of a bundle.
func BundleFileObject(prefix, bundleID, rel string) string {
	return prefix + "/bundles/" + bundleID + "/files/" + rel
}

// Decode parses a Record and checks its schema version.
func Decode(b []byte) (*Record, error) {
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	if r.Schema != Schema {
		return nil, fmt.Errorf("manifest schema %d is not supported (this build reads schema %d)", r.Schema, Schema)
	}
	return &r, nil
}

// Encode serialises a Record.
func Encode(r *Record) ([]byte, error) { return json.MarshalIndent(r, "", "  ") }
