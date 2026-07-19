package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSourceExistingGitHubJSONRemainsWireCompatible(t *testing.T) {
	existing := []byte(`{"github":{"repo":"orkanoio/demo","ref":"main"},"subPath":"web"}`)
	var source Source
	if err := json.Unmarshal(existing, &source); err != nil {
		t.Fatalf("unmarshal existing v1alpha1 Source: %v", err)
	}
	if source.GitHub == nil || source.GitHub.Repo != "orkanoio/demo" || source.GitHub.Ref != "main" {
		t.Fatalf("unmarshal existing v1alpha1 Source = %#v", source)
	}
	if source.Git != nil || source.Upload != nil || source.SubPath != "web" {
		t.Fatalf("unmarshal introduced another source member: %#v", source)
	}

	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatalf("marshal migrated Source: %v", err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(existing, &want); err != nil {
		t.Fatal(err)
	}
	if !mapsEqual(got, want) {
		t.Fatalf("wire shape changed: got %s, want %s", encoded, existing)
	}
}

func TestSourceVariantJSONOmitsUnselectedMembers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		source     Source
		wantMember string
	}{
		{
			name:       "github",
			source:     Source{GitHub: &GitHubSource{Repo: "orkanoio/demo"}},
			wantMember: `"github"`,
		},
		{
			name:       "git",
			source:     Source{Git: &GitSource{URL: "https://example.com/acme/demo.git"}},
			wantMember: `"git"`,
		},
		{
			name:       "upload",
			source:     Source{Upload: &UploadSource{Digest: "sha256:" + strings.Repeat("a", 64), FileName: "demo.zip"}},
			wantMember: `"upload"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.source)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(encoded), tc.wantMember) {
				t.Fatalf("marshal = %s, want member %s", encoded, tc.wantMember)
			}
			for _, member := range []string{`"github"`, `"git"`, `"upload"`} {
				if member != tc.wantMember && strings.Contains(string(encoded), member) {
					t.Fatalf("marshal = %s, unexpectedly contains %s", encoded, member)
				}
			}
		})
	}
}

func TestAppDeepCopyDoesNotAliasSourceOrNixpacks(t *testing.T) {
	app := &App{
		Spec: AppSpec{
			Source: Source{Git: &GitSource{URL: "https://example.com/acme/demo.git", Ref: "main"}},
			Build: BuildStrategy{
				Strategy: StrategyNixpacks,
				Nixpacks: &NixpacksBuild{ConfigPath: "deploy/nixpacks.toml"},
			},
		},
	}
	copy := app.DeepCopy()
	copy.Spec.Source.Git.Ref = "release"
	copy.Spec.Build.Nixpacks.ConfigPath = "nixpacks.toml"
	if app.Spec.Source.Git.Ref != "main" {
		t.Fatalf("DeepCopy aliased Git source: %#v", app.Spec.Source.Git)
	}
	if app.Spec.Build.Nixpacks.ConfigPath != "deploy/nixpacks.toml" {
		t.Fatalf("DeepCopy aliased Nixpacks config: %#v", app.Spec.Build.Nixpacks)
	}
}

func mapsEqual(left, right map[string]any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}
