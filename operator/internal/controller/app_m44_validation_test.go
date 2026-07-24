// Apiserver-level schema validation for the M4.4 AppSpec additions — the image
// source, the volume list, and the release pin — proven against envtest's real
// CEL + OpenAPI pattern engine. A server-side dry-run create exercises
// admission without persisting an object, so these run in plain `make test`
// with no cluster of their own.
package controller

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

// pinnedImage is a syntactically valid digest-pinned reference.
const pinnedImage = "ghcr.io/immich-app/immich-server@sha256:" +
	"1111111111111111111111111111111111111111111111111111111111111111"

func dryRunApp(name string, mutate func(*orkanov1alpha1.AppSpec)) error {
	spec := orkanov1alpha1.AppSpec{
		Source: orkanov1alpha1.Source{GitHub: &orkanov1alpha1.GitHubSource{Repo: "alice/app"}},
		Build:  orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyDockerfile},
	}
	mutate(&spec)
	app := &orkanov1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace},
		Spec:       spec,
	}
	return k8sClient.Create(context.Background(), app, client.DryRunAll)
}

// imageApp is the shape a deploy-from-image App takes: the image source paired
// with the no-op Image strategy.
func imageApp(spec *orkanov1alpha1.AppSpec, ref string) {
	spec.Source = orkanov1alpha1.Source{Image: &orkanov1alpha1.ImageSource{Ref: ref}}
	spec.Build = orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyImage}
}

func TestImageSourceAccepted(t *testing.T) {
	if err := dryRunApp("m44-image-ok", func(s *orkanov1alpha1.AppSpec) {
		imageApp(s, pinnedImage)
	}); err != nil {
		t.Fatalf("a digest-pinned image source should be admitted, got: %v", err)
	}
}

// INV-06 becomes an APISERVER guarantee for this path: a tag can never be
// stored, so no reconciler has to refuse it later.
func TestImageSourceRejectsTagOnly(t *testing.T) {
	for _, ref := range []string{
		"ghcr.io/immich-app/immich-server:v1.119.0",
		"ghcr.io/immich-app/immich-server",
		"nginx@sha256:1111111111111111111111111111111111111111111111111111111111111111", // no registry host
		"ghcr.io/immich-app/immich-server@sha256:abc",                                   // truncated digest
		"ghcr.io/immich-app/immich-server@sha512:" + strings.Repeat("a", 64),            // wrong algorithm
	} {
		err := dryRunApp("m44-image-bad", func(s *orkanov1alpha1.AppSpec) { imageApp(s, ref) })
		if !apierrors.IsInvalid(err) {
			t.Errorf("ref %q should be rejected by the schema, got: %v", ref, err)
		}
	}
}

// The two halves of the union must agree, in both directions.
func TestImageSourceRequiresImageStrategy(t *testing.T) {
	err := dryRunApp("m44-image-wrong-strategy", func(s *orkanov1alpha1.AppSpec) {
		s.Source = orkanov1alpha1.Source{Image: &orkanov1alpha1.ImageSource{Ref: pinnedImage}}
		s.Build = orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyDockerfile}
	})
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "build.strategy Image") {
		t.Fatalf("an image source with a Dockerfile strategy should be rejected, got: %v", err)
	}

	err = dryRunApp("m44-strategy-without-image", func(s *orkanov1alpha1.AppSpec) {
		s.Build = orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyImage}
	})
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "image source") {
		t.Fatalf("the Image strategy without an image source should be rejected, got: %v", err)
	}
}

func TestImageStrategyRejectsBuildMembers(t *testing.T) {
	err := dryRunApp("m44-image-with-dockerfile", func(s *orkanov1alpha1.AppSpec) {
		s.Source = orkanov1alpha1.Source{Image: &orkanov1alpha1.ImageSource{Ref: pinnedImage}}
		s.Build = orkanov1alpha1.BuildStrategy{
			Strategy:   orkanov1alpha1.StrategyImage,
			Dockerfile: &orkanov1alpha1.DockerfileBuild{Path: "Dockerfile"},
		}
	})
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "build members") {
		t.Fatalf("the Image strategy carries no build member, got: %v", err)
	}
}

func TestSourceUnionStillRejectsTwoMembers(t *testing.T) {
	err := dryRunApp("m44-two-sources", func(s *orkanov1alpha1.AppSpec) {
		s.Source.Image = &orkanov1alpha1.ImageSource{Ref: pinnedImage}
	})
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "exactly one of") {
		t.Fatalf("github + image should be rejected by the union rule, got: %v", err)
	}
}

// subPath scopes a checkout; there is no checkout for an image. The rule
// carries an explicit size-zero arm because subPath is a non-pointer string, so
// an explicitly-supplied empty value still reaches the apiserver with has()
// true — the EMPTY-VALUE trap.
func TestImageSourceRejectsSubPath(t *testing.T) {
	err := dryRunApp("m44-image-subpath", func(s *orkanov1alpha1.AppSpec) {
		imageApp(s, pinnedImage)
		s.Source.SubPath = "services/api"
	})
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "subPath does not apply") {
		t.Fatalf("an image source with a subPath should be rejected, got: %v", err)
	}

	if err := dryRunApp("m44-image-empty-subpath", func(s *orkanov1alpha1.AppSpec) {
		imageApp(s, pinnedImage)
		s.Source.SubPath = ""
	}); err != nil {
		t.Fatalf("an explicitly empty subPath must stay admissible, got: %v", err)
	}
}

