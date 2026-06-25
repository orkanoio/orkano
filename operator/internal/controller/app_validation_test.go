// Apiserver-level schema validation for the shared BuildStrategy, proven
// against envtest's real CEL + OpenAPI pattern engine (no reconciler runs —
// a server-side dry-run create exercises admission without persisting an
// object). This is the CI-runnable guard for build.dockerfile.path's no-".."
// CEL + character pattern; the kind-based validate-examples.sh covers the
// same rule via hack/testdata/invalid/app-bad-dockerfile-path.yaml.
package controller

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

func dryRunDockerfilePath(name, path string) error {
	app := &orkanov1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace},
		Spec: orkanov1alpha1.AppSpec{
			Source: orkanov1alpha1.Source{
				GitHub: orkanov1alpha1.GitHubSource{Repo: "alice/app"},
			},
			Build: orkanov1alpha1.BuildStrategy{
				Strategy:   orkanov1alpha1.StrategyDockerfile,
				Dockerfile: &orkanov1alpha1.DockerfileBuild{Path: path},
			},
		},
	}
	return k8sClient.Create(context.Background(), app, client.DryRunAll)
}

func TestDockerfilePathRejectsTraversal(t *testing.T) {
	// "../../etc/passwd" is in-pattern (dots and slashes are allowed) but
	// trips the no-".." CEL, isolating that rule.
	err := dryRunDockerfilePath("dockerfile-path-traversal", "../../etc/passwd")
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "path must not contain '..'") {
		t.Fatalf("expected the no-'..' CEL rule to reject a traversal path, got: %v", err)
	}
}

func TestDockerfilePathRejectsEmbeddedDotDot(t *testing.T) {
	// The rule is broader than slash-traversal: an embedded ".." with no slash
	// is in-pattern but still rejected, matching source.subPath/static.dir.
	err := dryRunDockerfilePath("dockerfile-path-embedded-dotdot", "Dockerfile..bak")
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "path must not contain '..'") {
		t.Fatalf("expected the no-'..' CEL rule to reject an embedded '..', got: %v", err)
	}
}

func TestDockerfilePathRejectsBadPattern(t *testing.T) {
	// A space is outside ^[A-Za-z0-9_./-]+$ and contains no "..", isolating
	// the pattern; the apiserver phrases a pattern violation as "should match".
	err := dryRunDockerfilePath("dockerfile-path-space", "deploy/prod Dockerfile")
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "should match") {
		t.Fatalf("expected the pattern to reject an out-of-pattern path, got: %v", err)
	}
}

func TestDockerfilePathAccepted(t *testing.T) {
	// Example 05's non-default path: "/" and "." are in-pattern, no "..".
	if err := dryRunDockerfilePath("dockerfile-path-ok", "deploy/prod.Dockerfile"); err != nil {
		t.Fatalf("expected a valid non-default Dockerfile path to be accepted, got: %v", err)
	}
}
