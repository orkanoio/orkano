package server

import (
	"net/http"
	"time"

	"github.com/orkanoio/orkano/internal/checks"
	"github.com/orkanoio/orkano/internal/doctor"
)

// The dashboard doctor face — the fourth consumer of the shared check framework
// (after the install preflight, the onboarding wizard, and the CLI doctor). It
// runs doctor's read-only cluster checks per-request under the impersonated
// viewer (the setup-status pattern), reporting health plus the hardening score.
// It omits the CLI's pod-creating net.networkpolicy-enforced probe and runs the
// store-health check value-blind (SkipSecretReads): the dashboard holds no
// grant to create pods (PRD principle #9) and its viewer identity never gains
// `secrets get` (INV-03/ADR-0013).

// doctorReport is the JSON body of GET /api/doctor. The per-check shape matches
// setupCheckJSON so the SPA reuses its types.
type doctorReport struct {
	// Status mirrors run.ExitCode(): healthy (0), unhealthy (1), indeterminate (2).
	Status  string           `json:"status"`
	Score   checks.Score     `json:"score"`
	Summary checks.Summary   `json:"summary"`
	Checks  []setupCheckJSON `json:"checks"`
	// CheckedAt is when this run completed, so the UI can show a "last checked"
	// time without a second clock.
	CheckedAt string `json:"checkedAt"`
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	reg := checks.New()
	if err := doctor.RegisterReadOnly(reg, doctor.Options{
		Client:          s.cfg.ViewerClient,
		Now:             s.cfg.Now,
		SkipSecretReads: true,
	}); err != nil {
		// A malformed static check is a programming error; surface it loudly.
		s.log.Error("register doctor checks failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	run, err := reg.Run(ctx)
	if err != nil {
		s.log.Error("doctor check run failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	resp := doctorReport{
		Status:    doctorStatus(run.ExitCode()),
		Score:     run.Score(),
		Summary:   run.Summary(),
		Checks:    make([]setupCheckJSON, 0, len(run.Results)),
		CheckedAt: s.now().UTC().Format(time.RFC3339),
	}
	for _, res := range run.Results {
		resp.Checks = append(resp.Checks, setupCheckJSON{
			ID:          res.ID,
			Severity:    string(res.Severity),
			Summary:     res.Summary,
			Outcome:     string(res.Outcome),
			Message:     res.Message,
			Blockers:    res.Blockers,
			Remediation: res.Remediation,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// doctorStatus maps the run's CI exit code to the report's status word.
func doctorStatus(code int) string {
	switch code {
	case checks.ExitOK:
		return "healthy"
	case checks.ExitCritical:
		return "unhealthy"
	default:
		return "indeterminate"
	}
}
