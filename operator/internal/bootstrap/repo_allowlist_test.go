package bootstrap

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/orkanoio/orkano/internal/repoallowlist"
)

func TestSeedRepoAllowlistCreatesCanonicalConfigMap(t *testing.T) {
	cs := fake.NewClientset()
	created, err := SeedRepoAllowlist(context.Background(), cs.CoreV1().ConfigMaps(ns), []string{
		"OrkanoIO/Orkano",
		"acme/widgets",
		"acme/widgets",
	})
	if err != nil {
		t.Fatalf("SeedRepoAllowlist: %v", err)
	}
	if !created {
		t.Fatal("expected the missing ConfigMap to be created")
	}
	got, err := cs.CoreV1().ConfigMaps(ns).Get(context.Background(), repoallowlist.ConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get seeded ConfigMap: %v", err)
	}
	if want := "acme/widgets\norkanoio/orkano\n"; got.Data[repoallowlist.DataKey] != want {
		t.Errorf("repositories = %q, want %q", got.Data[repoallowlist.DataKey], want)
	}
}

func TestSeedRepoAllowlistPreservesExistingWithoutReadingOrMutating(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: repoallowlist.ConfigMapName, Namespace: ns},
		Data:       map[string]string{repoallowlist.DataKey: "keep/existing\n"},
	}
	cs := fake.NewClientset(existing)
	seeding := true
	cs.PrependReactor("*", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if seeding && action.GetVerb() != "create" {
			t.Errorf("allowlist seeder issued %s; create-only preserves runtime Settings edits", action.GetVerb())
		}
		return false, nil, nil
	})
	created, err := SeedRepoAllowlist(context.Background(), cs.CoreV1().ConfigMaps(ns), []string{"replace/me"})
	seeding = false
	if err != nil {
		t.Fatalf("SeedRepoAllowlist: %v", err)
	}
	if created {
		t.Fatal("existing ConfigMap must be preserved")
	}
	got, err := cs.CoreV1().ConfigMaps(ns).Get(context.Background(), repoallowlist.ConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get preserved ConfigMap: %v", err)
	}
	if got.Data[repoallowlist.DataKey] != "keep/existing\n" {
		t.Errorf("existing allowlist was overwritten: %q", got.Data[repoallowlist.DataKey])
	}
}

func TestSeedRepoAllowlistRejectsInvalidRepositoryBeforeCreate(t *testing.T) {
	cs := fake.NewClientset()
	if _, err := SeedRepoAllowlist(context.Background(), cs.CoreV1().ConfigMaps(ns), []string{"owner-only"}); err == nil {
		t.Fatal("expected invalid repository to fail")
	}
	if actions := cs.Actions(); len(actions) != 0 {
		t.Errorf("invalid input issued API actions: %+v", actions)
	}
}
