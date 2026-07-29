package install

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/orkanoio/orkano/internal/repoallowlist"
)

func TestEnsureRepoAllowlistCreatesCanonicalConfigMapOnce(t *testing.T) {
	n := newFakeNode()
	created, err := ensureRepoAllowlist(context.Background(), newNode(n, false, nil), []string{
		"OrkanoIO/Orkano",
		"acme/widgets",
		"acme/widgets",
	})
	if err != nil {
		t.Fatalf("ensureRepoAllowlist: %v", err)
	}
	if !created {
		t.Fatal("expected a missing ConfigMap to be created")
	}

	var got corev1.ConfigMap
	if err := yaml.Unmarshal([]byte(n.configMaps[repoallowlist.ConfigMapName]), &got); err != nil {
		t.Fatalf("parse seeded ConfigMap: %v", err)
	}
	if got.Namespace != repoallowlist.Namespace {
		t.Errorf("namespace = %q, want %q", got.Namespace, repoallowlist.Namespace)
	}
	if want := "acme/widgets\norkanoio/orkano\n"; got.Data[repoallowlist.DataKey] != want {
		t.Errorf("repositories = %q, want %q", got.Data[repoallowlist.DataKey], want)
	}

	before := n.configMaps[repoallowlist.ConfigMapName]
	start := len(n.cmds)
	created, err = ensureRepoAllowlist(context.Background(), newNode(n, false, nil), []string{"replace/me"})
	if err != nil {
		t.Fatalf("second ensureRepoAllowlist: %v", err)
	}
	if created {
		t.Fatal("an existing ConfigMap must be preserved")
	}
	if n.configMaps[repoallowlist.ConfigMapName] != before {
		t.Fatal("installer re-run overwrote the live allowlist")
	}
	secondRun := n.cmds[start:]
	if len(secondRun) != 2 ||
		!strings.Contains(secondRun[0], "kubectl create -f -") ||
		!strings.Contains(secondRun[1], "get configmap "+repoallowlist.ConfigMapName) {
		t.Fatalf("existing-object race must be resolved by atomic create then exact get, got %v", secondRun)
	}
}

func TestEnsureRepoAllowlistRejectsInvalidRepositoryBeforeAPIUse(t *testing.T) {
	n := newFakeNode()
	if _, err := ensureRepoAllowlist(context.Background(), newNode(n, false, nil), []string{"owner-only"}); err == nil {
		t.Fatal("expected invalid repository to fail")
	}
	if len(n.cmds) != 0 {
		t.Errorf("invalid input issued node commands: %v", n.cmds)
	}
}

