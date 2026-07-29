package install

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/orkanoio/orkano/internal/repoallowlist"
)

const (
	legacyRepoAllowlistEnv      = "ORKANO_REPO_ALLOWLIST"
	receiverDeploymentName      = "orkano-receiver"
	ignoreNotFoundKubectlOption = "--ignore-not-found=true"
)

// legacyReceiverRepoAllowlist reads the startup-only policy used before the
// runtime ConfigMap existed. Apply calls this before replacing the receiver
// manifest, otherwise Kubernetes could roll away the only copy of that policy.
func legacyReceiverRepoAllowlist(ctx context.Context, n *node) ([]string, bool, error) {
	cmd := fmt.Sprintf(
		"%s%s kubectl -n %s get deployment %s %s -o json",
		n.sudo,
		k3sBin,
		systemNS,
		receiverDeploymentName,
		ignoreNotFoundKubectlOption,
	)
	got, err := n.r.Run(ctx, cmd)
	if err != nil {
		return nil, false, fmt.Errorf("install: read legacy receiver repository allowlist: %w", err)
	}
	if got.ExitStatus != 0 {
		return nil, false, fmt.Errorf(
			"install: read legacy receiver repository allowlist exited %d: %s",
			got.ExitStatus,
			firstLine(got.Stderr),
		)
	}
	if strings.TrimSpace(got.Stdout) == "" {
		return nil, false, nil
	}

	var deployment appsv1.Deployment
	if err := json.Unmarshal([]byte(got.Stdout), &deployment); err != nil {
		return nil, false, fmt.Errorf("install: parse legacy receiver Deployment: %w", err)
	}
	if deployment.Name != receiverDeploymentName {
		return nil, false, fmt.Errorf(
			"install: legacy receiver Deployment has name %q, want %q",
			deployment.Name,
			receiverDeploymentName,
		)
	}

	var raw *string
	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, variable := range container.Env {
			if variable.Name != legacyRepoAllowlistEnv {
				continue
			}
			if raw != nil {
				return nil, false, fmt.Errorf("install: legacy receiver Deployment has duplicate %s entries", legacyRepoAllowlistEnv)
			}
			if variable.ValueFrom != nil {
				return nil, false, fmt.Errorf("install: legacy receiver Deployment %s must be a literal value", legacyRepoAllowlistEnv)
			}
			value := variable.Value
			raw = &value
		}
	}
	if raw == nil {
		return nil, true, nil
	}

	repositories, err := repoallowlist.Normalize(strings.Split(*raw, ","))
	if err != nil {
		return nil, false, fmt.Errorf("install: legacy receiver repository allowlist: %w", err)
	}
	return repositories, true, nil
}

// ensureRepoAllowlist seeds the runtime allowlist exactly once. Later installer
// runs preserve the live ConfigMap so Setup and Settings remain the source of
// truth after onboarding.
func ensureRepoAllowlist(ctx context.Context, n *node, repositories []string) (bool, error) {
	data, err := repoallowlist.Format(repositories)
	if err != nil {
		return false, fmt.Errorf("install: repository allowlist: %w", err)
	}
	created, err := n.createConfigMapIfAbsent(ctx, repoallowlist.ConfigMapName, repoAllowlistManifest(data))
	if err != nil {
		return false, err
	}
	if created {
		n.logf("created configmap %s", repoallowlist.ConfigMapName)
	}
	return created, nil
}

func (n *node) createConfigMapIfAbsent(ctx context.Context, name string, manifest []byte) (bool, error) {
	enc := base64.StdEncoding.EncodeToString(manifest)
	cmd := fmt.Sprintf("printf %%s '%s' | base64 -d | %s%s kubectl create -f -", enc, n.sudo, k3sBin)
	created, err := n.r.Run(ctx, cmd)
	if err != nil {
		return false, fmt.Errorf("install: create configmap %s: %w", name, err)
	}
	if created.ExitStatus == 0 {
		return true, nil
	}

	// Create is the atomic generate-once primitive. If it lost a race or the
	// object already existed, a name-pinned Get confirms there is nothing left
	// to do. No installer path can update Settings-owned data.
	got, getErr := n.r.Run(ctx, fmt.Sprintf("%s%s kubectl -n %s get configmap %s -o name", n.sudo, k3sBin, systemNS, name))
	if getErr != nil {
		return false, fmt.Errorf("install: confirm configmap %s after create failed: %w", name, getErr)
	}
	if got.ExitStatus == 0 {
		return false, nil
	}
	return false, fmt.Errorf("install: create configmap %s exited %d: %s", name, created.ExitStatus, firstLine(created.Stderr))
}

func repoAllowlistManifest(data string) []byte {
	return []byte(fmt.Sprintf(
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: %s\ndata:\n  %s: %q\n",
		repoallowlist.ConfigMapName,
		repoallowlist.Namespace,
		repoallowlist.DataKey,
		data,
	))
}
