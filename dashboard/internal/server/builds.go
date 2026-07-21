package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
	"github.com/orkanoio/orkano/internal/db"
)

const manualDeliveryPrefix = "manual-"

var errDeliveryIDCollision = errors.New("manual deploy request ID collision")

type buildResponse struct {
	Name              string                     `json:"name"`
	CreationTimestamp metav1.Time                `json:"creationTimestamp"`
	Spec              orkanov1alpha1.BuildSpec   `json:"spec"`
	Status            orkanov1alpha1.BuildStatus `json:"status"`
}

func buildToResponse(build *orkanov1alpha1.Build) buildResponse {
	return buildResponse{
		Name:              build.Name,
		CreationTimestamp: build.CreationTimestamp,
		Spec:              build.Spec,
		Status:            build.Status,
	}
}

func (s *Server) handleListBuilds(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !validResourceName(name) {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	var app orkanov1alpha1.App
	key := client.ObjectKey{Namespace: appsNamespace, Name: name}
	if err := s.cfg.ViewerClient.Get(r.Context(), key, &app); err != nil {
		s.writeK8sError(w, "builds.app.get", err)
		return
	}

	var list orkanov1alpha1.BuildList
	if err := s.cfg.ViewerClient.List(r.Context(), &list, client.InNamespace(appsNamespace)); err != nil {
		s.writeK8sError(w, "builds.list", err)
		return
	}
	sort.SliceStable(list.Items, func(i, j int) bool {
		return list.Items[i].CreationTimestamp.After(list.Items[j].CreationTimestamp.Time)
	})
	items := make([]buildResponse, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.AppName == name {
			items = append(items, buildToResponse(&list.Items[i]))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":            items,
		"repo":             sourceDisplayName(app.Spec.Source),
		"automaticDeploys": app.Spec.Source.GitHub != nil && s.repoAllowed(app.Spec.Source.GitHub.Repo),
	})
}

func (s *Server) handleDeployApp(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	name := chi.URLParam(r, "name")
	if !validResourceName(name) {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	var app orkanov1alpha1.App
	key := client.ObjectKey{Namespace: appsNamespace, Name: name}
	if err := s.cfg.K8s.Get(r.Context(), key, &app); err != nil {
		s.auditResult(r, user, "app.deploy", name, err)
		s.writeK8sError(w, "apps.deploy", err)
		return
	}
	if err := s.cfg.Features.ValidateApp(app.Spec); err != nil {
		s.auditResult(r, user, "app.deploy", name, err)
		s.writeFeatureDisabled(w, err)
		return
	}
	deliveryID, err := s.enqueueManualDeploy(r.Context(), name, sourceQueueKey(app.Spec.Source))
	s.auditResult(r, user, "app.deploy", name, err)
	if err != nil {
		s.log.Error("queue manual App build failed", "app", name, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "requestId": deliveryID})
}

func sourceQueueKey(source orkanov1alpha1.Source) string {
	switch {
	case source.GitHub != nil:
		return source.GitHub.Repo
	case source.Git != nil:
		sum := sha256.Sum256([]byte(source.Git.URL))
		return "git:" + hex.EncodeToString(sum[:])
	case source.Upload != nil:
		return "zip:" + source.Upload.Digest
	default:
		return "unknown"
	}
}

func sourceDisplayName(source orkanov1alpha1.Source) string {
	switch {
	case source.GitHub != nil:
		return source.GitHub.Repo
	case source.Git != nil:
		return source.Git.URL
	case source.Upload != nil:
		if source.Upload.FileName != "" {
			return source.Upload.FileName
		}
		return source.Upload.Digest
	default:
		return ""
	}
}

func (s *Server) enqueueManualDeploy(ctx context.Context, appName, repo string) (string, error) {
	for range 3 {
		deliveryID, err := newManualDeliveryID()
		if err != nil {
			return "", err
		}
		inserted, err := s.cfg.Store.EnqueueManualDelivery(ctx, db.EnqueueManualDeliveryParams{
			DeliveryID: deliveryID,
			Repo:       repo,
			AppName:    pgtype.Text{String: appName, Valid: true},
		})
		if err != nil {
			return "", err
		}
		if inserted == 1 {
			return deliveryID, nil
		}
	}
	return "", errDeliveryIDCollision
}

func newManualDeliveryID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return manualDeliveryPrefix + hex.EncodeToString(raw[:]), nil
}

func (s *Server) repoAllowed(repo string) bool {
	want := strings.TrimSpace(repo)
	for _, allowed := range s.cfg.RepoAllowlist {
		if strings.EqualFold(strings.TrimSpace(allowed), want) {
			return true
		}
	}
	return false
}
