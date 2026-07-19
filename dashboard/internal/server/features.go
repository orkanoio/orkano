package server

import (
	"net/http"

	"github.com/orkanoio/orkano/internal/features"
)

type featureResponse struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Unsafe      bool   `json:"unsafe"`
	Enabled     bool   `json:"enabled"`
}

func (s *Server) handleFeatures(w http.ResponseWriter, _ *http.Request) {
	definitions := features.Definitions()
	items := make([]featureResponse, 0, len(definitions))
	for _, definition := range definitions {
		items = append(items, featureResponse{
			ID:          string(definition.ID),
			Label:       definition.Name,
			Description: definition.Description,
			Unsafe:      definition.Unsafe,
			Enabled:     s.cfg.Features.Enabled(definition.ID),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"features": items})
}
