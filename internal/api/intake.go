package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hypervisor-io/punk-records/internal/task"
)

// intakeIn is the generic webhook shape. Consumer-specific mappings
// (e.g. incident payloads) are translated by dedicated intakes.
type intakeIn struct {
	Source      string            `json:"source"`
	ExternalRef string            `json:"external_ref"`
	Kind        string            `json:"kind"`
	Labels      map[string]string `json:"labels"`
}

// handleIntakeWebhook ingests an external event as a task: dedup by open
// external_ref, route immediately. Idempotent for event retries.
func (s *Server) handleIntakeWebhook(w http.ResponseWriter, r *http.Request) {
	var in intakeIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(in.Source) == "" || strings.TrimSpace(in.ExternalRef) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source and external_ref are required"})
		return
	}
	tk, created, err := s.ledger.Submit(r.Context(), task.SubmitInput{
		ExternalRef: in.ExternalRef,
		Source:      in.Source,
		Kind:        in.Kind,
		Labels:      in.Labels,
		Budget:      s.defaultBudget,
		Actor:       "webhook",
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := taskOut{Task: tk, Created: created}
	if created {
		d, err := s.router.Route(r.Context(), tk)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		out.Decision = d
	}
	code := http.StatusOK
	if created {
		code = http.StatusAccepted
	}
	writeJSON(w, code, out)
}
