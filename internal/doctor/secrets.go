package doctor

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/api/check"
)

// IDSecretsStoreHealth is PERMANENT — it appears in --json output and CI
// configs.
const IDSecretsStoreHealth = "secrets.store-health"

// esoRefreshGraceFactor is how many refresh intervals may pass since the last
// successful sync before the sync counts as stale: 2× tolerates one missed
// cycle (store briefly unreachable, ESO restart) without flapping, while a
// second consecutive miss surfaces.
const esoRefreshGraceFactor = 2

// esoDefaultRefreshInterval mirrors the dashboard's default when an
// ExternalSecret carries no refreshInterval of its own.
const esoDefaultRefreshInterval = time.Hour

var (
	esoSecretStoreListGVK    = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "SecretStoreList"}
	esoExternalSecretListGVK = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecretList"}
)

// secretsStoreHealthCheck reads the RESULT of ESO's own live validation
// (ADR-0018 decision 6): every SecretStore and ExternalSecret in orkano-apps
// must be Ready, every sync fresh (refreshTime within the grace window), and
// every target Secret actually present. It deliberately does not re-probe the
// vault with a second credential path. ESO absent (no CRDs) is a skip — the
// install never opted in — as is an ESO with nothing configured.
func secretsStoreHealthCheck(opt Options) check.Check {
	return check.Check{
		ID:       IDSecretsStoreHealth,
		Severity: check.SeverityWarning,
		Summary:  "external secret stores and syncs are healthy (ADR-0018)",
		Remediation: "inspect `kubectl get secretstores,externalsecrets -n orkano-apps` — a store that is not Ready " +
			"usually means a bad or expired credential (rotate it from the dashboard) or an unreachable server; " +
			"a stale or missing sync means ESO cannot refresh: `kubectl describe externalsecret <name> -n orkano-apps`",
		Probe: func(ctx context.Context) (check.Result, error) {
			stores := &unstructured.UnstructuredList{}
			stores.SetGroupVersionKind(esoSecretStoreListGVK)
			err := opt.Client.List(ctx, stores, client.InNamespace(appsNamespace))
			switch {
			case meta.IsNoMatchError(err):
				// The CRD is absent: the install never opted in. A definitive
				// inapplicability, not an unknown — the vendored set always
				// carries both CRDs, so one missing means both are.
				return check.Result{
					Status:  check.StatusSkip,
					Message: "External Secrets Operator not installed — enable external vaults with `orkano init --secrets-vault`",
				}, nil
			case err != nil:
				return check.Result{}, fmt.Errorf("list SecretStores in %s: %w", appsNamespace, err)
			}
			syncs := &unstructured.UnstructuredList{}
			syncs.SetGroupVersionKind(esoExternalSecretListGVK)
			if err := opt.Client.List(ctx, syncs, client.InNamespace(appsNamespace)); err != nil {
				if meta.IsNoMatchError(err) {
					return check.Result{
						Status:  check.StatusSkip,
						Message: "External Secrets Operator not installed — enable external vaults with `orkano init --secrets-vault`",
					}, nil
				}
				return check.Result{}, fmt.Errorf("list ExternalSecrets in %s: %w", appsNamespace, err)
			}

			if len(stores.Items)+len(syncs.Items) == 0 {
				return check.Result{
					Status:  check.StatusSkip,
					Message: "External Secrets Operator is installed but no stores or syncs are configured",
				}, nil
			}

			for i := range stores.Items {
				store := &stores.Items[i]
				if status, reason, message := esoReadyCondition(store); status != "True" {
					return check.Result{
						Status:  check.StatusFail,
						Message: fmt.Sprintf("SecretStore %s is not Ready (%s): %s", store.GetName(), orUnknown(reason), message),
					}, nil
				}
			}

			now := opt.now()
			for i := range syncs.Items {
				if res, err := checkExternalSecret(ctx, opt, &syncs.Items[i], now); err != nil || res != nil {
					if err != nil {
						return check.Result{}, err
					}
					return *res, nil
				}
			}

			return check.Result{
				Status: check.StatusPass,
				Message: fmt.Sprintf("%d store(s) Ready, %d sync(s) fresh with their target Secrets present",
					len(stores.Items), len(syncs.Items)),
			}, nil
		},
	}
}

// checkExternalSecret returns a non-nil failing Result, a probe error, or
// (nil, nil) when the sync is healthy.
func checkExternalSecret(ctx context.Context, opt Options, es *unstructured.Unstructured, now time.Time) (*check.Result, error) {
	name := es.GetName()
	if status, reason, message := esoReadyCondition(es); status != "True" {
		return &check.Result{
			Status:  check.StatusFail,
			Message: fmt.Sprintf("ExternalSecret %s is not Ready (%s): %s", name, orUnknown(reason), message),
		}, nil
	}

	// Freshness: ESO stamps status.refreshTime on every successful sync. A
	// Ready object without one, or with one that does not parse, is an
	// inconsistent upstream state — unknown, never hardened.
	refreshRaw, found, err := unstructured.NestedString(es.Object, "status", "refreshTime")
	if err != nil || !found || refreshRaw == "" {
		return nil, fmt.Errorf("ExternalSecret %s is Ready but carries no readable status.refreshTime", name)
	}
	refreshed, err := time.Parse(time.RFC3339, refreshRaw)
	if err != nil {
		return nil, fmt.Errorf("ExternalSecret %s: parse status.refreshTime %q: %w", name, refreshRaw, err)
	}
	interval := esoDefaultRefreshInterval
	if raw, _, _ := unstructured.NestedString(es.Object, "spec", "refreshInterval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("ExternalSecret %s: parse spec.refreshInterval %q: %w", name, raw, err)
		}
		interval = d
	}
	if age := now.Sub(refreshed); age > time.Duration(esoRefreshGraceFactor)*interval {
		return &check.Result{
			Status: check.StatusFail,
			Message: fmt.Sprintf("ExternalSecret %s last synced %s ago (interval %s) — ESO cannot refresh it",
				name, fmtDuration(age), interval),
		}, nil
	}

	// The produced Secret must actually exist — the App references it by name.
	target := name
	if t, _, _ := unstructured.NestedString(es.Object, "spec", "target", "name"); t != "" {
		target = t
	}
	var sec corev1.Secret
	err = opt.Client.Get(ctx, client.ObjectKey{Namespace: appsNamespace, Name: target}, &sec)
	switch {
	case apierrors.IsNotFound(err):
		return &check.Result{
			Status:  check.StatusFail,
			Message: fmt.Sprintf("ExternalSecret %s reports Ready but its target Secret %s is missing", name, target),
		}, nil
	case err != nil:
		return nil, fmt.Errorf("read target Secret %s/%s: %w", appsNamespace, target, err)
	}
	return nil, nil
}

// esoReadyCondition extracts the Ready condition from an ESO object's status;
// absent conditions read as Unknown, never as healthy.
func esoReadyCondition(u *unstructured.Unstructured) (status, reason, message string) {
	status = "Unknown"
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != "Ready" {
			continue
		}
		if v, _ := m["status"].(string); v != "" {
			status = v
		}
		reason, _ = m["reason"].(string)
		message, _ = m["message"].(string)
	}
	return status, reason, message
}

func orUnknown(reason string) string {
	if reason == "" {
		return "Unknown"
	}
	return reason
}
