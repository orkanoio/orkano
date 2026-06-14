package buildjob

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

// commit is the pinned 40-char SHA every composed Build snapshots; the
// dispatcher resolves a ref to one of these at trigger time (M1.3).
const composeCommit = "0123456789abcdef0123456789abcdef01234567"

// TestComposeOverExamplePermutations drives Compose with a Build snapshotted
// from each of the five archetype Apps — exactly as the dispatcher (and the
// examples_test supplyImage helper) snapshot source+strategy — and pins the
// composed git context, Dockerfile path, and push target. Loading the real
// docs/examples files keeps the permutations honest: if an archetype's source
// or strategy shape changes, this test moves with it.
func TestComposeOverExamplePermutations(t *testing.T) {
	cases := []struct {
		file               string
		wantContextURL     string
		wantDockerfilePath string
		wantImageRef       string
	}{
		{
			// Static strategy, default branch, no subPath: no Dockerfile is
			// composed — the static-strategy task generates one.
			file:           "01-static-site.yaml",
			wantContextURL: "https://github.com/alice/blog.git#" + composeCommit,
			wantImageRef:   RegistryHost + "/blog:" + composeCommit,
		},
		{
			// Dockerfile strategy with no dockerfile block (the valid CEL
			// edge): filename defaults to Dockerfile.
			file:               "02-web-service-postgres.yaml",
			wantContextURL:     "https://github.com/alice/api.git#" + composeCommit,
			wantDockerfilePath: DefaultDockerfile,
			wantImageRef:       RegistryHost + "/api:" + composeCommit,
		},
		{
			// Same source as 02 but a different App, so the image repository
			// is the App name, not the GitHub repo.
			file:               "03-background-worker.yaml",
			wantContextURL:     "https://github.com/alice/api.git#" + composeCommit,
			wantDockerfilePath: DefaultDockerfile,
			wantImageRef:       RegistryHost + "/mailer:" + composeCommit,
		},
		{
			// subPath scopes the git context; the default Dockerfile is then
			// relative to that directory.
			file:               "04-monorepo-subpath.yaml",
			wantContextURL:     "https://github.com/acme/platform.git#" + composeCommit + ":services/billing",
			wantDockerfilePath: DefaultDockerfile,
			wantImageRef:       RegistryHost + "/billing:" + composeCommit,
		},
		{
			// Explicit non-default Dockerfile path passes through verbatim.
			file:               "05-dockerfile.yaml",
			wantContextURL:     "https://github.com/alice/imageproc.git#" + composeCommit,
			wantDockerfilePath: "deploy/prod.Dockerfile",
			wantImageRef:       RegistryHost + "/imageproc:" + composeCommit,
		},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			build := snapshotBuild(t, tc.file)
			inv := Compose(build)
			if inv.ContextURL != tc.wantContextURL {
				t.Errorf("ContextURL = %q, want %q", inv.ContextURL, tc.wantContextURL)
			}
			if inv.DockerfilePath != tc.wantDockerfilePath {
				t.Errorf("DockerfilePath = %q, want %q", inv.DockerfilePath, tc.wantDockerfilePath)
			}
			if inv.ImageRef != tc.wantImageRef {
				t.Errorf("ImageRef = %q, want %q", inv.ImageRef, tc.wantImageRef)
			}
		})
	}
}