func TestApplySeedsRepoAllowlistWithVersionedComponents(t *testing.T) {
	n := newFakeNode()
	if _, err := Apply(context.Background(), n, Config{
		Version:       "2.0.0",
		RepoAllowlist: []string{"acme/widgets"},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, ok := n.configMaps[repoallowlist.ConfigMapName]; !ok {
		t.Fatal("versioned component install did not seed the allowlist ConfigMap")
	}
}

func TestApplyMigratesLegacyReceiverRepoAllowlistBeforeManifestRewrite(t *testing.T) {
	n := newFakeNode()
	n.legacyReceiverDeployment = legacyReceiverJSON(t, "OrkanoIO/Orkano, acme/widgets,acme/widgets")

	if _, err := Apply(context.Background(), n, Config{Version: "2.0.0"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var got corev1.ConfigMap
	if err := yaml.Unmarshal([]byte(n.configMaps[repoallowlist.ConfigMapName]), &got); err != nil {
		t.Fatalf("parse migrated ConfigMap: %v", err)
	}
	if want := "acme/widgets\norkanoio/orkano\n"; got.Data[repoallowlist.DataKey] != want {
		t.Errorf("repositories = %q, want migrated %q", got.Data[repoallowlist.DataKey], want)
	}

	readIdx := cmdIndex(n.cmds, func(cmd string) bool {
		return strings.Contains(cmd, "get deployment "+receiverDeploymentName)
	})
	seedIdx := cmdIndex(n.cmds, func(cmd string) bool {
		return strings.Contains(cmd, "kubectl create -f -")
	})
	writeIdx := cmdIndex(n.cmds, func(cmd string) bool {
		return strings.Contains(cmd, "components-receiver.yaml.tmp") &&
			strings.Contains(cmd, "| base64 -d |")
	})
	if readIdx < 0 || seedIdx < 0 || writeIdx < 0 || readIdx >= seedIdx || seedIdx >= writeIdx {
		t.Fatalf("legacy policy must be read and persisted before receiver manifest write; read=%d seed=%d write=%d",
			readIdx, seedIdx, writeIdx)
	}
}

func TestApplyExplicitRepoAllowlistOverridesLegacyReceiver(t *testing.T) {
	n := newFakeNode()
	n.legacyReceiverDeployment = legacyReceiverJSON(t, "old/repository")

	if _, err := Apply(context.Background(), n, Config{
		Version:       "2.0.0",
		RepoAllowlist: []string{"new/repository"},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if hasCmd(n.cmds, func(cmd string) bool {
		return strings.Contains(cmd, "get deployment "+receiverDeploymentName)
	}) {
		t.Fatal("explicit repository flags must bypass legacy discovery")
	}

	var got corev1.ConfigMap
	if err := yaml.Unmarshal([]byte(n.configMaps[repoallowlist.ConfigMapName]), &got); err != nil {
		t.Fatalf("parse ConfigMap: %v", err)
	}
	if want := "new/repository\n"; got.Data[repoallowlist.DataKey] != want {
		t.Errorf("repositories = %q, want explicit seed %q", got.Data[repoallowlist.DataKey], want)
	}
}

func TestApplyLegacyReceiverRepoAllowlistFailuresAreFailClosed(t *testing.T) {
	tests := map[string]struct {
		deployment string
		failure    string
		runErr     error
		want       string
	}{
		"invalid repository": {
			deployment: legacyReceiverJSON(t, "valid/repository,owner-only"),
			want:       "invalid repository",
		},
		"malformed Deployment JSON": {
			deployment: `{"metadata":`,
			want:       "parse legacy receiver Deployment",
		},
		"API failure is not NotFound": {
			failure: "Error from server (Forbidden)",
			want:    "Forbidden",
		},
		"transport failure": {
			runErr: errors.New("connection reset"),
			want:   "connection reset",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			n := newFakeNode()
			n.legacyReceiverDeployment = tc.deployment
			n.receiverDeploymentFailure = tc.failure
			if tc.runErr != nil {
				n.runErr = map[string]error{"get deployment " + receiverDeploymentName: tc.runErr}
			}

			_, err := Apply(context.Background(), n, Config{Version: "2.0.0"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Apply error = %v, want containing %q", err, tc.want)
			}
			if _, ok := n.configMaps[repoallowlist.ConfigMapName]; ok {
				t.Fatal("failed legacy discovery must not seed a ConfigMap")
			}
			if hasCmd(n.cmds, func(cmd string) bool {
				return strings.Contains(cmd, "components-receiver.yaml.tmp") &&
					strings.Contains(cmd, "| base64 -d |")
			}) {
				t.Fatal("failed legacy discovery must stop before the receiver manifest rewrite")
			}
		})
	}
}

func TestApplyMissingLegacyReceiverSeedsDenyAllPolicy(t *testing.T) {
	n := newFakeNode()
	if _, err := Apply(context.Background(), n, Config{Version: "2.0.0"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !hasCmd(n.cmds, func(cmd string) bool {
		return strings.Contains(cmd, "get deployment "+receiverDeploymentName) &&
			strings.Contains(cmd, ignoreNotFoundKubectlOption)
	}) {
		t.Fatal("legacy lookup must use kubectl's exact-resource NotFound handling")
	}

	var got corev1.ConfigMap
	if err := yaml.Unmarshal([]byte(n.configMaps[repoallowlist.ConfigMapName]), &got); err != nil {
		t.Fatalf("parse ConfigMap: %v", err)
	}
	if got.Data[repoallowlist.DataKey] != "" {
		t.Errorf("missing legacy receiver should seed deny-all, got %q", got.Data[repoallowlist.DataKey])
	}
}

func TestEnsureRepoAllowlistDoesNotSwallowFailedCreate(t *testing.T) {
	n := newFakeNode()
	n.failConfigMap = repoallowlist.ConfigMapName

	created, err := ensureRepoAllowlist(context.Background(), newNode(n, false, nil), []string{"acme/widgets"})
	if err == nil || !strings.Contains(err.Error(), "create refused") {
		t.Fatalf("ensureRepoAllowlist error = %v, want create failure", err)
	}
	if created {
		t.Fatal("failed create cannot report a change")
	}
}

func TestMaximumRepoAllowlistFitsSeedTransports(t *testing.T) {
	repositories := make([]string, repoallowlist.MaxRepositories)
	for i := range repositories {
		repositories[i] = "owner/" + strings.Repeat("a", repoallowlist.MaxRepositoryLength-len("owner/")-4) + fmt.Sprintf("-%03d", i)
	}
	data, err := repoallowlist.Format(repositories)
	if err != nil {
		t.Fatalf("Format maximum allowlist: %v", err)
	}
	if len(data) >= 128*1024 {
		t.Fatalf("formatted allowlist is %d bytes; Helm's single environment value must stay below Linux MAX_ARG_STRLEN", len(data))
	}
	encoded := base64.StdEncoding.EncodeToString(repoAllowlistManifest(data))
	if len(encoded) > maxInlineBase64 {
		t.Fatalf("encoded ConfigMap is %d bytes; SSH seed command limit is %d", len(encoded), maxInlineBase64)
	}
}

func legacyReceiverJSON(t *testing.T, repositories string) string {
	t.Helper()
	deployment := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      receiverDeploymentName,
			Namespace: systemNS,
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "receiver",
						Env: []corev1.EnvVar{{
							Name:  legacyRepoAllowlistEnv,
							Value: repositories,
						}},
					}},
				},
			},
		},
	}
	data, err := json.Marshal(deployment)
	if err != nil {
		t.Fatalf("marshal legacy receiver Deployment: %v", err)
	}
	return string(data)
}
