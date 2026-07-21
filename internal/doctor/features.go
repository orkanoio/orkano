package doctor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/features"
)

const IDUnsafeFeaturesDisabled = "features.unsafe-disabled"

const unsafeFeaturesEnv = "ORKANO_UNSAFE_FEATURES"

func unsafeFeaturesDisabledCheck(opt Options) check.Check {
	return check.Check{
		ID:          IDUnsafeFeaturesDisabled,
		Severity:    check.SeverityWarning,
		Summary:     "default-off unsafe source and build features remain disabled",
		Remediation: "move Apps back to GitHub plus Dockerfile/Static, then re-run `orkano init` without --enable-unsafe-feature (or set Helm features.unsafe to []) so operator and dashboard roll together",
		Probe: func(ctx context.Context) (check.Result, error) {
			values := make(map[string]string, 2)
			missing := 0
			for _, name := range []string{"orkano-operator", "orkano-dashboard"} {
				var deployment appsv1.Deployment
				err := opt.Client.Get(ctx, client.ObjectKey{Namespace: systemNamespace, Name: name}, &deployment)
				switch {
				case apierrors.IsNotFound(err):
					missing++
					continue
				case err != nil:
					return check.Result{}, fmt.Errorf("read Deployment %s/%s: %w", systemNamespace, name, err)
				}
				raw, err := deploymentEnv(&deployment, name, unsafeFeaturesEnv)
				if err != nil {
					return check.Result{}, err
				}
				set, err := features.ParseCSV(raw)
				if err != nil {
					return check.Result{}, fmt.Errorf("deployment %s has invalid %s: %w", name, unsafeFeaturesEnv, err)
				}
				values[name] = set.CSV()
			}
			if missing == 2 {
				return check.Result{Status: check.StatusSkip, Message: "operator and dashboard Deployments are not installed"}, nil
			}
			if missing != 0 {
				return check.Result{}, errors.New("only one of the operator and dashboard Deployments exists; unsafe-feature policy cannot be compared")
			}
			operator, dashboard := values["orkano-operator"], values["orkano-dashboard"]
			if operator != dashboard {
				return check.Result{Status: check.StatusFail, Message: fmt.Sprintf("unsafe-feature policy drift: operator=%q dashboard=%q", operator, dashboard)}, nil
			}
			if operator != "" {
				return check.Result{Status: check.StatusFail, Message: "unsafe features enabled: " + strings.ReplaceAll(operator, ",", ", ")}, nil
			}
			return check.Result{Status: check.StatusPass, Message: "operator and dashboard have no unsafe features enabled"}, nil
		},
	}
}

func deploymentEnv(deployment *appsv1.Deployment, containerName, envName string) (string, error) {
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name != strings.TrimPrefix(containerName, "orkano-") && container.Name != containerName {
			continue
		}
		for _, env := range container.Env {
			if env.Name == envName {
				if env.ValueFrom != nil {
					return "", fmt.Errorf("deployment %s sources %s indirectly; policy must be an explicit pod-template value", deployment.Name, envName)
				}
				return env.Value, nil
			}
		}
		return "", fmt.Errorf("deployment %s container %s has no %s", deployment.Name, container.Name, envName)
	}
	return "", fmt.Errorf("deployment %s has no %s container", deployment.Name, containerName)
}
