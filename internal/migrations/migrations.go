// Package migrations reads a directory of Flyway-style SQL migrations.
//
// Naming follows Flyway's defaults:
//
//	V<version>__<description>.sql   versioned, runs once
//	R__<description>.sql            repeatable, re-runs when its checksum changes
//
// Underscores in a version become dots ("V1_1__x.sql" is version 1.1) and
// underscores in a description become spaces, matching what Flyway records in
// its schema history table.
package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Kind is the type of a migration.
type Kind string

const (
	Versioned  Kind = "versioned"
	Repeatable Kind = "repeatable"
)

// Limits that stop a mistyped source_dir (a repo root, say) from being
// uploaded wholesale.
const (
	maxFileBytes   = 10 << 20
	maxBundleBytes = 50 << 20
	maxFiles       = 5000
)

// Migration is one versioned or repeatable SQL file.
type Migration struct {
	Kind        Kind
	Version     string // "1.1"; empty for repeatables
	Description string // "create roles"
	Path        string // slash-separated, relative to the source directory
	Checksum    int32  // Flyway-compatible CRC32
}

// File is one file that is shipped to the runner.
type File struct {
	Path    string // slash-separated, relative to the source directory
	SHA256  string
	Size    int64
	Content []byte
}

// Source is everything read from a source directory.
type Source struct {
	Migrations []Migration // versioned by version, then repeatables by description
	Files      []File      // every file shipped to the runner, sorted by path
	BundleID   string      // content hash of Files
	Warnings   []string
}

// Load reads dir. Every *.sql and *.sql.conf file is shipped so callbacks and
// per-script config travel with the migrations; only V and R files count as
// migrations.
func Load(dir string) (*Source, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("reading source_dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("source_dir %q is not a directory", dir)
	}

	src := &Source{}
	var total int64
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := d.Name()
		if p != dir && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(name, ".sql") && !strings.HasSuffix(name, ".sql.conf") {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		if fi.Size() > maxFileBytes {
			return fmt.Errorf("%s is %d bytes; the limit is %d", rel, fi.Size(), maxFileBytes)
		}
		total += fi.Size()
		if total > maxBundleBytes {
			return fmt.Errorf("source_dir holds more than %d bytes of SQL; check that it points at the migrations folder", maxBundleBytes)
		}
		if len(src.Files) >= maxFiles {
			return fmt.Errorf("source_dir holds more than %d SQL files; check that it points at the migrations folder", maxFiles)
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		src.Files = append(src.Files, File{Path: rel, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content)), Content: content})

		if !strings.HasSuffix(name, ".sql") {
			return nil
		}
		m, ok, warn := ParseName(name)
		if warn != "" {
			src.Warnings = append(src.Warnings, rel+": "+warn)
		}
		if !ok {
			return nil
		}
		m.Path = rel
		m.Checksum = Checksum(content)
		src.Migrations = append(src.Migrations, m)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(src.Files, func(i, j int) bool { return src.Files[i].Path < src.Files[j].Path })
	if err := sortAndCheck(src.Migrations); err != nil {
		return nil, err
	}
	src.BundleID = BundleID(src.Files)
	return src, nil
}

// sortAndCheck orders migrations the way Flyway runs them and rejects
// duplicates, which Flyway would also reject.
func sortAndCheck(ms []Migration) error {
	sort.SliceStable(ms, func(i, j int) bool {
		a, b := ms[i], ms[j]
		if a.Kind != b.Kind {
			return a.Kind == Versioned
		}
		if a.Kind == Versioned {
			if c := CompareVersions(a.Version, b.Version); c != 0 {
				return c < 0
			}
			return a.Path < b.Path
		}
		if a.Description != b.Description {
			return a.Description < b.Description
		}
		return a.Path < b.Path
	})
	var errs []error
	for i := 1; i < len(ms); i++ {
		a, b := ms[i-1], ms[i]
		if a.Kind != b.Kind {
			continue
		}
		if a.Kind == Versioned && CompareVersions(a.Version, b.Version) == 0 {
			errs = append(errs, fmt.Errorf("version %s is used by both %s and %s", b.Version, a.Path, b.Path))
		}
		if a.Kind == Repeatable && a.Description == b.Description {
			errs = append(errs, fmt.Errorf("repeatable description %q is used by both %s and %s", b.Description, a.Path, b.Path))
		}
	}
	return errors.Join(errs...)
}

