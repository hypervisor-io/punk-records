package api

import (
	_ "embed"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/hypervisor-io/punk-records/internal/task"
)

// handleCancelTask lets an operator kill any open task.
func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.ledger.SetStatus(r.Context(), id, task.StatusCanceled, "operator", "canceled via API"); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceled"})
}

// handleRequeueTask resumes a parked (input_required) task.
func (s *Server) handleRequeueTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.ledger.Requeue(r.Context(), id, "operator", "requeued via API"); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "requeued"})
}

// handleListProposals feeds the approval inbox.
func (s *Server) handleListProposals(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "proposed"
	}
	list, err := s.props.ListByStatus(r.Context(), status, queryLimit(r))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleListAgents exposes the active snapshot for the spec browser.
func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	snap := s.reg.Current()
	type agentOut struct {
		Name        string   `json:"name"`
		Version     string   `json:"version"`
		Description string   `json:"description"`
		Autonomy    string   `json:"autonomy"`
		Disabled    bool     `json:"disabled"`
		Skills      []string `json:"skills"`
		VersionID   int64    `json:"version_id"`
	}
	out := struct {
		SnapshotVersion int64      `json:"snapshot_version"`
		Agents          []agentOut `json:"agents"`
	}{}
	if snap != nil {
		out.SnapshotVersion = snap.Version
		for name, a := range snap.Bundle.Agents {
			out.Agents = append(out.Agents, agentOut{
				Name: a.Name, Version: a.Version, Description: a.Description,
				Autonomy: a.Autonomy, Disabled: a.Disabled, Skills: a.Skills,
				VersionID: snap.AgentVersionIDs[name],
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

//go:embed ui/index.html
var uiHTML []byte

// MountUI serves the operator console. Auth rides the same bearer the
// JS sends per request; the page itself is public chrome.
func (s *Server) MountUI() {
	s.mux.Get("/ui", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(uiHTML)
	})
}

// MountAgentCard serves the A2A Agent Card at the well-known discovery path.
// The card advertises the full v0.3 JSON-RPC task transport (streaming,
// push, one skill per registered agent) - see buildAgentCard.
func (s *Server) MountAgentCard(version string) {
	s.version = version
	s.mux.Get("/.well-known/agent-card.json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.buildAgentCard())
	})
}
