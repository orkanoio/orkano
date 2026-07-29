package bootstrap

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/orkanoio/orkano/internal/repoallowlist"
)

// SeedRepoAllowlist creates the runtime repository allowlist once. An existing
// ConfigMap is preserved byte-for-byte so a Helm upgrade can never reset edits
// made through Setup or Settings.
func SeedRepoAllowlist(ctx context.Context, configMaps typedcorev1.ConfigMapInterface, repos []string) (bool, error) {
	data, err := repoallowlist.Format(repos)
	if err != nil {
		return false, err
	}
	obj := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: repoallowlist.ConfigMapName},
		Data:       map[string]string{repoallowlist.DataKey: data},
	}
	switch _, err := configMaps.Create(ctx, obj, metav1.CreateOptions{}); {
	case err == nil:
		return true, nil
	case apierrors.IsAlreadyExists(err):
		return false, nil
	default:
		return false, fmt.Errorf("create configmap %s: %w", repoallowlist.ConfigMapName, err)
	}
}
