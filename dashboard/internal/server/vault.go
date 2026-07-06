package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

// The ESO kinds are read and written as unstructured on purpose: no
// external-secrets Go dependency, the same deliberate choice the Domain
// controller makes for cert-manager Certificates. The dashboard writes only
// the narrow shapes below (ADR-0018) — never a caller-supplied raw spec, so a
// request can neither point a store's auth at a foreign Secret nor aim a sync
// at creationPolicy Merge/Orphan.
var (
	secretStoreGVK        = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "SecretStore"}
	secretStoreListGVK    = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "SecretStoreList"}
	externalSecretGVK     = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"}
	externalSecretListGVK = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecretList"}
)

const (
	// credentialsSuffix names the value-blind Secret a store's auth points at:
	// <store>-credentials (ADR-0018 design-by-example). The suffix is reserved
	// so an ExternalSecret can never target a store credential.
	credentialsSuffix = "-credentials"
	// envSecretSuffix is the env editor's managed-Secret suffix, reserved so a
	// sync can never be aimed at an app's env Secret.
	envSecretSuffix = "-env"
	// credentialsTokenKey is the one key inside a Vault store's credentials
	// Secret; spec.provider.vault.auth.tokenSecretRef names it.
	credentialsTokenKey = "token"
	// defaultRefreshInterval keeps synced Secrets reasonably fresh without
	// hammering the store (the ADR example value).
	defaultRefreshInterval = "1h"
	// maxSyncKeys mirrors the env editor's 64-var cap.
	maxSyncKeys = 64
)

func credentialsName(store string) string { return store + credentialsSuffix }

// writeVaultK8sError maps a missing ESO CRD to its own code before the shared
// mapping: writeK8sError's cluster_not_ready ("resolves itself") would mislead
// here — on a cluster that never opted in, the fix is `orkano init
// --secrets-vault`, not waiting.
func (s *Server) writeVaultK8sError(w http.ResponseWriter, action string, err error) {
	if meta.IsNoMatchError(err) {
		writeJSONError(w, http.StatusServiceUnavailable, "secrets_vault_not_installed")
		return
	}
	s.writeK8sError(w, action, err)
}

type secretStoreResponse struct {
	Name              string      `json:"name"`
	CreationTimestamp metav1.Time `json:"creationTimestamp"`
	Provider          string      `json:"provider"`
	Server            string      `json:"server,omitempty"`
	Path              string      `json:"path,omitempty"`
	Ready             string      `json:"ready"`
	Reason            string      `json:"reason,omitempty"`
	Message           string      `json:"message,omitempty"`
}

type externalSecretResponse struct {
	Name              string            `json:"name"`
	CreationTimestamp metav1.Time       `json:"creationTimestamp"`
	StoreName         string            `json:"storeName"`
	RefreshInterval   string            `json:"refreshInterval,omitempty"`
	Keys              []syncKeyResponse `json:"keys"`
	Ready             string            `json:"ready"`
	Reason            string            `json:"reason,omitempty"`
	Message           string            `json:"message,omitempty"`
	RefreshTime       string            `json:"refreshTime,omitempty"`
}

type syncKeyResponse struct {
	SecretKey string `json:"secretKey"`
	RemoteKey string `json:"remoteKey"`
}

