package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

func validSourceValidationApp(name string) *orkanov1alpha1.App {
	return &orkanov1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace},
		Spec: orkanov1alpha1.AppSpec{
			Source: orkanov1alpha1.Source{
				GitHub: &orkanov1alpha1.GitHubSource{Repo: "orkanoio/demo"},
			},
			Build: orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyDockerfile},
		},
	}
}

func dryRunSourceValidationApp(app *orkanov1alpha1.App) error {
	return k8sClient.Create(context.Background(), app, client.DryRunAll)
}

func TestSourceUnionAcceptsEachExactVariant(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source orkanov1alpha1.Source
	}{
		{
			name:   "github",
			source: orkanov1alpha1.Source{GitHub: &orkanov1alpha1.GitHubSource{Repo: "orkanoio/demo"}},
		},
		{
			name:   "git",
			source: orkanov1alpha1.Source{Git: &orkanov1alpha1.GitSource{URL: "https://git.example.com/orkano/demo.git", Ref: "main"}},
		},
		{
			name: "upload",
			source: orkanov1alpha1.Source{Upload: &orkanov1alpha1.UploadSource{
				Digest:   "sha256:" + strings.Repeat("a", 64),
				FileName: "demo.zip",
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := validSourceValidationApp("source-variant-" + tc.name)
			app.Spec.Source = tc.source
			if err := dryRunSourceValidationApp(app); err != nil {
				t.Fatalf("valid %s source rejected: %v", tc.name, err)
			}
		})
	}
}

func TestSourceUnionRejectsZeroOrMultipleVariants(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source orkanov1alpha1.Source
	}{
		{name: "none", source: orkanov1alpha1.Source{}},
		{
			name: "github-and-git",
			source: orkanov1alpha1.Source{
				GitHub: &orkanov1alpha1.GitHubSource{Repo: "orkanoio/demo"},
				Git:    &orkanov1alpha1.GitSource{URL: "https://git.example.com/orkano/demo.git"},
			},
		},
		{
			name: "all",
			source: orkanov1alpha1.Source{
				GitHub: &orkanov1alpha1.GitHubSource{Repo: "orkanoio/demo"},
				Git:    &orkanov1alpha1.GitSource{URL: "https://git.example.com/orkano/demo.git"},
				Upload: &orkanov1alpha1.UploadSource{Digest: "sha256:" + strings.Repeat("a", 64)},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := validSourceValidationApp("source-union-" + tc.name)
			app.Spec.Source = tc.source
			err := dryRunSourceValidationApp(app)
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "exactly one of github, git, or upload must be set") {
				t.Fatalf("invalid source union error = %v", err)
			}
		})
	}
}

func TestGenericGitURLRejectsNonHTTPSOrDecoratedShapes(t *testing.T) {
	for i, value := range []string{
		"http://git.example.com/orkano/demo.git",
		"ssh://git@git.example.com/orkano/demo.git",
		"https://user@git.example.com/orkano/demo.git",
		"https://git.example.com:8443/orkano/demo.git",
		"https://git.example.com/orkano/demo.git?token=secret",
		"https://git.example.com/orkano/demo.git#main",
	} {
		app := validSourceValidationApp("git-url-bad-" + string(rune('a'+i)))
		app.Spec.Source = orkanov1alpha1.Source{Git: &orkanov1alpha1.GitSource{URL: value}}
		err := dryRunSourceValidationApp(app)
		if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "should match") {
			t.Errorf("URL %q error = %v, want pattern rejection", value, err)
		}
	}
}

func TestUploadDigestAndFileNameValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		digest   string
		fileName string
	}{
		{name: "short-digest", digest: "sha256:" + strings.Repeat("a", 63), fileName: "demo.zip"},
		{name: "uppercase-digest", digest: "sha256:" + strings.Repeat("A", 64), fileName: "demo.zip"},
		{name: "path-filename", digest: "sha256:" + strings.Repeat("a", 64), fileName: "../demo.zip"},
		{name: "wrong-extension", digest: "sha256:" + strings.Repeat("a", 64), fileName: "demo.tar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := validSourceValidationApp("upload-bad-" + tc.name)
			app.Spec.Source = orkanov1alpha1.Source{Upload: &orkanov1alpha1.UploadSource{Digest: tc.digest, FileName: tc.fileName}}
			if err := dryRunSourceValidationApp(app); !apierrors.IsInvalid(err) {
				t.Fatalf("invalid upload source error = %v", err)
			}
		})
	}
}

func TestNixpacksStrategyRequiresMatchingMember(t *testing.T) {
	valid := validSourceValidationApp("nixpacks-valid")
	valid.Spec.Build = orkanov1alpha1.BuildStrategy{
		Strategy: orkanov1alpha1.StrategyNixpacks,
		Nixpacks: &orkanov1alpha1.NixpacksBuild{ConfigPath: "deploy/nixpacks.toml"},
	}
	if err := dryRunSourceValidationApp(valid); err != nil {
		t.Fatalf("valid Nixpacks strategy rejected: %v", err)
	}

	for _, tc := range []struct {
		name  string
		build orkanov1alpha1.BuildStrategy
	}{
		{
			name:  "missing-member",
			build: orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyNixpacks},
		},
		{
			name: "static-member",
			build: orkanov1alpha1.BuildStrategy{
				Strategy: orkanov1alpha1.StrategyNixpacks,
				Static:   &orkanov1alpha1.StaticBuild{Dir: "dist"},
			},
		},
		{
			name: "nixpacks-member-on-dockerfile",
			build: orkanov1alpha1.BuildStrategy{
				Strategy: orkanov1alpha1.StrategyDockerfile,
				Nixpacks: &orkanov1alpha1.NixpacksBuild{},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := validSourceValidationApp("nixpacks-bad-" + tc.name)
			app.Spec.Build = tc.build
			err := dryRunSourceValidationApp(app)
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "build members must match the chosen strategy") {
				t.Fatalf("invalid Nixpacks union error = %v", err)
			}
		})
	}
}

func TestBuildCommitAcceptsGitAndArtifactDigests(t *testing.T) {
	for _, length := range []int{40, 64} {
		build := &orkanov1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("commit-valid-%d", length), Namespace: appsNamespace},
			Spec: orkanov1alpha1.BuildSpec{
				AppName: "demo",
				Commit:  strings.Repeat("a", length),
				Source: orkanov1alpha1.Source{
					GitHub: &orkanov1alpha1.GitHubSource{Repo: "orkanoio/demo"},
				},
				Strategy: orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyDockerfile},
			},
		}
		if err := k8sClient.Create(context.Background(), build, client.DryRunAll); err != nil {
			t.Errorf("%d-character commit rejected: %v", length, err)
		}
	}

	invalid := &orkanov1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "commit-invalid", Namespace: appsNamespace},
		Spec: orkanov1alpha1.BuildSpec{
			AppName: "demo",
			Commit:  strings.Repeat("a", 63),
			Source: orkanov1alpha1.Source{
				Upload: &orkanov1alpha1.UploadSource{Digest: "sha256:" + strings.Repeat("a", 64)},
			},
			Strategy: orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyDockerfile},
		},
	}
	err := k8sClient.Create(context.Background(), invalid, client.DryRunAll)
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "should match") {
		t.Fatalf("63-character commit error = %v", err)
	}
}
