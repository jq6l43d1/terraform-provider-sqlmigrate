package migrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Values reported by Flyway 13.10.3 ("Applied to database : N") after
// migrating each content against SQLite.
func TestChecksumMatchesFlyway(t *testing.T) {
	cases := []struct {
		content string
		want    int32
	}{
		{"CREATE TABLE a (id INT);\r\nINSERT INTO a VALUES (1);\n", -287911070},
		{"CREATE TABLE a (id INT, x INT);\r\nINSERT INTO a VALUES (1);\n", 379654745},
		{"\xef\xbb\xbfSELECT 1;\n", 78787420},
		{"SELECT 1;", 78787420},
		{"\n\n\nSELECT 1;\n\n", 78787420},
		{"SELECT 1;\rSELECT 2;\r", -1665099012},
		{"-- caf\xc3\xa9 \xe2\x9c\x93\nSELECT 1;\n", 508562274},
		{"  SELECT 1;  \t\n", 1266872113},
	}
	for _, c := range cases {
		if got := Checksum([]byte(c.content)); got != c.want {
			t.Errorf("Checksum(%q) = %d, want %d", c.content, got, c.want)
		}
	}
}

func TestParseName(t *testing.T) {
	cases := []struct {
		name    string
		ok      bool
		kind    Kind
		version string
		desc    string
		warn    bool
	}{
		{"V1__init.sql", true, Versioned, "1", "init", false},
		{"V1_1__create_roles.sql", true, Versioned, "1.1", "create roles", false},
		{"V2026.11.06.1__add_publication.sql", true, Versioned, "2026.11.06.1", "add publication", false},
		{"V3__.sql", true, Versioned, "3", "", false},
		{"R__refresh_views.sql", true, Repeatable, "", "refresh views", false},
		{"V1_init.sql", false, "", "", "", true},
		{"Vx__init.sql", false, "", "", "", true},
		{"afterMigrate.sql", false, "", "", "", false},
		{"beforeEachMigrate__log.sql", false, "", "", "", false},
		{"U1__undo.sql", false, "", "", "", true},
		{"notes.sql", false, "", "", "", true},
	}
	for _, c := range cases {
		m, ok, warn := ParseName(c.name)
		if ok != c.ok || (warn != "") != c.warn {
			t.Errorf("%s: ok=%v warn=%q", c.name, ok, warn)
			continue
		}
		if ok && (m.Kind != c.kind || m.Version != c.version || m.Description != c.desc) {
			t.Errorf("%s: got %+v", c.name, m)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1", "1.0", 0},
		{"1", "1.0.0", 0},
		{"01", "1", 0},
		{"1.1", "1.10", -1},
		{"2", "10", -1},
		{"1.2.3", "1.2", 1},
		{"99999999999999999999", "100000000000000000000", -1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
		if c.want == 0 && VersionKey(c.a) != VersionKey(c.b) {
			t.Errorf("VersionKey(%s)=%s, VersionKey(%s)=%s", c.a, VersionKey(c.a), c.b, VersionKey(c.b))
		}
	}
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "V10__ten.sql", "SELECT 10;\n")
	write(t, dir, "sub/V2__two.sql", "SELECT 2;\n")
	write(t, dir, "V1_1__one_one.sql", "SELECT 11;\n")
	write(t, dir, "R__views.sql", "SELECT 'v';\n")
	write(t, dir, "afterMigrate.sql", "SELECT 'cb';\n")
	write(t, dir, "V2__two.sql.conf", "executeInTransaction=false\n")
	write(t, dir, "README.md", "not shipped\n")
	write(t, dir, "oops.sql", "SELECT 0;\n")
	write(t, dir, ".hidden/V9__nope.sql", "SELECT 9;\n")

	src, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range src.Migrations {
		got = append(got, string(m.Kind)+":"+m.Version+":"+m.Description+":"+m.Path)
	}
	want := []string{
		"versioned:1.1:one one:V1_1__one_one.sql",
		"versioned:2:two:sub/V2__two.sql",
		"versioned:10:ten:V10__ten.sql",
		"repeatable::views:R__views.sql",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("migrations:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	var files []string
	for _, f := range src.Files {
		files = append(files, f.Path)
	}
	wantFiles := "R__views.sql V10__ten.sql V1_1__one_one.sql V2__two.sql.conf afterMigrate.sql oops.sql sub/V2__two.sql"
	if strings.Join(files, " ") != wantFiles {
		t.Fatalf("files = %s", strings.Join(files, " "))
	}
	if len(src.Warnings) != 1 || !strings.Contains(src.Warnings[0], "oops.sql") {
		t.Fatalf("warnings = %v", src.Warnings)
	}

	before := src.BundleID
	write(t, dir, "R__views.sql", "SELECT 'v2';\n")
	src2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if src2.BundleID == before {
		t.Fatal("bundle id did not change after editing a file")
	}
}

func TestLoadRejectsDuplicates(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "V1__a.sql", "SELECT 1;\n")
	write(t, dir, "sub/V1_0__b.sql", "SELECT 1;\n")
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "version 1") {
		t.Fatalf("want duplicate version error, got %v", err)
	}
}