func TestVolumesAccepted(t *testing.T) {
	size := resource.MustParse("5Gi")
	if err := dryRunApp("m44-volumes-ok", func(s *orkanov1alpha1.AppSpec) {
		s.Volumes = []orkanov1alpha1.AppVolume{{Name: "data", MountPath: "/var/lib/data", Size: &size}}
	}); err != nil {
		t.Fatalf("a well-formed volume should be admitted, got: %v", err)
	}
}

// A ReadWriteOnce claim cannot serve two pods: on CSI the surge pod blocks on
// Multi-Attach forever, and on a node-affine local volume it succeeds and
// writes the same files concurrently.
func TestVolumesCapReplicas(t *testing.T) {
	two := int32(2)
	err := dryRunApp("m44-volumes-replicas", func(s *orkanov1alpha1.AppSpec) {
		s.Volumes = []orkanov1alpha1.AppVolume{{Name: "data", MountPath: "/var/lib/data"}}
		s.Replicas = &two
	})
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "one replica") {
		t.Fatalf("volumes with replicas=2 should be rejected, got: %v", err)
	}

	// replicas: 0 is the stop-the-app idiom and must stay available.
	zero := int32(0)
	if err := dryRunApp("m44-volumes-stopped", func(s *orkanov1alpha1.AppSpec) {
		s.Volumes = []orkanov1alpha1.AppVolume{{Name: "data", MountPath: "/var/lib/data"}}
		s.Replicas = &zero
	}); err != nil {
		t.Fatalf("replicas=0 with volumes must stay admissible, got: %v", err)
	}
}

func TestVolumeMountPathValidated(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		{"traversal", "/var/../etc/passwd", "must not contain '..'"},
		{"root", "/", "at least 2 chars"}, // MinLength catches "/" before the pattern does
		{"relative", "var/lib/data", "should match"},
		{"space", "/var/lib my data", "should match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := dryRunApp("m44-volume-"+tc.name, func(s *orkanov1alpha1.AppSpec) {
				s.Volumes = []orkanov1alpha1.AppVolume{{Name: "data", MountPath: tc.path}}
			})
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("mountPath %q should be rejected with %q, got: %v", tc.path, tc.want, err)
			}
		})
	}
}

// The name derives the claim name, so it must be a DNS-1123 label.
func TestVolumeNameValidated(t *testing.T) {
	err := dryRunApp("m44-volume-badname", func(s *orkanov1alpha1.AppSpec) {
		s.Volumes = []orkanov1alpha1.AppVolume{{Name: "Data_1", MountPath: "/var/lib/data"}}
	})
	if !apierrors.IsInvalid(err) {
		t.Fatalf("an out-of-pattern volume name should be rejected, got: %v", err)
	}
}

func TestPinnedBuildAccepted(t *testing.T) {
	if err := dryRunApp("m44-pin-ok", func(s *orkanov1alpha1.AppSpec) {
		s.PinnedBuild = "app-abc123def456"
	}); err != nil {
		t.Fatalf("a pinned build should be admitted, got: %v", err)
	}
	// Unpinning by patching the field to "" is the natural gesture and must
	// not be rejected by a MinLength.
	if err := dryRunApp("m44-pin-empty", func(s *orkanov1alpha1.AppSpec) {
		s.PinnedBuild = ""
	}); err != nil {
		t.Fatalf("an empty pin must stay admissible, got: %v", err)
	}
}

// ADR-0011: every M4.4 change is a relaxation, so an object written by a v0.1.0
// client — no image member, no volumes, no pin — must still validate unchanged.
func TestPreExistingAppShapesStillValidate(t *testing.T) {
	cases := map[string]func(*orkanov1alpha1.AppSpec){
		"github dockerfile": func(s *orkanov1alpha1.AppSpec) {},
		"github static": func(s *orkanov1alpha1.AppSpec) {
			s.Build = orkanov1alpha1.BuildStrategy{
				Strategy: orkanov1alpha1.StrategyStatic,
				Static:   &orkanov1alpha1.StaticBuild{Dir: "dist"},
			}
		},
		"github nixpacks": func(s *orkanov1alpha1.AppSpec) {
			s.Build = orkanov1alpha1.BuildStrategy{
				Strategy: orkanov1alpha1.StrategyNixpacks,
				Nixpacks: &orkanov1alpha1.NixpacksBuild{},
			}
		},
		"generic git": func(s *orkanov1alpha1.AppSpec) {
			s.Source = orkanov1alpha1.Source{Git: &orkanov1alpha1.GitSource{URL: "https://git.example.com/a/b.git"}}
		},
		"zip upload": func(s *orkanov1alpha1.AppSpec) {
			s.Source = orkanov1alpha1.Source{Upload: &orkanov1alpha1.UploadSource{
				Digest: "sha256:" + strings.Repeat("a", 64),
			}}
		},
		"subpath monorepo": func(s *orkanov1alpha1.AppSpec) {
			s.Source.SubPath = "services/api"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if err := dryRunApp("m44-compat-"+strings.ReplaceAll(name, " ", "-"), mutate); err != nil {
				t.Fatalf("a shape a v0.1.0 client could already store must still validate, got: %v", err)
			}
		})
	}
}
