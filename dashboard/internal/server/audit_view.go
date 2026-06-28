package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/orkanoio/orkano/internal/db"
)

// auditResponse is one append-only audit row (INV-08). detail is the structured
// jsonb context (client IP, changed env-var NAMES, ...) — never a secret value
// (INV-03), so it is safe to surface verbatim.
type auditResponse struct {
	OccurredAt time.Time       `json:"occurredAt"`
	Actor      string          `json:"actor"`
	Action     string          `json:"action"`
	Target     string          `json:"target"`
	Outcome    string          `json:"outcome"`
	Detail     json.RawMessage `json:"detail,omitempty"`
}

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePage(r)
	rows, err := s.cfg.Store.ListAuditEntries(r.Context(), db.ListAuditEntriesParams{Limit: limit, Offset: offset})
	if err != nil {
		s.log.Error("list audit failed", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	out := make([]auditResponse, 0, len(rows))
	for _, a := range rows {
		out = append(out, auditResponse{
			OccurredAt: a.OccurredAt.Time,
			Actor:      a.Actor,
			Action:     a.Action,
			Target:     a.Target,
			Outcome:    a.Outcome,
			Detail:     json.RawMessage(a.Detail),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
