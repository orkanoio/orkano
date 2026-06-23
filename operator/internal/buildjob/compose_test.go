package buildjob

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
		file                    string
		wantContextURL          string
		wantDockerfilePath      string
		wantGeneratedDockerfile string
		wantImageRef            string
	}{
		{
			// Static strategy (static.dir: public): no DockerfilePath, but a
			// COPY-only Dockerfile is generated onto the static server image.
			file:                    "01-static-site.yaml",
			wantContextURL:          "https://github.com/alice/blog.git#" + composeCommit,
			wantGeneratedDockerfile: "FROM " + StaticServerImage + "\nCOPY public/ " + staticServeRoot + "\n",
			wantImageRef:            RegistryHost + "/blog:" + composeCommit,
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
			inv := Compose(build, DefaultGitBaseURL)
			if inv.ContextURL != tc.wantContextURL {
				t.Errorf("ContextURL = %q, want %q", inv.ContextURL, tc.wantContextURL)
			}
			if inv.DockerfilePath != tc.wantDockerfilePath {
				t.Errorf("DockerfilePath = %q, want %q", inv.DockerfilePath, tc.wantDockerfilePath)
			}
			if inv.GeneratedDockerfile != tc.wantGeneratedDockerfile {
				t.Errorf("GeneratedDockerfile = %q, want %q", inv.GeneratedDockerfile, tc.wantGeneratedDockerfile)
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
			inv := Compose(build, DefaultGitBaseURL)
			if inv.ContextURL != tc.wantContextURL {
				t.Errorf("ContextURL = %q, want %q", inv.ContextURL, tc.wantContextURL)
			}
			if inv.DockerfilePath != tc.wantDockerfile {
				t.Errorf("DockerfilePath = %q, want %q", inv.DockerfilePath, tc.wantDockerfile)
			}
		})
	}
}

