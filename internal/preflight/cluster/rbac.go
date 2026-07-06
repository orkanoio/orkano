package cluster

import (
	"context"
	"fmt"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"

	"github.com/orkanoio/orkano/api/check"
)

// accessTuple is one (resource, namespace) the install must be allowed to
// create; an empty namespace means cluster-scoped.
type accessTuple struct {
	group     string
	resource  string
	namespace string
}

func (t accessTuple) String() string {
	name := t.resource
	if t.group != "" {
		name = t.resource + "." + t.group
	}
	if t.namespace == "" {
		return name + " (cluster-scoped)"
	}
	return name + " in " + t.namespace
}

// installAccessSet is the create-verb walk over what the chart writes. The
// namespaced half is deliberately the UNION of the chart's namespaced kinds,
// probed in every Orkano namespace (one list, not a per-namespace matrix):
// the failure mode this check exists to catch is under-privilege discovered
// mid-install, and an installer identity narrow enough to be refused by the
// union is not one anyone points a chart at. Two approximations, named
// honestly: create-only (an upgrade additionally needs update/patch on the
// same set), and no transitive-escalation modeling (RBAC escalation
// prevention means creating Orkano's own Roles requires holding their grants
// — impersonate included — or the escalate verb; a cluster-admin-ish
// installer has both). M4.2's chart drift guard should assert this list
// against the chart's actual contents.
func installAccessSet() []accessTuple {
	clusterScoped := []accessTuple{
		{group: "apiextensions.k8s.io", resource: "customresourcedefinitions"},
		{resource: "namespaces"},
		{group: "rbac.authorization.k8s.io", resource: "clusterroles"},
		{group: "rbac.authorization.k8s.io", resource: "clusterrolebindings"},
		// The vendored cert-manager ships webhook configurations; the
		// orkano-platform issuer is a ClusterIssuer either way.
		{group: "admissionregistration.k8s.io", resource: "validatingwebhookconfigurations"},
		{group: "admissionregistration.k8s.io", resource: "mutatingwebhookconfigurations"},
		{group: "cert-manager.io", resource: "clusterissuers"},
	}

	// SelfSubjectAccessReview evaluates pure policy, so probing a group whose
	// CRD is not yet installed (cert-manager.io) or a namespace that does not
	// exist yet is well-defined.
	namespaced := []accessTuple{
		{resource: "serviceaccounts"},
		{group: "rbac.authorization.k8s.io", resource: "roles"},
		{group: "rbac.authorization.k8s.io", resource: "rolebindings"},
		{group: "apps", resource: "deployments"},
		{group: "apps", resource: "statefulsets"},
		// The opt-in node-prep component (ADR-0019 decision 3) — part of the
		// union like cert-manager's kinds, even though both are values-gated.
		{group: "apps", resource: "daemonsets"},
		{resource: "services"},
		{resource: "configmaps"},
		// Helm stores release state as Secrets in the release namespace, and
		// the bootstrap Job's Role must be grantable (see the escalation note
		// above).
		{resource: "secrets"},
		{group: "batch", resource: "jobs"},
		{resource: "persistentvolumeclaims"},
		{group: "networking.k8s.io", resource: "networkpolicies"},
		{group: "networking.k8s.io", resource: "ingresses"},
		{group: "cert-manager.io", resource: "issuers"},
		{group: "cert-manager.io", resource: "certificates"},
	}

	tuples := clusterScoped
	for _, ns := range []string{systemNamespace, appsNamespace, buildNamespace, certManagerNamespace} {
		for _, t := range namespaced {
			t.namespace = ns
			tuples = append(tuples, t)
		}
	}
	return tuples
}

// rbacSufficientCheck walks installAccessSet through SelfSubjectAccessReview:
// can the kubeconfig identity — the one that will run the install — create
// everything the chart writes? Aggregates every denial into one message (the
// doctor convention: the full picture, no whack-a-mole), capped so a
// zero-privilege identity does not print sixty lines.
func rbacSufficientCheck(opt Options) check.Check {
	return check.Check{
		ID:       IDRBACSufficient,
		Severity: check.SeverityCritical,
		Summary:  "the kubeconfig identity can create everything the install writes",
		Remediation: "install with an identity holding create on the denied resources (cluster-admin, or an " +
			"equivalently broad role — RBAC escalation prevention also requires the grants Orkano's own Roles contain)",
		Probe: func(ctx context.Context) (check.Result, error) {
			tuples := installAccessSet()
			var denied []string
			for _, tp := range tuples {
				ssar := &authorizationv1.SelfSubjectAccessReview{
					Spec: authorizationv1.SelfSubjectAccessReviewSpec{
						ResourceAttributes: &authorizationv1.ResourceAttributes{
							Group:     tp.group,
							Resource:  tp.resource,
							Verb:      "create",
							Namespace: tp.namespace,
						},
					},
				}
				if err := opt.Client.Create(ctx, ssar); err != nil {
					return check.Result{}, fmt.Errorf("SelfSubjectAccessReview for create %s: %w", tp, err)
				}
				if !ssar.Status.Allowed {
					denied = append(denied, tp.String())
				}
			}

			if len(denied) > 0 {
				const maxListed = 8
				listed := denied
				var more string
				if len(listed) > maxListed {
					listed = listed[:maxListed]
					more = fmt.Sprintf(" (+%d more)", len(denied)-maxListed)
				}
				return check.Result{
					Status: check.StatusFail,
					Message: fmt.Sprintf("the kubeconfig identity is denied create on %d of %d install resources: %s%s",
						len(denied), len(tuples), strings.Join(listed, ", "), more),
				}, nil
			}
			return check.Result{
				Status:  check.StatusPass,
				Message: fmt.Sprintf("the kubeconfig identity may create all %d resource kinds the install writes", len(tuples)),
			}, nil
		},
	}
}
