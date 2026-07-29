package repoallowlist

import (
	"fmt"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	got, err := Normalize([]string{
		"  OrkanoIO/Orkano  ",
		"acme/Widgets",
		"",
		"orkanoio/orkano",
		"ACME/widgets",
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	want := []string{"acme/widgets", "orkanoio/orkano"}
	if len(got) != len(want) {
		t.Fatalf("Normalize = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Normalize[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNormalizeRejectsInvalidRepository(t *testing.T) {
	cases := []string{
		"owner",
		"/repository",
		"owner/",
		"owner/repository/extra",
		"owner/*",
		"owner /repository",
		"ownér/repository",
		strings.Repeat("a", MaxRepositoryLength-4) + "/repo",
	}
	for _, repository := range cases {
		t.Run(repository, func(t *testing.T) {
			if _, err := Normalize([]string{repository}); err == nil {
				t.Fatalf("Normalize(%q) succeeded, want validation error", repository)
			}
		})
	}
}

func TestNormalizeRepositoryLimitAppliesBeforeDeduplication(t *testing.T) {
	duplicates := make([]string, MaxRepositories+1)
	for i := range duplicates {
		duplicates[i] = "owner/repository"
	}
	if _, err := Normalize(duplicates); err == nil {
		t.Fatal("Normalize accepted more than MaxRepositories raw entries")
	}

	tooMany := make([]string, MaxRepositories+1)
	for i := range tooMany {
		tooMany[i] = "owner/repository-" + strings.Repeat("a", i/26) + string(rune('a'+i%26))
	}
	if _, err := Normalize(tooMany); err == nil {
		t.Fatal("Normalize accepted more than MaxRepositories distinct entries")
	}
}

func TestMaximumPolicyFormatsAndParses(t *testing.T) {
	repositories := make([]string, MaxRepositories)
	for i := range repositories {
		repositories[i] = fmt.Sprintf("owner/repository-%03d", i)
	}
	formatted, err := Format(repositories)
	if err != nil {
		t.Fatalf("Format maximum policy: %v", err)
	}
	got, err := Parse(formatted)
	if err != nil {
		t.Fatalf("Parse maximum formatted policy: %v", err)
	}
	if len(got) != MaxRepositories {
		t.Fatalf("Parse maximum formatted policy returned %d entries, want %d", len(got), MaxRepositories)
	}
}

func TestParseAndFormat(t *testing.T) {
	raw := " OrkanoIO/Orkano \r\n\nacme/Widgets\norkanoio/orkano\n"
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	formatted, err := Format(got)
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if want := "acme/widgets\norkanoio/orkano\n"; formatted != want {
		t.Fatalf("Format(Parse(...)) = %q, want %q", formatted, want)
	}

	roundTrip, err := Parse(formatted)
	if err != nil {
		t.Fatalf("Parse formatted: %v", err)
	}
	if len(roundTrip) != len(got) {
		t.Fatalf("round trip = %v, want %v", roundTrip, got)
	}
	for i := range got {
		if roundTrip[i] != got[i] {
			t.Errorf("round trip[%d] = %q, want %q", i, roundTrip[i], got[i])
		}
	}
}

func TestEmptyPolicyIsValidDenyAll(t *testing.T) {
	got, err := Parse("")
	if err != nil {
		t.Fatalf("Parse empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Parse empty = %v, want no entries", got)
	}
	formatted, err := Format(nil)
	if err != nil {
		t.Fatalf("Format nil: %v", err)
	}
	if formatted != "" {
		t.Fatalf("Format nil = %q, want empty", formatted)
	}
}

func TestParseRejectsCommaDelimitedPolicy(t *testing.T) {
	if _, err := Parse("owner/one,owner/two"); err == nil {
		t.Fatal("Parse accepted comma-delimited content; ConfigMap format is one repository per line")
	}
}