// TestComposeHonorsGitBaseURL pins the --git-base-url thread: a configured base
// (the hermetic E2E's in-cluster git server, or an air-gapped mirror) prefixes
// the git context verbatim, an empty base falls back to DefaultGitBaseURL, and
// the push target (ImageRef) is base-independent — the registry host is fixed.
func TestComposeHonorsGitBaseURL(t *testing.T) {
	build := &orkanov1alpha1.Build{
		Spec: orkanov1alpha1.BuildSpec{
			AppName: "blog",
			Commit:  composeCommit,
			Source: orkanov1alpha1.Source{
				GitHub:  orkanov1alpha1.GitHubSource{Repo: "alice/blog"},
				SubPath: "site",
			},
			Strategy: orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyDockerfile},
		},
	}
	for _, tc := range []struct {
		name           string
		gitBaseURL     string
		wantContextURL string
	}{
		{
			name:           "in-cluster http base",
			gitBaseURL:     "http://gitfixture.orkano-system.svc/",
			wantContextURL: "http://gitfixture.orkano-system.svc/alice/blog.git#" + composeCommit + ":site",
		},
		{
			name:           "empty falls back to the github.com default",
			gitBaseURL:     "",
			wantContextURL: DefaultGitBaseURL + "alice/blog.git#" + composeCommit + ":site",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := Compose(build, tc.gitBaseURL)
			if inv.ContextURL != tc.wantContextURL {
				t.Errorf("ContextURL = %q, want %q", inv.ContextURL, tc.wantContextURL)
			}
			if want := RegistryHost + "/blog:" + composeCommit; inv.ImageRef != want {
				t.Errorf("ImageRef = %q, want %q (base must not affect the push target)", inv.ImageRef, want)
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
		if strings.HasPrefix(a, "--opt=filename=") {
			t.Errorf("empty DockerfilePath still emitted a filename opt: %q", a)
		}
	}
}

// TestRenderStaticMode pins the Static branch of Render: the dockerfilekey
// injection flags, the init container that writes the generated Dockerfile from
// an env var (data, never shell), and the read-only dockerfile mount — none of
// which a Dockerfile build renders.
func TestRenderStaticMode(t *testing.T) {
	build, opts := goldenInputs()
	opts.DockerfilePath = ""
	opts.GeneratedDockerfile = "FROM " + StaticServerImage + "\nCOPY public/ " + staticServeRoot + "\n"
	job, err := Render(build, opts)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	pod := job.Spec.Template.Spec

	args := pod.Containers[0].Args
	out := slices.Index(args, "--output=type=image,name="+opts.ImageRef+",push=true")
	for _, want := range []string{
		"--local=dockerfile=" + dockerfileMountPath,
		"--opt=dockerfilekey=" + dockerfileLocalName,
		"--opt=filename=Dockerfile",
	} {
		i := slices.Index(args, want)
		if i < 0 {
			t.Errorf("static args %v missing %q", args, want)
		} else if out >= 0 && i >= out {
			t.Errorf("%q must precede --output in %v", want, args)
		}
	}

	if len(pod.InitContainers) != 1 {
		t.Fatalf("static build rendered %d init containers, want 1", len(pod.InitContainers))
	}
	ini := pod.InitContainers[0]
	wantCmd := []string{"sh", "-c", `printf '%s' "$ORKANO_DOCKERFILE" > ` + dockerfileMountPath + "/" + DefaultDockerfile}
	if !slices.Equal(ini.Command, wantCmd) {
		t.Errorf("init Command = %v, want %v", ini.Command, wantCmd)
	}
	if len(ini.Args) != 0 {
		t.Errorf("init Args = %v, want none (everything is in Command)", ini.Args)
	}
	if got := envValue(ini, "ORKANO_DOCKERFILE"); got != opts.GeneratedDockerfile {
		t.Errorf("init ORKANO_DOCKERFILE = %q, want the generated Dockerfile %q", got, opts.GeneratedDockerfile)
	}
	if ini.Image != pod.Containers[0].Image {
		t.Errorf("init image %q should reuse the build image %q", ini.Image, pod.Containers[0].Image)
	}
	// The init container must mount the dockerfile volume writable; a read-only
	// mount would make the printf write fail at runtime (the build container
	// mounts the same volume read-only).
	writable := false
	for _, m := range ini.VolumeMounts {
		if m.Name == dockerfileLocalName && m.MountPath == dockerfileMountPath {
			writable = !m.ReadOnly
		}
	}
	if !writable {
		t.Error("init container must mount the dockerfile volume writable")
	}
	if sc := ini.SecurityContext; sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot ||
		sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault ||
		sc.Capabilities == nil || !slices.Contains(sc.Capabilities.Drop, corev1.Capability("ALL")) {
		t.Errorf("init securityContext is not restricted-grade: %+v", ini.SecurityContext)
	}

	if !hasEmptyDirVolume(pod.Volumes, dockerfileLocalName) {
		t.Errorf("static build missing the %q emptyDir volume: %+v", dockerfileLocalName, pod.Volumes)
	}
	if !hasReadOnlyMount(pod.Containers[0].VolumeMounts, dockerfileLocalName, dockerfileMountPath) {
		t.Errorf("build container does not read-only-mount the dockerfile volume: %+v", pod.Containers[0].VolumeMounts)
	}

	// A Dockerfile build renders none of the static machinery.
	df, dopts := goldenInputs()
	djob, err := Render(df, dopts)
	if err != nil {
		t.Fatalf("Render dockerfile: %v", err)
	}
	dpod := djob.Spec.Template.Spec
	if len(dpod.InitContainers) != 0 {
		t.Errorf("Dockerfile build rendered %d init containers, want 0", len(dpod.InitContainers))
	}
	if hasEmptyDirVolume(dpod.Volumes, dockerfileLocalName) {
		t.Error("Dockerfile build rendered a dockerfile volume it should not")
	}
	for _, a := range dpod.Containers[0].Args {
		if strings.HasPrefix(a, "--local=") || strings.HasPrefix(a, "--opt=dockerfilekey=") {
			t.Errorf("Dockerfile build emitted a static-only flag: %q", a)
		}
	}
}

// TestStaticDockerfileNormalizesDir pins that a trailing slash on static.dir
// (the CRD pattern admits it) yields one canonical COPY, matching the no-slash
// form — the static analogue of TestComposeNormalizesSubPath.
func TestStaticDockerfileNormalizesDir(t *testing.T) {
	want := "FROM " + StaticServerImage + "\nCOPY public/ " + staticServeRoot + "\n"
	for _, dir := range []string{"public", "public/"} {
		if got := staticDockerfile(dir); got != want {
			t.Errorf("staticDockerfile(%q) = %q, want %q", dir, got, want)
		}
	}
}

func envValue(c corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func hasEmptyDirVolume(vols []corev1.Volume, name string) bool {
	for _, v := range vols {
		if v.Name == name && v.EmptyDir != nil {
			return true
		}
	}
	return false
}

func hasReadOnlyMount(mounts []corev1.VolumeMount, name, path string) bool {
	for _, m := range mounts {
		if m.Name == name && m.MountPath == path && m.ReadOnly {
			return true
		}
	}
	return false
}
