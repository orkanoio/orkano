package server

import (
	"context"
	"net/http"
	"regexp"
	"sort"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

// envNameRe is the EnvVar name pattern (api/v1alpha1 shared_types). A name that
// matches is also a valid Secret data key, so one check guards both the spec.env
// entry and the Secret key.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const (
	// maxEnvVars mirrors AppSpec.Env's MaxItems=64. Checked client-side before the
	// Secret write so an over-limit set is a clean 400, not a 422 that lands after
	// the value Secret was already written.
	maxEnvVars = 64
	// maxObjectNameLen is the Kubernetes object-name (DNS-1123 subdomain) cap. The
	// derived "<app>-env" Secret name must fit it.
	maxObjectNameLen = 253
)

// setEnvRequest sets an App's complete set of secret-backed env vars. The
// dashboard never reads the Secret (the Role grants no get on secrets), so a
// write replaces the whole set: every secret env var the app should have, with
// its value. Values flow straight to the Kubernetes Secret and never touch the
// dashboard's database (INV-03).
type setEnvRequest struct {
	Secrets map[string]string `json:"secrets"`
}

// envSecretName is the per-app Secret holding its secret env values, keyed by env
// var name — the one Secret a user inspects with `kubectl get secret <app>-env`
// to see their app's secret environment.
func envSecretName(app string) string { return app + "-env" }

// handleSetEnv is the value-blind env editor: it writes the secret values to the
// per-app Secret and reconciles the App's spec.env to reference them. Step-up
// gated (rotating secrets is destructive, ADR-0003).
func (s *Server) handleSetEnv(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	// The derived Secret name must fit the object-name limit; a clean 400 beats an
	// opaque 422 from a pathologically long app name.
	if len(envSecretName(name)) > maxObjectNameLen {
		writeJSONError(w, http.StatusBadRequest, "app_name_too_long")
		return
	}
	var req setEnvRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	keys, ok := sortedEnvKeys(req.Secrets)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_env")
		return
	}

	// The App must exist — we also need its UID for the Secret's owner reference.
	var app orkanov1alpha1.App
	if err := s.cfg.K8s.Get(r.Context(), client.ObjectKey{Namespace: appsNamespace, Name: name}, &app); err != nil {
		s.auditEnv(r, user, name, keys, err)
		s.writeK8sError(w, "env.update", err)
		return
	}

	// Compute the reconciled env and bound it BEFORE the Secret write, so an
	// over-limit set never leaves a written Secret with no spec.env reference.
	newEnv := reconcileEnvRefs(app.Spec.Env, envSecretName(name), keys)
	if len(newEnv) > maxEnvVars {
		writeJSONError(w, http.StatusBadRequest, "env_limit_exceeded")
		return
	}

	// 1. Blind-write the value Secret (create or whole-object overwrite). This
	// precedes the spec update so a pod never references a key the Secret lacks; if
	// the spec update then fails, the values are written but unreferenced — a
	// recoverable partial state the (idempotent) retry heals.
	if err := s.writeEnvSecret(r.Context(), &app, req.Secrets); err != nil {
		s.auditEnv(r, user, name, keys, err)
		s.writeK8sError(w, "env.update", err)
		return
	}

	// 2. Point spec.env at the Secret. reconcileEnvRefs already preserved plaintext
	// vars and refs to OTHER secrets (e.g. a Postgres connection Secret).
	app.Spec.Env = newEnv
	if err := s.cfg.K8s.Update(r.Context(), &app); err != nil {
		s.auditEnv(r, user, name, keys, err)
		s.writeK8sError(w, "env.update", err)
		return
	}

	s.auditEnv(r, user, name, keys, nil)
	// The response carries the App spec (secretRef names, no values) — never the
	// secret values, which the dashboard cannot read back anyway.
	writeJSON(w, http.StatusOK, appToResponse(&app))
}

// writeEnvSecret creates or blind-overwrites the per-app env Secret. The Role
// grants secrets create+update only (never get/patch/delete, ADR-0013), so this
// is a whole-object write from the request data — no read-modify-write. The
// Secret is owned by the App so it cascades on app deletion (a plain owner
// reference: blockOwnerDeletion stays false, so no finalizers grant is needed).
func (s *Server) writeEnvSecret(ctx context.Context, app *orkanov1alpha1.App, secrets map[string]string) error {
	data := make(map[string][]byte, len(secrets))
	for k, v := range secrets {
		data[k] = []byte(v)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: envSecretName(app.Name), Namespace: appsNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
	if err := controllerutil.SetOwnerReference(app, secret, s.cfg.K8s.Scheme()); err != nil {
		return err
	}
	if err := s.cfg.K8s.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		// Whole-object overwrite: no resourceVersion is set, so the apiserver
		// replaces the object without an optimistic-concurrency check — the only
		// way to update a Secret the dashboard is forbidden to read.
		return s.cfg.K8s.Update(ctx, secret)
	}
	return nil
}

// reconcileEnvRefs returns env with the managed secret keys set as secretRefs
// into the per-app Secret, preserving plaintext vars and refs to other Secrets in
// their original order. An existing entry — plaintext OR a foreign ref — whose
// name a new secret key takes over is REPLACED (dropped here, re-added below):
// spec.env is a listType=map keyed by name, so a duplicate name would be rejected
// by the apiserver. Setting secret FOO therefore overrides any prior FOO.
func reconcileEnvRefs(env []orkanov1alpha1.EnvVar, secretName string, keys []string) []orkanov1alpha1.EnvVar {
	managed := make(map[string]bool, len(keys))
	for _, k := range keys {
		managed[k] = true
	}
	out := make([]orkanov1alpha1.EnvVar, 0, len(env)+len(keys))
	for _, e := range env {
		if managed[e.Name] {
			continue // a new secret key takes over this name
		}
		if e.SecretRef != nil && e.SecretRef.Name == secretName {
			continue // a stale managed ref no longer in the set
		}
		out = append(out, e)
	}
	for _, k := range keys {
		out = append(out, orkanov1alpha1.EnvVar{
			Name:      k,
			SecretRef: &orkanov1alpha1.SecretKeyRef{Name: secretName, Key: k},
		})
	}
	return out
}

// sortedEnvKeys validates the secret env var names and returns them sorted (a
// stable order for the audit trail and a deterministic spec.env). A name must
// match the EnvVar pattern; ok is false if any name is invalid.
func sortedEnvKeys(secrets map[string]string) ([]string, bool) {
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		if len(k) > 253 || !envNameRe.MatchString(k) {
			return nil, false
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, true
}

// auditEnv records an env-editor mutation (INV-08) carrying the changed key
// NAMES — never the values (INV-03).
func (s *Server) auditEnv(r *http.Request, user *sessionUser, app string, keys []string, err error) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	s.auditDetail(r.Context(), actorName(user), "env.update", app, outcome, r, map[string]any{"keys": keys})
}
