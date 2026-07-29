package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/internal/db"
	"github.com/orkanoio/orkano/internal/repoallowlist"
)

var errRepoAllowlistInvalid = errors.New("repository allowlist configuration is invalid")

const repoAllowlistAuditTimeout = 2 * time.Second

type repoAllowlistResponse struct {
	Repositories    []string `json:"repositories"`
	ResourceVersion string   `json:"resourceVersion"`
}

type updateRepoAllowlistRequest struct {
	Repositories    []string `json:"repositories"`
	ResourceVersion string   `json:"resourceVersion"`
}

func (s *Server) handleGetRepoAllowlist(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.loadRepoAllowlistSnapshot(r.Context())
	if err != nil {
		s.writeRepoAllowlistReadError(w, "repo allowlist get", err)
		return
	}
	writeJSON(w, http.StatusOK, repoAllowlistResponse{
		Repositories:    append([]string{}, snapshot.Repositories...),
		ResourceVersion: snapshot.ResourceVersion,
	})
}

func (s *Server) handleUpdateRepoAllowlist(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := userFromContext(ctx)
	var req updateRepoAllowlistRequest
	if !s.decodeAPIJSON(w, r, &req) {
		s.recordRepoAllowlistAudit(r, user, "failure", "")
		return
	}
	if req.Repositories == nil {
		s.recordRepoAllowlistAudit(r, user, "failure", "")
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if req.ResourceVersion == "" {
		s.recordRepoAllowlistAudit(r, user, "failure", "")
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	repositories, err := repoallowlist.Normalize(req.Repositories)
	if err != nil {
		s.recordRepoAllowlistAudit(r, user, "failure", "")
		writeJSONError(w, http.StatusUnprocessableEntity, "invalid_repo_allowlist")
		return
	}
	formatted, err := repoallowlist.Format(repositories)
	if err != nil {
		s.log.Error("format repository allowlist failed", "err", err)
		s.recordRepoAllowlistAudit(r, user, "failure", "")
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	policyHash := repoAllowlistPolicyHash(formatted)
	if err := s.appendRepoAllowlistAudit(r, user, "attempt", policyHash); err != nil {
		s.log.Error("repository allowlist audit intent failed", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}

	var configMap corev1.ConfigMap
	key := client.ObjectKey{
		Namespace: repoallowlist.Namespace,
		Name:      repoallowlist.ConfigMapName,
	}
	if err := s.cfg.K8s.Get(ctx, key, &configMap); err != nil {
		s.recordRepoAllowlistAudit(r, user, "failure", policyHash)
		s.writeRepoAllowlistReadError(w, "repo allowlist get for update", err)
		return
	}
	if configMap.ResourceVersion != req.ResourceVersion {
		s.recordRepoAllowlistAudit(r, user, "failure", policyHash)
		writeJSONError(w, http.StatusConflict, "conflict")
		return
	}
	if configMap.Data == nil {
		configMap.Data = map[string]string{}
	}
	configMap.Data[repoallowlist.DataKey] = formatted
	err = s.cfg.K8s.Update(ctx, &configMap)
	if err != nil {
		snapshot, outcome, readErr := s.resolveRepoAllowlistUpdate(r.Context(), policyHash)
		if readErr != nil {
			s.log.Error("repository allowlist update result is indeterminate", "updateErr", err, "readErr", readErr)
		}
		if outcome == "success" {
			if err := s.appendRepoAllowlistAudit(r, user, outcome, policyHash); err != nil {
				s.log.Error("repository allowlist success audit failed after policy read-back", "err", err)
				writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
				return
			}
			writeJSON(w, http.StatusOK, repoAllowlistResponse{
				Repositories:    append([]string{}, snapshot.Repositories...),
				ResourceVersion: snapshot.ResourceVersion,
			})
			return
		}
		s.recordRepoAllowlistAudit(r, user, outcome, policyHash)
		s.writeK8sError(w, "repo allowlist update", err)
		return
	}
	if err := s.appendRepoAllowlistAudit(r, user, "success", policyHash); err != nil {
		s.log.Error("repository allowlist success audit failed after policy update", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}

	writeJSON(w, http.StatusOK, repoAllowlistResponse{
		Repositories:    append([]string{}, repositories...),
		ResourceVersion: configMap.ResourceVersion,
	})
}

func (s *Server) resolveRepoAllowlistUpdate(
	requestContext context.Context,
	policyHash string,
) (repoAllowlistSnapshot, string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(requestContext), repoAllowlistAuditTimeout)
	defer cancel()
	snapshot, err := s.loadRepoAllowlistSnapshot(ctx)
	if err != nil {
		return repoAllowlistSnapshot{}, "indeterminate", err
	}
	formatted, err := repoallowlist.Format(snapshot.Repositories)
	if err != nil {
		return repoAllowlistSnapshot{}, "indeterminate", err
	}
	if repoAllowlistPolicyHash(formatted) == policyHash {
		return snapshot, "success", nil
	}
	return snapshot, "failure", nil
}

func repoAllowlistPolicyHash(formatted string) string {
	sum := sha256.Sum256([]byte(formatted))
	return hex.EncodeToString(sum[:])
}

func (s *Server) recordRepoAllowlistAudit(
	r *http.Request,
	user *sessionUser,
	outcome string,
	policyHash string,
) {
	if err := s.appendRepoAllowlistAudit(r, user, outcome, policyHash); err != nil {
		s.log.Warn("repository allowlist audit append failed", "outcome", outcome, "err", err)
	}
}

func (s *Server) appendRepoAllowlistAudit(
	r *http.Request,
	user *sessionUser,
	outcome string,
	policyHash string,
) error {
	detail := map[string]any{"ip": clientIP(r)}
	if requestID := middleware.GetReqID(r.Context()); requestID != "" {
		detail["requestId"] = requestID
	}
	if policyHash != "" {
		detail["policySha256"] = policyHash
	}
	payload, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("marshal repository allowlist audit detail: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), repoAllowlistAuditTimeout)
	defer cancel()
	if err := s.cfg.Store.AppendAuditEntry(ctx, db.AppendAuditEntryParams{
		Actor:   actorName(user),
		Action:  "github.repo_allowlist_update",
		Target:  repoallowlist.ConfigMapName,
		Outcome: outcome,
		Detail:  payload,
	}); err != nil {
		return fmt.Errorf("append repository allowlist audit: %w", err)
	}
	return nil
}

func (s *Server) loadRepoAllowlist(ctx context.Context) ([]string, error) {
	snapshot, err := s.loadRepoAllowlistSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	return snapshot.Repositories, nil
}

type repoAllowlistSnapshot struct {
	Repositories    []string
	ResourceVersion string
}

func (s *Server) loadRepoAllowlistSnapshot(ctx context.Context) (repoAllowlistSnapshot, error) {
	var configMap corev1.ConfigMap
	if err := s.cfg.K8s.Get(ctx, client.ObjectKey{
		Namespace: repoallowlist.Namespace,
		Name:      repoallowlist.ConfigMapName,
	}, &configMap); err != nil {
		return repoAllowlistSnapshot{}, err
	}
	raw, ok := configMap.Data[repoallowlist.DataKey]
	if !ok {
		return repoAllowlistSnapshot{}, fmt.Errorf("%w: ConfigMap %s/%s has no %q key",
			errRepoAllowlistInvalid, repoallowlist.Namespace, repoallowlist.ConfigMapName, repoallowlist.DataKey)
	}
	repositories, err := repoallowlist.Parse(raw)
	if err != nil {
		return repoAllowlistSnapshot{}, fmt.Errorf("%w: %w", errRepoAllowlistInvalid, err)
	}
	return repoAllowlistSnapshot{
		Repositories:    repositories,
		ResourceVersion: configMap.ResourceVersion,
	}, nil
}

func (s *Server) writeRepoAllowlistReadError(w http.ResponseWriter, action string, err error) {
	switch {
	case apierrors.IsNotFound(err):
		s.log.Warn("repository allowlist ConfigMap is missing", "action", action, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "cluster_not_ready")
	case errors.Is(err, errRepoAllowlistInvalid):
		s.log.Error("repository allowlist ConfigMap is invalid", "action", action, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
	default:
		s.writeK8sError(w, action, err)
	}
}