// readyCondition extracts the Ready condition from an unstructured ESO
// object's status; absent conditions read as Unknown, never as healthy.
func readyCondition(u *unstructured.Unstructured) (status, reason, message string) {
	status = "Unknown"
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
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

type secretStoreWriteRequest struct {
	Name  string `json:"name"`
	Vault struct {
		Server  string `json:"server"`
		Path    string `json:"path"`
		Version string `json:"version"`
	} `json:"vault"`
	// Token is write-only (value-blind, ADR-0013): stored in the credentials
	// Secret, never echoed back. Optional on update — empty keeps the current
	// credential and rewires only the store spec.
	Token string `json:"token"`
}

// validateStoreRequest normalizes and validates the Vault connect shape; it
// returns a JSON error code ("" = valid). Only https servers are accepted:
// the credential this store carries flows over that connection on every sync.
func validateStoreRequest(req *secretStoreWriteRequest) string {
	if !validResourceName(req.Name) ||
		len(credentialsName(req.Name)) > maxObjectNameLen ||
		strings.HasSuffix(req.Name, credentialsSuffix) ||
		strings.HasSuffix(req.Name, envSecretSuffix) {
		return "invalid_name"
	}
	u, err := url.Parse(req.Vault.Server)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "vault_server_must_be_https"
	}
	if req.Vault.Path == "" || len(req.Vault.Path) > 1024 {
		return "invalid_vault_path"
	}
	switch req.Vault.Version {
	case "":
		req.Vault.Version = "v2"
	case "v1", "v2":
	default:
		return "invalid_vault_version"
	}
	return ""
}

func secretStoreObject(req *secretStoreWriteRequest) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": secretStoreGVK.GroupVersion().String(),
		"kind":       secretStoreGVK.Kind,
		"metadata": map[string]any{
			"name":      req.Name,
			"namespace": appsNamespace,
		},
		"spec": map[string]any{
			"provider": map[string]any{
				"vault": map[string]any{
					"server":  req.Vault.Server,
					"path":    req.Vault.Path,
					"version": req.Vault.Version,
					"auth": map[string]any{
						"tokenSecretRef": map[string]any{
							"name": credentialsName(req.Name),
							"key":  credentialsTokenKey,
						},
					},
				},
			},
		},
	}}
	return u
}

// writeCredentialsSecret blind-writes the store's credentials Secret, owned by
// the store so deletion cascades (the dashboard holds no secrets delete). The
// update fallback mirrors writeEnvSecret (no resourceVersion — the only way to
// replace a Secret the dashboard is forbidden to read) but is allowed ONLY on
// rotation: on connect an existing Secret at this name belongs to something
// else, and blindly replacing it would destroy that object's data (the
// review-found Postgres-connection-Secret clobber), so create is strict there.
func (s *Server) writeCredentialsSecret(ctx context.Context, store *unstructured.Unstructured, token string, allowExisting bool) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      credentialsName(store.GetName()),
			Namespace: appsNamespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: secretStoreGVK.GroupVersion().String(),
				Kind:       secretStoreGVK.Kind,
				Name:       store.GetName(),
				UID:        store.GetUID(),
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{credentialsTokenKey: []byte(token)},
	}
	if err := s.cfg.K8s.Create(ctx, secret); err != nil {
		if !allowExisting || !apierrors.IsAlreadyExists(err) {
			return err
		}
		return s.cfg.K8s.Update(ctx, secret)
	}
	return nil
}

// credentialsNameTaken reports whether the store's credentials Secret name is
// already claimed by a catalog Postgres — whose connection Secret is named
// after the object (ADR-0014) — the one same-name collision the value-blind
// dashboard can detect without a secrets read. Anything else squatting on the
// name surfaces as AlreadyExists from the strict create.
func (s *Server) credentialsNameTaken(ctx context.Context, storeName string) (bool, error) {
	var pg orkanov1alpha1.Postgres
	err := s.cfg.K8s.Get(ctx, client.ObjectKey{Namespace: appsNamespace, Name: credentialsName(storeName)}, &pg)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

// esoClaimsSecretName reports whether an ESO object already claims name as a
// Secret: a dashboard-authored ExternalSecret (target.name == metadata.name)
// or, for a name ending in -credentials, the SecretStore it belongs to. The
// Postgres create handler mirrors the vault API's collision refusals through
// this. NoMatch — ESO not installed — claims nothing.
func (s *Server) esoClaimsSecretName(ctx context.Context, name string) (bool, error) {
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(externalSecretGVK)
	err := s.cfg.K8s.Get(ctx, client.ObjectKey{Namespace: appsNamespace, Name: name}, es)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
	default:
		return false, err
	}
	base, ok := strings.CutSuffix(name, credentialsSuffix)
	if !ok || base == "" {
		return false, nil
	}
	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(secretStoreGVK)
	err = s.cfg.K8s.Get(ctx, client.ObjectKey{Namespace: appsNamespace, Name: base}, store)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return false, nil
	default:
		return false, err
	}
}

