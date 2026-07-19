package features

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

func TestDefinitionsAreStableUnsafeIDs(t *testing.T) {
	want := []ID{SourceGit, SourceZip, BuildNixpacks}
	got := Definitions()
	if len(got) != len(want) {
		t.Fatalf("Definitions() returned %d entries, want %d", len(got), len(want))
	}
	for i, definition := range got {
		if definition.ID != want[i] {
			t.Fatalf("definition %d ID = %q, want %q", i, definition.ID, want[i])
		}
		if definition.Name == "" || definition.Description == "" || !definition.Unsafe {
			t.Fatalf("definition %q is missing unsafe product metadata: %#v", definition.ID, definition)
		}
	}

	got[0].Name = "changed"
	if Definitions()[0].Name == "changed" {
		t.Fatal("Definitions returned mutable package storage")
	}
}

func TestParseSecureDefaultAndCanonicalCSV(t *testing.T) {
	empty, err := Parse(nil)
	if err != nil {
		t.Fatalf("Parse(nil): %v", err)
	}
	if empty.CSV() != "" || empty.Enabled(SourceGit) {
		t.Fatalf("empty set = %q, source.git enabled = %v", empty.CSV(), empty.Enabled(SourceGit))
	}
	var zero Set
	if zero.CSV() != "" || zero.Enabled(SourceZip) {
		t.Fatalf("zero set = %q, source.zip enabled = %v", zero.CSV(), zero.Enabled(SourceZip))
	}

	set, err := Parse([]string{" source.zip ", "build.nixpacks", "source.git", "source.zip"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := set.CSV(), "build.nixpacks,source.git,source.zip"; got != want {
		t.Fatalf("CSV() = %q, want %q", got, want)
	}
	for _, id := range []ID{SourceGit, SourceZip, BuildNixpacks} {
		if !set.Enabled(id) {
			t.Errorf("Enabled(%q) = false", id)
		}
	}
	if set.Enabled(ID("unknown")) {
		t.Fatal("unknown ID reported enabled")
	}
	if got, want := set.IDs(), []ID{BuildNixpacks, SourceGit, SourceZip}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs() = %#v, want %#v", got, want)
	}
}

func TestParseRejectsInvalidIDs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		want   string
	}{
		{name: "empty", values: []string{""}, want: "must not be empty"},
		{name: "whitespace", values: []string{"  "}, want: "must not be empty"},
		{name: "unknown", values: []string{"source.gti"}, want: `unknown unsafe feature "source.gti"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, err := Parse(tc.values)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse(%q) = (%q, %v), want error containing %q", tc.values, set.CSV(), err, tc.want)
			}
			if set.CSV() != "" {
				t.Fatalf("failed parse returned partially enabled set %q", set.CSV())
			}
		})
	}
}

func TestParseCSVRoundTrip(t *testing.T) {
	for _, value := range []string{"", "   ", "source.git", "source.zip, build.nixpacks,source.git"} {
		set, err := ParseCSV(value)
		if err != nil {
			t.Fatalf("ParseCSV(%q): %v", value, err)
		}
		roundTrip, err := ParseCSV(set.CSV())
		if err != nil {
			t.Fatalf("ParseCSV(CSV(%q)): %v", value, err)
		}
		if roundTrip.CSV() != set.CSV() {
			t.Fatalf("round trip = %q, want %q", roundTrip.CSV(), set.CSV())
		}
	}

	if _, err := ParseCSV("source.git,,source.zip"); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("ParseCSV with empty member error = %v", err)
	}
}

func TestRequiredAndMissingForApp(t *testing.T) {
	github := orkanov1alpha1.AppSpec{
		Source: orkanov1alpha1.Source{GitHub: &orkanov1alpha1.GitHubSource{Repo: "orkanoio/demo"}},
		Build:  orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyDockerfile},
	}
	if got := RequiredForApp(github); len(got) != 0 {
		t.Fatalf("core app requires unsafe gates: %#v", got)
	}

	unsafe := orkanov1alpha1.AppSpec{
		Source: orkanov1alpha1.Source{Git: &orkanov1alpha1.GitSource{URL: "https://example.com/acme/app.git"}},
		Build: orkanov1alpha1.BuildStrategy{
			Strategy: orkanov1alpha1.StrategyNixpacks,
			Nixpacks: &orkanov1alpha1.NixpacksBuild{},
		},
	}
	if got, want := RequiredForApp(unsafe), []ID{SourceGit, BuildNixpacks}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RequiredForApp() = %#v, want %#v", got, want)
	}

	partial, err := Parse([]string{string(SourceGit)})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := partial.MissingForApp(unsafe), []ID{BuildNixpacks}; !reflect.DeepEqual(got, want) {
		t.Fatalf("MissingForApp() = %#v, want %#v", got, want)
	}
	if err := partial.ValidateApp(unsafe); !errors.Is(err, ErrDisabled) {
		t.Fatalf("ValidateApp error = %v, want ErrDisabled", err)
	} else {
		var disabled *DisabledError
		if !errors.As(err, &disabled) || !reflect.DeepEqual(disabled.IDs, []ID{BuildNixpacks}) {
			t.Fatalf("ValidateApp disabled error = %#v", err)
		}
	}

	all, err := Parse([]string{string(SourceGit), string(BuildNixpacks)})
	if err != nil {
		t.Fatal(err)
	}
	if err := all.ValidateApp(unsafe); err != nil {
		t.Fatalf("ValidateApp with gates enabled: %v", err)
	}
}

func TestUploadedSourceRequiresZipGate(t *testing.T) {
	spec := orkanov1alpha1.AppSpec{
		Source: orkanov1alpha1.Source{Upload: &orkanov1alpha1.UploadSource{Digest: "sha256:" + strings.Repeat("a", 64)}},
		Build:  orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyStatic, Static: &orkanov1alpha1.StaticBuild{Dir: "dist"}},
	}
	if got, want := RequiredForApp(spec), []ID{SourceZip}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RequiredForApp(upload) = %#v, want %#v", got, want)
	}
}
