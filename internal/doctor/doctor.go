// Package doctor holds the runtime checks `orkano doctor` runs against a live
// cluster — the third consumer of the api/check contract, after the install
// preflight and the onboarding wizard. Checks close over a controller-runtime
// client built from the admin kubeconfig and read the cluster's actual state,
// so they report what is deployed, not what was configured.
package doctor

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/checks"
)

const (
	systemNamespace = "orkano-system"
	appsNamespace   = "orkano-apps"

	// dashboardName is the dashboard Service name rendered by internal/install's
	// dashboard.yaml.tmpl; keep the two in sync.
	dashboardName = "orkano-dashboard"
)

// IDDashboardNotPublic is PERMANENT — it appears in --json output and CI
// configs.
const IDDashboardNotPublic = "exposure.dashboard-not-public"

// Options carries the dependencies the doctor checks close over.
type Options struct {
	// Client reads the cluster. Doctor runs under the admin kubeconfig that
	// `orkano init` produced, so no Orkano RBAC grant is involved.
	Client client.Client
	// MaxSnapshotAge overrides DefaultMaxSnapshotAge when positive — Go
	// surface for a customized snapshot cron; no CLI flag exposes it yet.
	MaxSnapshotAge time.Duration
	// Now is the doctor's clock for age and expiry math; defaults to time.Now.
	// Injected in tests, mirroring preflight.Options.
	Now func() time.Time
	// SkipSecretReads makes secrets.store-health skip the target-Secret
	// existence Get. The dashboard doctor face runs value-blind (INV-03/
	// ADR-0013): its impersonated viewer identity never gains `secrets get`, so
	// it cannot read a Secret to confirm it exists. Skipping is outcome-neutral —
	// a Ready, fresh sync still passes — and the healthy message says existence
	// is verified only by the CLI doctor, so the report never overclaims.
	SkipSecretReads bool
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) maxSnapshotAge() time.Duration {
	if o.MaxSnapshotAge > 0 {
		return o.MaxSnapshotAge
	}
	return DefaultMaxSnapshotAge
}

// Checks returns the doctor's cluster checks for the given options. Components
// first — "is Orkano even running" precedes any report on how it is configured.
func Checks(opt Options) []check.Check {
	return []check.Check{
		componentsReadyCheck(opt),
		dashboardNotPublicCheck(opt),
		certificateExpiryCheck(opt),
		etcdSnapshotAgeCheck(opt),
		networkPolicyEnforcedCheck(opt),
		secretsStoreHealthCheck(opt),
		unsafeFeaturesDisabledCheck(opt),
	}
}

// ReadOnlyChecks is the dashboard doctor face's set: Checks minus
// net.networkpolicy-enforced, the one pod-creating probe. The dashboard runs
// value-blind under the impersonated viewer, which holds no grant to create
// pods, and PRD principle #9 reserves canary-pod probing for the CLI's explicit
// disclosure — so the dashboard reports the read-only subset only. Same order
// as Checks.
func ReadOnlyChecks(opt Options) []check.Check {
	return []check.Check{
		componentsReadyCheck(opt),
		dashboardNotPublicCheck(opt),
		certificateExpiryCheck(opt),
		etcdSnapshotAgeCheck(opt),
		secretsStoreHealthCheck(opt),
		unsafeFeaturesDisabledCheck(opt),
	}
}

// Register adds every doctor cluster check to reg.
func Register(reg *checks.Registry, opt Options) error {
	return registerAll(reg, Checks(opt))
}

// RegisterReadOnly adds the dashboard doctor face's read-only check set to reg.
func RegisterReadOnly(reg *checks.Registry, opt Options) error {
	return registerAll(reg, ReadOnlyChecks(opt))
}

func registerAll(reg *checks.Registry, cs []check.Check) error {
	for _, c := range cs {
		if err := reg.Register(c); err != nil {
			return fmt.Errorf("register %s: %w", c.ID, err)
		}
	}
	return nil
}

// traefikIngressRouteListGVKs are the routing kinds of k3s's bundled ingress
// controller (Traefik v3). On the default substrate a plain Ingress is not the
// only exposure path: an IngressRoute (or a TCP one pointed at the dashboard's
// HTTP port) reaches the internet through the same Traefik entrypoints without
// any networking.k8s.io object existing.
var traefikIngressRouteListGVKs = []schema.GroupVersionKind{
	{Group: "traefik.io", Version: "v1alpha1", Kind: "IngressRouteList"},
	{Group: "traefik.io", Version: "v1alpha1", Kind: "IngressRouteTCPList"},
}

