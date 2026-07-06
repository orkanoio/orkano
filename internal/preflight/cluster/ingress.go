package cluster

import (
	"context"
	"fmt"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/orkanoio/orkano/api/check"
)

const defaultIngressClassAnnotation = "ingressclass.kubernetes.io/is-default-class"

// ingressClassPresentCheck verifies an ingress controller is installed, seen
// through the IngressClass it registers. Critical, by the same reasoning as
// the StorageClass check: routing + TLS are table stakes and a missing
// controller never heals on its own — a green preflight must not precede
// Domains that can never come up. Verdicts, precisely: no IngressClass at all
// is a fail (controllers that predate the IngressClass API can still route
// classless Ingresses, but a controller only detectable by its absence is
// outside the tested claim, which the message says rather than asserting
// nothing routes); classes present without a default is a PASS with the
// action spelled out (Domain-rendered Ingresses leave ingressClassName unset
// today, so either a default class or the ingress.className install value —
// ADR-0019 decision 4 — must route them; that is an install-values choice,
// not a cluster defect).
func ingressClassPresentCheck(opt Options) check.Check {
	return check.Check{
		ID:       IDIngressClassPresent,
		Severity: check.SeverityCritical,
		Summary:  "an ingress controller is installed so Domain routing and TLS can work",
		Remediation: "install an ingress controller that registers an IngressClass (Traefik and ingress-nginx are " +
			"the tested adapters); if one exists without a default class, mark it default or set ingress.className at install",
		Probe: func(ctx context.Context) (check.Result, error) {
			var classes networkingv1.IngressClassList
			if err := opt.Client.List(ctx, &classes); err != nil {
				return check.Result{}, fmt.Errorf("list IngressClasses: %w", err)
			}
			if len(classes.Items) == 0 {
				return check.Result{
					Status: check.StatusFail,
					Message: "no IngressClass exists — no tested ingress controller is detectable, so Domain routing, " +
						"TLS and receiver exposure for GitHub webhooks cannot be relied on (Apps still deploy and build)",
				}, nil
			}

			var names, defaults []string
			for i := range classes.Items {
				names = append(names, classes.Items[i].Name)
				if classes.Items[i].Annotations[defaultIngressClassAnnotation] == "true" {
					defaults = append(defaults, classes.Items[i].Name)
				}
			}
			if len(defaults) == 0 {
				return check.Result{
					Status: check.StatusPass,
					Message: fmt.Sprintf("IngressClasses exist (%s) but none is default — Domain Ingresses name no "+
						"class, so mark one default or set ingress.className at install", strings.Join(names, ", ")),
				}, nil
			}
			return check.Result{
				Status:  check.StatusPass,
				Message: fmt.Sprintf("default IngressClass %s (%d present)", strings.Join(defaults, ", "), len(classes.Items)),
			}, nil
		},
	}
}
