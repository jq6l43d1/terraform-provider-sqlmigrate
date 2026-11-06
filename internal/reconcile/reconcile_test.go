package reconcile

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/manifest"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/migrations"
)

func v(version, desc string, sum int32) migrations.Migration {
	return migrations.Migration{Kind: migrations.Versioned, Version: version, Description: desc,
		Path: "V" + strings.ReplaceAll(version, ".", "_") + "__" + strings.ReplaceAll(desc, " ", "_") + ".sql", Checksum: sum}
}

func r(desc string, sum int32) migrations.Migration {
	return migrations.Migration{Kind: migrations.Repeatable, Description: desc, Path: "R__" + desc + ".sql", Checksum: sum}
}

func av(version, desc string, sum *int32) manifest.Applied {
	return manifest.Applied{Category: manifest.CategoryVersioned, Version: version, Description: desc, Type: "SQL", State: "Success", Checksum: sum}
}

func ar(desc string, sum *int32) manifest.Applied {
	return manifest.Applied{Category: manifest.CategoryRepeatable, Description: desc, Type: "SQL", State: "Success", Checksum: sum}
}

func p(i int32) *int32 { return &i }

func rec(applied ...manifest.Applied) *manifest.Record {
	return &manifest.Record{Schema: manifest.Schema, HistoryRead: true, Applied: applied}
}

func pendingLabels(res Result) []string {
	out := []string{}
	for _, m := range res.Pending {
		out = append(out, Label(m))
	}
	return out
}

func TestNoManifestMeansEverythingPending(t *testing.T) {
	src := []migrations.Migration{v("1", "a", 1), r("views", 9)}
	res := Evaluate(src, nil)
	if len(res.Applied) != 0 || len(res.Pending) != 2 {
		t.Fatalf("got %+v", res)
	}
	if !reflect.DeepEqual(res.Expected, []string{"1: a", "R: views (checksum 9)"}) {
		t.Fatalf("expected = %v", res.Expected)
	}
	res = Evaluate(src, &manifest.Record{Schema: 1, HistoryRead: false})
	if len(res.Pending) != 2 {
		t.Fatalf("unread history should count as nothing applied: %+v", res)
	}
}

func TestNewVersionedAndChangedRepeatable(t *testing.T) {
	src := []migrations.Migration{v("1", "a", 1), v("2", "b", 2), r("views", 10)}
	res := Evaluate(src, rec(av("1", "a", p(1)), ar("views", p(9))))
	wantApplied := []string{"1: a", "R: views (checksum 9)"}
	wantExpected := []string{"1: a", "2: b", "R: views (checksum 10)"}
	if !reflect.DeepEqual(res.Applied, wantApplied) || !reflect.DeepEqual(res.Expected, wantExpected) {
		t.Fatalf("applied=%v expected=%v", res.Applied, res.Expected)
	}
	if got := pendingLabels(res); !reflect.DeepEqual(got, []string{"2: b", "R: views (checksum 10)"}) {
		t.Fatalf("pending = %v", got)
	}
	if len(res.Errors)+len(res.Warnings) != 0 {
		t.Fatalf("unexpected diagnostics %v %v", res.Errors, res.Warnings)
	}
}

func TestUpToDate(t *testing.T) {
	src := []migrations.Migration{v("1", "a", 1), r("views", 9)}
	res := Evaluate(src, rec(av("1.0", "a", p(1)), ar("views", p(9))))
	if len(res.Pending) != 0 || !reflect.DeepEqual(res.Applied, res.Expected) {
		t.Fatalf("got %+v", res)
	}
}

func TestEditedVersionedIsAnError(t *testing.T) {
	res := Evaluate([]migrations.Migration{v("1", "a", 2)}, rec(av("1", "a", p(1))))
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "changed after it was applied") {
		t.Fatalf("errors = %v", res.Errors)
	}
	// Unknown checksum: cannot tell, so no error.
	res = Evaluate([]migrations.Migration{v("1", "a", 2)}, rec(av("1", "a", nil)))
	if len(res.Errors) != 0 || len(res.Pending) != 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestRenamedVersionedIsAnError(t *testing.T) {
	res := Evaluate([]migrations.Migration{v("1", "b", 1)}, rec(av("1", "a", p(1))))
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "renamed") {
		t.Fatalf("errors = %v", res.Errors)
	}
}

func TestFailedRowIsAnError(t *testing.T) {
	a := av("2", "b", nil)
	a.State = "Failed"
	res := Evaluate([]migrations.Migration{v("1", "a", 1), v("2", "b", 2)}, rec(av("1", "a", p(1)), a))
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "repair") {
		t.Fatalf("errors = %v", res.Errors)
	}
}

func TestBaselineCountsAsApplied(t *testing.T) {
	base := manifest.Applied{Category: manifest.CategoryVersioned, Version: "5", Description: "<< Flyway Baseline >>", Type: "BASELINE", State: "Baseline"}
	res := Evaluate([]migrations.Migration{v("1", "a", 1), v("5", "e", 5), v("6", "f", 6)}, rec(base))
	if got := pendingLabels(res); !reflect.DeepEqual(got, []string{"6: f"}) {
		t.Fatalf("pending = %v", got)
	}
	if !reflect.DeepEqual(res.Applied, []string{"1: a", "5: e"}) {
		t.Fatalf("applied = %v", res.Applied)
	}
}

func TestWarnings(t *testing.T) {
	src := []migrations.Migration{v("1", "a", 1), v("2", "b", 2), v("4", "d", 4)}
	res := Evaluate(src, rec(av("1", "a", p(1)), av("3", "c", p(3)), av("9", "z", p(9)), ar("gone", p(1))))
	joined := strings.Join(res.Warnings, "\n")
	for _, want := range []string{"older than the newest applied version 9", "version 3 (c) is applied but missing", "version 9 (z) is applied but newer", `repeatable "gone"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings missing %q:\n%s", want, joined)
		}
	}
}
