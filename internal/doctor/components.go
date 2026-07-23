package doctor

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/api/check"
)

// IDComponentsReady is PERMANENT — it appears in --json output and CI configs.
const IDComponentsReady = "platform.components-ready"

// componentDeployments and componentStatefulSets are the orkano-system
// workloads Orkano's control plane runs on — the orkano-system subset of
// internal/install.DefaultReadinessTargets(). cert-manager's own Deployments
// live in the cert-manager namespace and are deliberately excluded: on a BYO
// install cert-manager may be pre-existing or relocated, and PKI health is
// already covered by tls.certificate-expiry. TestComponentsMatchReadinessTargets
// cross-checks these lists against DefaultReadinessTargets so the two cannot
// drift.
var (
	componentDeployments  = []string{"orkano-operator", "orkano-receiver", "orkano-registry", "orkano-dashboard"}
	componentStatefulSets = []string{"orkano-postgres"}
)

// componentsReadyCheck verifies the platform control plane is actually running:
// every core Orkano workload in orkano-system has at least one ready replica.
// It answers "is Orkano even up" before the other checks report on how it is
// configured, so it is registered first. A missing or unready workload is a
// definitive Fail (all aggregated into one message, the tls.go convention); a
// read the cluster refuses is a probe error — unknown never counts as hardened.
func componentsReadyCheck(opt Options) check.Check {
	return check.Check{
		ID:       IDComponentsReady,
		Severity: check.SeverityCritical,
		Summary:  "the platform control-plane components are running",
		Remediation: "inspect the workloads with `kubectl -n orkano-system get deploy,statefulset` and the failing pod's " +
			"events and logs; a component stuck unready is usually a missing Secret or an image pull failure — re-run `orkano init` to repair a partial install",
		Probe: func(ctx context.Context) (check.Result, error) {
			var problems []string
			for _, name := range componentDeployments {
				var d appsv1.Deployment
				err := opt.Client.Get(ctx, client.ObjectKey{Namespace: systemNamespace, Name: name}, &d)
				switch {
				case apierrors.IsNotFound(err):
					problems = append(problems, fmt.Sprintf("Deployment %s is missing", name))
				case err != nil:
					return check.Result{}, fmt.Errorf("read Deployment %s/%s: %w", systemNamespace, name, err)
				default:
					if d.Status.ReadyReplicas < 1 {
						problems = append(problems, fmt.Sprintf("Deployment %s has no ready replicas", name))
					}
				}
			}
			for _, name := range componentStatefulSets {
				var sts appsv1.StatefulSet
				err := opt.Client.Get(ctx, client.ObjectKey{Namespace: systemNamespace, Name: name}, &sts)
				switch {
				case apierrors.IsNotFound(err):
					problems = append(problems, fmt.Sprintf("StatefulSet %s is missing", name))
				case err != nil:
					return check.Result{}, fmt.Errorf("read StatefulSet %s/%s: %w", systemNamespace, name, err)
				default:
					if sts.Status.ReadyReplicas < 1 {
						problems = append(problems, fmt.Sprintf("StatefulSet %s has no ready replicas", name))
					}
				}
			}
			if len(problems) > 0 {
				return check.Result{Status: check.StatusFail, Message: strings.Join(problems, "; ")}, nil
			}
			return check.Result{
				Status: check.StatusPass,
				Message: fmt.Sprintf("%d Deployment(s) and %d StatefulSet(s) in %s are ready",
					len(componentDeployments), len(componentStatefulSets), systemNamespace),
			}, nil
		},
	}
}