// TestComposeNormalizesSubPath pins the slash edges the CRD pattern admits but
// BuildKit's git fetcher does not expect, and that subPath composes
// independently of an explicit Dockerfile path (which stays relative to the
// subPath-scoped context). None of the five archetypes exercise these, so this
// is hand-built rather than example-driven.
func TestComposeNormalizesSubPath(t *testing.T) {
	const base = "https://github.com/acme/platform.git#" + composeCommit
	for _, tc := range []struct {
		name           string
		subPath        string
		dockerfilePath string
		wantContextURL string
		wantDockerfile string
	}{
		{
			name:           "trailing slash trimmed",
			subPath:        "services/billing/",
			wantContextURL: base + ":services/billing",
			wantDockerfile: DefaultDockerfile,
		},
		{
			name:           "leading slash trimmed",
			subPath:        "/services/billing",
			wantContextURL: base + ":services/billing",
			wantDockerfile: DefaultDockerfile,
		},
		{
			name:           "bare slash means repo root",
			subPath:        "/",
			wantContextURL: base,
			wantDockerfile: DefaultDockerfile,
		},
		{
			name:           "subPath with explicit dockerfile path",
			subPath:        "services/billing",
			dockerfilePath: "docker/prod.Dockerfile",
			wantContextURL: base + ":services/billing",
			wantDockerfile: "docker/prod.Dockerfile",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build := &orkanov1alpha1.Build{
				Spec: orkanov1alpha1.BuildSpec{
					AppName: "billing",
					Commit:  composeCommit,
					Source: orkanov1alpha1.Source{
						GitHub:  orkanov1alpha1.GitHubSource{Repo: "acme/platform"},
						SubPath: tc.subPath,
					},
					Strategy: orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyDockerfile},
				},
			}
			if tc.dockerfilePath != "" {
				build.Spec.Strategy.Dockerfile = &orkanov1alpha1.DockerfileBuild{Path: tc.dockerfilePath}
			}
			inv := Compose(build)
			if inv.ContextURL != tc.wantContextURL {
				t.Errorf("ContextURL = %q, want %q", inv.ContextURL, tc.wantContextURL)
			}
			if inv.DockerfilePath != tc.wantDockerfile {
				t.Errorf("DockerfilePath = %q, want %q", inv.DockerfilePath, tc.wantDockerfile)
			}
		})
	}
}

// snapshotBuild reads the App from an example file and snapshots its source and
// strategy into a Build the way the dispatcher will — the only shape Compose
// ever sees in production.
func snapshotBuild(t *testing.T, file string) *orkanov1alpha1.Build {
	t.Helper()
	app := loadExampleApp(t, file)
	return &orkanov1alpha1.Build{
		Spec: orkanov1alpha1.BuildSpec{
			AppName:  app.Name,
			Commit:   composeCommit,
			Source:   app.Spec.Source,
			Strategy: app.Spec.Build,
		},
	}
}

// loadExampleApp returns the App document from a docs/examples file; some
// files also carry a Domain, so it scans every document for the App.
func loadExampleApp(t *testing.T, file string) *orkanov1alpha1.App {
	t.Helper()
	raw, err := os.ReadFile("../../../docs/examples/" + file)
	if err != nil {
		t.Fatalf("reading example %s: %v", file, err)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var app orkanov1alpha1.App
		if err := dec.Decode(&app); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decoding example %s: %v", file, err)
		}
		if app.Kind == "App" {
			return &app
		}
	}
	t.Fatalf("example %s has no App document", file)
	return nil
}

// TestRenderEmitsDockerfileFilename pins that the filename opt appears only
// when a Dockerfile path is composed, and lands between the context and the
// output so buildctl parses it as a frontend opt.
func TestRenderEmitsDockerfileFilename(t *testing.T) {
	build, opts := goldenInputs()

	opts.DockerfilePath = "deploy/prod.Dockerfile"
	job, err := Render(build, opts)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	args := job.Spec.Template.Spec.Containers[0].Args
	want := "--opt=filename=deploy/prod.Dockerfile"
	ctxIdx := slices.Index(args, "--opt=context="+opts.ContextURL)
	fileIdx := slices.Index(args, want)
	outIdx := slices.Index(args, "--output=type=image,name="+opts.ImageRef+",push=true")
	if ctxIdx < 0 || fileIdx < 0 || outIdx < 0 {
		t.Fatalf("args %v missing an expected opt (context %d, filename %d, output %d)", args, ctxIdx, fileIdx, outIdx)
	}
	if ctxIdx >= fileIdx || fileIdx >= outIdx {
		t.Errorf("filename opt out of order in %v (context %d, filename %d, output %d)", args, ctxIdx, fileIdx, outIdx)
	}

	opts.DockerfilePath = ""
	job, err = Render(build, opts)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, a := range job.Spec.Template.Spec.Containers[0].Args {
		if len(a) >= len("--opt=filename=") && a[:len("--opt=filename=")] == "--opt=filename=" {
			t.Errorf("empty DockerfilePath still emitted a filename opt: %q", a)
		}
	}
}