func (s *Server) handleListSecretStores(w http.ResponseWriter, r *http.Request) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(secretStoreListGVK)
	if err := s.cfg.ViewerClient.List(r.Context(), list, client.InNamespace(appsNamespace)); err != nil {
		s.writeVaultK8sError(w, "secretstore.list", err)
		return
	}
	out := make([]secretStoreResponse, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, secretStoreToResponse(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func secretStoreToResponse(u *unstructured.Unstructured) secretStoreResponse {
	res := secretStoreResponse{
		Name:              u.GetName(),
		CreationTimestamp: u.GetCreationTimestamp(),
		Provider:          "unknown",
	}
	provider, _, _ := unstructured.NestedMap(u.Object, "spec", "provider")
	for name := range provider {
		res.Provider = name
	}
	res.Server, _, _ = unstructured.NestedString(u.Object, "spec", "provider", "vault", "server")
	res.Path, _, _ = unstructured.NestedString(u.Object, "spec", "provider", "vault", "path")
	res.Ready, res.Reason, res.Message = readyCondition(u)
	return res
}

func (s *Server) handleCreateSecretStore(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	var req secretStoreWriteRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	if code := validateStoreRequest(&req); code != "" {
		writeJSONError(w, http.StatusBadRequest, code)
		return
	}
	if req.Token == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_token")
		return
	}

	if taken, err := s.credentialsNameTaken(r.Context(), req.Name); err != nil {
		s.writeK8sError(w, "secretstore.create", err)
		return
	} else if taken {
		writeJSONError(w, http.StatusConflict, "credentials_name_taken")
		return
	}

	store := secretStoreObject(&req)
	err := s.cfg.K8s.Create(r.Context(), store)
	s.auditResult(r, user, "secretstore.create", req.Name, err)
	if err != nil {
		s.writeVaultK8sError(w, "secretstore.create", err)
		return
	}
	// The credentials Secret comes second so it can carry the store's UID as
	// owner (cascade delete). If its write fails, roll the store back rather
	// than leave a connect that can never become Ready.
	if err := s.writeCredentialsSecret(r.Context(), store, req.Token, false); err != nil {
		if delErr := s.cfg.K8s.Delete(r.Context(), store); delErr != nil {
			s.log.Warn("SecretStore rollback after failed credentials write also failed",
				"store", req.Name, "err", delErr)
		}
		s.auditResult(r, user, "secretstore.credentials", req.Name, err)
		if apierrors.IsAlreadyExists(err) {
			writeJSONError(w, http.StatusConflict, "credentials_name_taken")
			return
		}
		s.writeVaultK8sError(w, "secretstore.credentials", err)
		return
	}
	writeJSON(w, http.StatusCreated, secretStoreToResponse(store))
}

func (s *Server) handleUpdateSecretStore(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	var req secretStoreWriteRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	req.Name = name
	if code := validateStoreRequest(&req); code != "" {
		writeJSONError(w, http.StatusBadRequest, code)
		return
	}
	// Rotation blind-writes <name>-credentials, so the same collision guard
	// as connect applies (a kubectl-authored store could sit at a name whose
	// credentials Secret is a Postgres connection Secret).
	if req.Token != "" {
		if taken, err := s.credentialsNameTaken(r.Context(), name); err != nil {
			s.writeK8sError(w, "secretstore.update", err)
			return
		} else if taken {
			writeJSONError(w, http.StatusConflict, "credentials_name_taken")
			return
		}
	}

	// Read-modify-write on the SA client (the write path): replace the spec
	// with the dashboard-owned shape, never merge caller YAML.
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(secretStoreGVK)
	key := client.ObjectKey{Namespace: appsNamespace, Name: name}
	if err := s.cfg.K8s.Get(r.Context(), key, existing); err != nil {
		s.auditResult(r, user, "secretstore.update", name, err)
		s.writeVaultK8sError(w, "secretstore.update", err)
		return
	}
	desired := secretStoreObject(&req)
	existing.Object["spec"] = desired.Object["spec"]
	err := s.cfg.K8s.Update(r.Context(), existing)
	s.auditResult(r, user, "secretstore.update", name, err)
	if err != nil {
		s.writeVaultK8sError(w, "secretstore.update", err)
		return
	}
	if req.Token != "" {
		if err := s.writeCredentialsSecret(r.Context(), existing, req.Token, true); err != nil {
			s.auditResult(r, user, "secretstore.credentials", name, err)
			s.writeVaultK8sError(w, "secretstore.credentials", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, secretStoreToResponse(existing))
}

func (s *Server) handleDeleteSecretStore(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(secretStoreGVK)
	store.SetNamespace(appsNamespace)
	store.SetName(name)
	err := s.cfg.K8s.Delete(r.Context(), store)
	s.auditResult(r, user, "secretstore.delete", name, err)
	if err != nil {
		s.writeVaultK8sError(w, "secretstore.delete", err)
		return
	}
	// The credentials Secret cascades via its owner reference.
	w.WriteHeader(http.StatusNoContent)
}

type externalSecretCreateRequest struct {
	Name            string `json:"name"`
	StoreName       string `json:"storeName"`
	RefreshInterval string `json:"refreshInterval"`
	Keys            []struct {
		SecretKey string `json:"secretKey"`
		RemoteKey string `json:"remoteKey"`
	} `json:"keys"`
}

func (s *Server) handleListExternalSecrets(w http.ResponseWriter, r *http.Request) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(externalSecretListGVK)
	if err := s.cfg.ViewerClient.List(r.Context(), list, client.InNamespace(appsNamespace)); err != nil {
		s.writeVaultK8sError(w, "externalsecret.list", err)
		return
	}
	out := make([]externalSecretResponse, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, externalSecretToResponse(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func externalSecretToResponse(u *unstructured.Unstructured) externalSecretResponse {
	res := externalSecretResponse{
		Name:              u.GetName(),
		CreationTimestamp: u.GetCreationTimestamp(),
		Keys:              []syncKeyResponse{},
	}
	res.StoreName, _, _ = unstructured.NestedString(u.Object, "spec", "secretStoreRef", "name")
	res.RefreshInterval, _, _ = unstructured.NestedString(u.Object, "spec", "refreshInterval")
	data, _, _ := unstructured.NestedSlice(u.Object, "spec", "data")
	for _, d := range data {
		m, ok := d.(map[string]any)
		if !ok {
			continue
		}
		k := syncKeyResponse{}
		k.SecretKey, _ = m["secretKey"].(string)
		if remote, ok := m["remoteRef"].(map[string]any); ok {
			k.RemoteKey, _ = remote["key"].(string)
		}
		res.Keys = append(res.Keys, k)
	}
	res.Ready, res.Reason, res.Message = readyCondition(u)
	res.RefreshTime, _, _ = unstructured.NestedString(u.Object, "status", "refreshTime")
	return res
}

func (s *Server) handleCreateExternalSecret(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	var req externalSecretCreateRequest
	if !s.decodeAPIJSON(w, r, &req) {
		return
	}
	if !validResourceName(req.Name) || len(req.Name) > maxObjectNameLen {
		writeJSONError(w, http.StatusBadRequest, "invalid_name")
		return
	}
	// Reserved target names: the produced Secret is named after this object
	// (target.name == metadata.name, one honest mapping), so a sync can never
	// be aimed at a store credential or an app's env Secret. ESO's
	// creationPolicy Owner is the runtime backstop for everything else.
	if strings.HasSuffix(req.Name, credentialsSuffix) || strings.HasSuffix(req.Name, envSecretSuffix) {
		writeJSONError(w, http.StatusBadRequest, "reserved_name")
		return
	}
	if req.RefreshInterval == "" {
		req.RefreshInterval = defaultRefreshInterval
	}
	if d, err := time.ParseDuration(req.RefreshInterval); err != nil || d <= 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_refresh_interval")
		return
	}
	if len(req.Keys) == 0 || len(req.Keys) > maxSyncKeys {
		writeJSONError(w, http.StatusBadRequest, "invalid_keys")
		return
	}
	seen := make(map[string]bool, len(req.Keys))
	for _, k := range req.Keys {
		// envNameRe (secrets.go) — the same EnvVar.Name pattern the CEL schema
		// enforces; the looser apimachinery IsEnvVarName admits keys ("my.key")
		// that could never be wired into spec.env later.
		if !envNameRe.MatchString(k.SecretKey) || seen[k.SecretKey] {
			writeJSONError(w, http.StatusBadRequest, "invalid_keys")
			return
		}
		if k.RemoteKey == "" || len(k.RemoteKey) > 2048 {
			writeJSONError(w, http.StatusBadRequest, "invalid_keys")
			return
		}
		seen[k.SecretKey] = true
	}

	// The catalog names its connection Secret after the Postgres object, so an
	// existing database with this name means the target Secret is claimed.
	// NoMatch is tolerated here, unlike writeK8sError's 503 mapping: this Get
	// is only a collision probe, and a cluster still converging its Postgres
	// CRD has no Postgres to collide with.
	var pg orkanov1alpha1.Postgres
	pgKey := client.ObjectKey{Namespace: appsNamespace, Name: req.Name}
	if err := s.cfg.K8s.Get(r.Context(), pgKey, &pg); err == nil {
		writeJSONError(w, http.StatusConflict, "name_conflict")
		return
	} else if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		s.writeK8sError(w, "externalsecret.create", err)
		return
	}
	// The store must exist — a typo would otherwise sit NotReady forever.
	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(secretStoreGVK)
	if err := s.cfg.K8s.Get(r.Context(), client.ObjectKey{Namespace: appsNamespace, Name: req.StoreName}, store); err != nil {
		if apierrors.IsNotFound(err) {
			writeJSONError(w, http.StatusBadRequest, "unknown_store")
			return
		}
		s.writeVaultK8sError(w, "externalsecret.create", err)
		return
	}

	keys := make([]any, 0, len(req.Keys))
	for _, k := range req.Keys {
		keys = append(keys, map[string]any{
			"secretKey": k.SecretKey,
			"remoteRef": map[string]any{"key": k.RemoteKey},
		})
	}
	es := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": externalSecretGVK.GroupVersion().String(),
		"kind":       externalSecretGVK.Kind,
		"metadata": map[string]any{
			"name":      req.Name,
			"namespace": appsNamespace,
		},
		"spec": map[string]any{
			"refreshInterval": req.RefreshInterval,
			"secretStoreRef": map[string]any{
				"kind": secretStoreGVK.Kind,
				"name": req.StoreName,
			},
			"target": map[string]any{
				"name":           req.Name,
				"creationPolicy": "Owner",
			},
			"data": keys,
		},
	}}
	err := s.cfg.K8s.Create(r.Context(), es)
	s.auditResult(r, user, "externalsecret.create", req.Name, err)
	if err != nil {
		s.writeVaultK8sError(w, "externalsecret.create", err)
		return
	}
	writeJSON(w, http.StatusCreated, externalSecretToResponse(es))
}

func (s *Server) handleDeleteExternalSecret(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(externalSecretGVK)
	es.SetNamespace(appsNamespace)
	es.SetName(name)
	err := s.cfg.K8s.Delete(r.Context(), es)
	s.auditResult(r, user, "externalsecret.delete", name, err)
	if err != nil {
		s.writeVaultK8sError(w, "externalsecret.delete", err)
		return
	}
	// The synced target Secret cascades via ESO's Owner creationPolicy.
	w.WriteHeader(http.StatusNoContent)
}
