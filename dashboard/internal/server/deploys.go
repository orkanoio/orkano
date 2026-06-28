package server

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/orkanoio/orkano/internal/db"
)

// Deploy-timeline status values the dashboard records for the App lifecycle
// changes it drives. Delete is intentionally not recorded — the timeline is a
// per-app rollout history, moot once the app is gone (the delete is still
// audited, INV-08).
const (
	deployStatusCreated = "created"
	deployStatusUpdated = "updated"
)

// deployResponse is one row of an App's deploy timeline. The dashboard records a
// row when a user creates or updates the App; the operator owns rollout truth, so
// buildName/image are empty for a dashboard-initiated change (the live image is
// on App.status, surfaced separately).
type deployResponse struct {
	OccurredAt time.Time `json:"occurredAt"`
	BuildName  string    `json:"buildName,omitempty"`
	Image      string    `json:"image,omitempty"`
	Status     string    `json:"status"`
}

func (s *Server) handleListDeploys(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePage(r)
	rows, err := s.cfg.Store.ListAppDeploys(r.Context(), db.ListAppDeploysParams{
		AppNamespace: appsNamespace,
		AppName:      chi.URLParam(r, "name"),
		Limit:        limit,
		Offset:       offset,
	})
	if err != nil {
		s.log.Error("list deploys failed", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	out := make([]deployResponse, 0, len(rows))
	for _, d := range rows {
		out = append(out, deployResponse{
			OccurredAt: d.OccurredAt.Time,
			BuildName:  d.BuildName,
			Image:      d.Image,
			Status:     d.Status,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// recordDeploy appends a deploy-timeline row, best-effort: a write failure must
// not fail the mutation that already succeeded. The operator owns rollout truth
// (the actual digest-pinned image), so a dashboard-initiated change records the
// status only — the live image stays on App.status.
func (s *Server) recordDeploy(ctx context.Context, app, status string) {
	if _, err := s.cfg.Store.RecordDeploy(ctx, db.RecordDeployParams{
		AppNamespace: appsNamespace,
		AppName:      app,
		Status:       status,
	}); err != nil {
		s.log.Warn("record deploy failed", "app", app, "err", err)
	}
}