// dashboardNotPublicCheck verifies INV-05: the dashboard is never
// internet-reachable by default. It fails when the dashboard Service has been
// flipped to NodePort/LoadBalancer or given externalIPs, or when any Ingress or
// Traefik IngressRoute in orkano-system routes to it. A missing dashboard
// Service is a skip (nothing to expose), and a read the cluster refuses is a
// probe error — unknown never counts as hardened.
func dashboardNotPublicCheck(opt Options) check.Check {
	return check.Check{
		ID:       IDDashboardNotPublic,
		Severity: check.SeverityCritical,
		Summary:  "dashboard is not exposed outside the cluster (INV-05)",
		Remediation: "revert the orkano-dashboard Service to ClusterIP without externalIPs and delete any Ingress " +
			"or Traefik IngressRoute routing to it; reach the dashboard privately instead (SSH tunnel or kubectl port-forward)",
		Probe: func(ctx context.Context) (check.Result, error) {
			var svc corev1.Service
			err := opt.Client.Get(ctx, client.ObjectKey{Namespace: systemNamespace, Name: dashboardName}, &svc)
			switch {
			case apierrors.IsNotFound(err):
				return check.Result{
					Status:  check.StatusSkip,
					Message: fmt.Sprintf("Service %s/%s not found — dashboard not installed, nothing to expose", systemNamespace, dashboardName),
				}, nil
			case err != nil:
				return check.Result{}, fmt.Errorf("read Service %s/%s: %w", systemNamespace, dashboardName, err)
			}

			if t := svc.Spec.Type; t == corev1.ServiceTypeNodePort || t == corev1.ServiceTypeLoadBalancer {
				return check.Result{
					Status:  check.StatusFail,
					Message: fmt.Sprintf("dashboard Service is type %s — it must stay ClusterIP (INV-05)", t),
				}, nil
			}
			// externalIPs make even a ClusterIP Service reachable on those
			// addresses (kube-proxy programs them), with no type flip and no
			// Ingress object to notice.
			if len(svc.Spec.ExternalIPs) > 0 {
				return check.Result{
					Status:  check.StatusFail,
					Message: fmt.Sprintf("dashboard Service carries externalIPs %v — it is reachable outside the cluster (INV-05)", svc.Spec.ExternalIPs),
				}, nil
			}

			var ingresses networkingv1.IngressList
			if err := opt.Client.List(ctx, &ingresses, client.InNamespace(systemNamespace)); err != nil {
				return check.Result{}, fmt.Errorf("list Ingresses in %s: %w", systemNamespace, err)
			}
			for i := range ingresses.Items {
				if ingressRoutesToService(&ingresses.Items[i], dashboardName) {
					return check.Result{
						Status:  check.StatusFail,
						Message: fmt.Sprintf("Ingress %s routes to the dashboard Service (INV-05)", ingresses.Items[i].Name),
					}, nil
				}
			}

			for _, gvk := range traefikIngressRouteListGVKs {
				routes := &unstructured.UnstructuredList{}
				routes.SetGroupVersionKind(gvk)
				err := opt.Client.List(ctx, routes, client.InNamespace(systemNamespace))
				switch {
				case meta.IsNoMatchError(err) || apierrors.IsNotFound(err):
					// The Traefik CRD is not installed, so no such route can
					// exist — a definitive absence, not an unknown.
					continue
				case err != nil:
					return check.Result{}, fmt.Errorf("list %s in %s: %w", gvk.Kind, systemNamespace, err)
				}
				for i := range routes.Items {
					if ingressRouteRoutesToService(&routes.Items[i], dashboardName) {
						return check.Result{
							Status:  check.StatusFail,
							Message: fmt.Sprintf("Traefik %s %s routes to the dashboard Service (INV-05)", routes.Items[i].GetKind(), routes.Items[i].GetName()),
						}, nil
					}
				}
			}

			return check.Result{
				Status:  check.StatusPass,
				Message: "dashboard Service is ClusterIP without externalIPs and no Ingress or Traefik IngressRoute routes to it",
			}, nil
		},
	}
}

// ingressRouteRoutesToService walks a Traefik IngressRoute/IngressRouteTCP's
// spec.routes[].services[] — both kinds carry direct Service references there.
// Indirection through a TraefikService is out of scope (nothing in Orkano
// creates one); the direct reference is the realistic misconfiguration.
func ingressRouteRoutesToService(route *unstructured.Unstructured, service string) bool {
	routes, _, _ := unstructured.NestedSlice(route.Object, "spec", "routes")
	for _, r := range routes {
		rm, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		services, _, _ := unstructured.NestedSlice(rm, "services")
		for _, s := range services {
			sm, ok := s.(map[string]interface{})
			if !ok {
				continue
			}
			if name, _, _ := unstructured.NestedString(sm, "name"); name == service {
				return true
			}
		}
	}
	return false
}

func ingressRoutesToService(ing *networkingv1.Ingress, service string) bool {
	if b := ing.Spec.DefaultBackend; b != nil && b.Service != nil && b.Service.Name == service {
		return true
	}
	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, p := range rule.HTTP.Paths {
			if p.Backend.Service != nil && p.Backend.Service.Name == service {
				return true
			}
		}
	}
	return false
}
