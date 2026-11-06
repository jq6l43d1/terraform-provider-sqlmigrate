// Package reconcile compares the migrations in a source directory with the
// schema history recorded in a manifest. It needs no database connection,
// which is what lets `terraform plan` show pending migrations.
package reconcile

import (
	"fmt"
	"sort"

	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/manifest"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/migrations"
)

// Result is the outcome of a comparison.
type Result struct {
	// Applied lists, in source order, the source migrations the manifest
	// shows as applied. A repeatable that changed since it last ran is listed
	// with the checksum the database has, so the plan shows it being replaced.
	Applied []string
	// Expected lists every source migration as it will look once the runner
	// has applied the source.
	Expected []string
	// Pending are the migrations the next run will execute.
	Pending []migrations.Migration
	// Errors are problems Flyway would refuse to migrate past.
	Errors []string
	// Warnings are problems Flyway may refuse, depending on job settings.
	Warnings []string
}

// Label is how a source migration appears in the resource's applied list.
func Label(m migrations.Migration) string {
	if m.Kind == migrations.Versioned {
		return m.Version + ": " + m.Description
	}
	return fmt.Sprintf("R: %s (checksum %d)", m.Description, m.Checksum)
}

func repeatableLabel(desc string, checksum *int32) string {
	if checksum == nil {
		return fmt.Sprintf("R: %s (checksum unknown)", desc)
	}
	return fmt.Sprintf("R: %s (checksum %d)", desc, *checksum)
}

// Evaluate compares src with rec. A nil rec, or one whose history could not
// be read, means nothing is known to be applied.
func Evaluate(src []migrations.Migration, rec *manifest.Record) Result {
	res := Result{Applied: []string{}, Expected: []string{}}
	for _, m := range src {
		res.Expected = append(res.Expected, Label(m))
	}
	if rec == nil || !rec.HistoryRead {
		res.Pending = append(res.Pending, src...)
		return res
	}

	versioned := map[string]manifest.Applied{}
	repeatable := map[string]manifest.Applied{}
	var baseline, maxApplied string
	for _, a := range rec.Applied {
		switch {
		case a.IsBaseline():
			if migrations.ValidVersion(a.Version) && (baseline == "" || migrations.CompareVersions(a.Version, baseline) > 0) {
				baseline = a.Version
			}
		case a.Category == manifest.CategoryVersioned && migrations.ValidVersion(a.Version):
			versioned[migrations.VersionKey(a.Version)] = a
			if !a.IsFailed() && (maxApplied == "" || migrations.CompareVersions(a.Version, maxApplied) > 0) {
				maxApplied = a.Version
			}
		case a.Category == manifest.CategoryRepeatable:
			// The runner keeps one current row per description; if there are
			// several, the last one wins, matching Flyway's ordering.
			repeatable[a.Description] = a
		}
	}

	matchedV := map[string]bool{}
	matchedR := map[string]bool{}
	var maxSource string
	for _, m := range src {
		switch m.Kind {
		case migrations.Versioned:
			if maxSource == "" || migrations.CompareVersions(m.Version, maxSource) > 0 {
				maxSource = m.Version
			}
			key := migrations.VersionKey(m.Version)
			a, ok := versioned[key]
			if ok {
				matchedV[key] = true
				if a.IsFailed() {
					res.Errors = append(res.Errors, fmt.Sprintf(
						"version %s (%s) failed in an earlier run and Flyway will not continue until it is repaired. "+
							"Fix the database, then run the job once with SQLMIGRATE_COMMAND=repair.", m.Version, m.Path))
					res.Pending = append(res.Pending, m)
					continue
				}
				if a.Description != m.Description {
					res.Errors = append(res.Errors, fmt.Sprintf(
						"version %s is applied as %q but %s describes it as %q; Flyway rejects renamed migrations. "+
							"Restore the original file name.", m.Version, a.Description, m.Path, m.Description))
				}
				if a.Checksum != nil && *a.Checksum != m.Checksum {
					res.Errors = append(res.Errors, fmt.Sprintf(
						"%s was changed after it was applied (checksum in the database %d, local %d). "+
							"Applied versioned migrations must not change; revert the edit and add a new migration instead.",
						m.Path, *a.Checksum, m.Checksum))
				}
				res.Applied = append(res.Applied, Label(m))
				continue
			}
			if baseline != "" && migrations.CompareVersions(m.Version, baseline) <= 0 {
				// Flyway skips migrations at or below the baseline.
				res.Applied = append(res.Applied, Label(m))
				continue
			}
			res.Pending = append(res.Pending, m)
			if maxApplied != "" && migrations.CompareVersions(m.Version, maxApplied) < 0 {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"%s (version %s) is older than the newest applied version %s. Flyway refuses it unless the job sets FLYWAY_OUT_OF_ORDER=true.",
					m.Path, m.Version, maxApplied))
			}

		case migrations.Repeatable:
			a, ok := repeatable[m.Description]
			if ok {
				matchedR[m.Description] = true
				if a.Checksum != nil && *a.Checksum == m.Checksum && !a.IsFailed() {
					res.Applied = append(res.Applied, Label(m))
					continue
				}
				res.Applied = append(res.Applied, repeatableLabel(a.Description, a.Checksum))
			}
			res.Pending = append(res.Pending, m)
		}
	}

	for key, a := range versioned {
		if matchedV[key] {
			continue
		}
		if maxSource != "" && migrations.CompareVersions(a.Version, maxSource) < 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"version %s (%s) is applied but missing from source_dir. Flyway fails validation unless the job sets FLYWAY_IGNORE_MIGRATION_PATTERNS to include *:missing.",
				a.Version, a.Description))
		} else {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"version %s (%s) is applied but newer than anything in source_dir.", a.Version, a.Description))
		}
	}
	for desc := range repeatable {
		if !matchedR[desc] {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"repeatable %q is applied but missing from source_dir. Flyway fails validation unless the job sets FLYWAY_IGNORE_MIGRATION_PATTERNS to include *:missing.", desc))
		}
	}
	sort.Strings(res.Warnings)
	return res
}
