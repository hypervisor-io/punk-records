package api

import (
	"net/http"
	"time"

	"github.com/hypervisor-io/punk-records/internal/cost"
)

// handleCosts publishes the burn table: day/month spend per agent and
// average cost per completed investigation (P14.4).
func (s *Server) handleCosts(w http.ResponseWriter, r *http.Request) {
	o, err := cost.Summarize(r.Context(), s.db, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}