// ParseName parses a file name such as "V1_2__create_roles.sql". ok is false
// for files Flyway does not treat as migrations; warn explains why when the
// file looks like a mistake rather than a callback.
func ParseName(name string) (m Migration, ok bool, warn string) {
	base := strings.TrimSuffix(path.Base(name), ".sql")
	switch {
	case strings.HasPrefix(base, "R__"):
		return Migration{Kind: Repeatable, Description: describe(strings.TrimPrefix(base, "R__"))}, true, ""
	case strings.HasPrefix(base, "V"):
		raw, desc, found := strings.Cut(strings.TrimPrefix(base, "V"), "__")
		if !found {
			return Migration{}, false, `ignored by Flyway: a versioned migration needs "__" between the version and the description`
		}
		version := strings.ReplaceAll(raw, "_", ".")
		if err := checkVersion(version); err != nil {
			return Migration{}, false, "ignored: " + err.Error()
		}
		return Migration{Kind: Versioned, Version: version, Description: describe(desc)}, true, ""
	}
	event, _, _ := strings.Cut(base, "__")
	if callbackEvents[event] {
		return Migration{}, false, ""
	}
	if strings.HasPrefix(base, "U") || strings.HasPrefix(base, "B") {
		return Migration{}, false, "ignored: undo and baseline migrations need a paid Flyway edition"
	}
	return Migration{}, false, `ignored by Flyway: migration names start with "V<version>__" or "R__"`
}

func describe(s string) string { return strings.ReplaceAll(s, "_", " ") }

func checkVersion(v string) error {
	if v == "" {
		return errors.New("empty version")
	}
	for _, part := range strings.Split(v, ".") {
		if part == "" {
			return fmt.Errorf("version %q has an empty part", v)
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return fmt.Errorf("version %q is not numeric", v)
			}
		}
	}
	return nil
}

// CompareVersions compares dotted numeric versions the way Flyway does:
// part by part as integers, with missing parts counting as zero, so "1" and
// "1.0" are equal. Inputs must have passed checkVersion.
func CompareVersions(a, b string) int {
	ap, bp := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(ap) || i < len(bp); i++ {
		x, y := "0", "0"
		if i < len(ap) {
			x = ap[i]
		}
		if i < len(bp) {
			y = bp[i]
		}
		x, y = strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
		if len(x) != len(y) {
			if len(x) < len(y) {
				return -1
			}
			return 1
		}
		if c := strings.Compare(x, y); c != 0 {
			return c
		}
	}
	return 0
}

// VersionKey returns a canonical form of v for use as a map key, so that
// "1.0" and "1" collide like they do in Flyway.
func VersionKey(v string) string {
	parts := strings.Split(v, ".")
	for i, p := range parts {
		p = strings.TrimLeft(p, "0")
		if p == "" {
			p = "0"
		}
		parts[i] = p
	}
	for len(parts) > 1 && parts[len(parts)-1] == "0" {
		parts = parts[:len(parts)-1]
	}
	return strings.Join(parts, ".")
}

// ValidVersion reports whether v is a numeric dotted version.
func ValidVersion(v string) bool { return checkVersion(v) == nil }

// BundleID hashes file paths and contents, so identical sources share a
// bundle and any change produces a new one.
func BundleID(files []File) string {
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%s\n", f.Path, f.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// callbackEvents lists Flyway's SQL callback names, so callback files are
// shipped without a warning.
var callbackEvents = map[string]bool{
	"beforeMigrate": true, "beforeRepeatables": true, "beforeEachMigrate": true,
	"beforeEachMigrateStatement": true, "afterEachMigrateStatement": true,
	"afterEachMigrateStatementError": true, "afterEachMigrate": true,
	"afterEachMigrateError": true, "afterMigrate": true, "afterMigrateApplied": true,
	"afterVersioned": true, "afterMigrateError": true,
	"beforeUndo": true, "beforeEachUndo": true, "beforeEachUndoStatement": true,
	"afterEachUndoStatement": true, "afterEachUndoStatementError": true,
	"afterEachUndo": true, "afterEachUndoError": true, "afterUndo": true, "afterUndoError": true,
	"beforeClean": true, "afterClean": true, "afterCleanError": true,
	"beforeInfo": true, "afterInfo": true, "afterInfoError": true,
	"beforeValidate": true, "afterValidate": true, "afterValidateError": true,
	"beforeBaseline": true, "afterBaseline": true, "afterBaselineError": true,
	"beforeRepair": true, "afterRepair": true, "afterRepairError": true,
	"createSchema": true, "beforeConnect": true, "afterConnect": true,
	"beforeEachConnect": true, "afterEachConnect": true,
}
