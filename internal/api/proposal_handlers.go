package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

type proposalPatchIn struct {
	Status      string `json:"status"`       // submitted|approved|rejected|expired
	ExternalRef string `json:"external_ref"` // consumer's execution id
}

// handlePatchProposal is the consumer callback: an external system (or an
// operator) reports what happened to a proposed action.
func (s *Server) handlePatchProposal(w http.ResponseWriter, r *http.Request) {
	var in proposalPatchIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	pr, err := s.props.UpdateStatus(r.Context(), chi.URLParam(r, "id"), in.Status, in.ExternalRef)
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			code = http.StatusNotFound
		}
		writeErr(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, pr)
}

func (s *Server) handleListTaskProposals(w http.ResponseWriter, r *http.Request) {
	list, err := s.props.ListByTask(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
