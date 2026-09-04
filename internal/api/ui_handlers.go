package api

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"

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

//go:embed ui/brain.html ui/brain.js ui/brain-core.js ui/vendor/*
var brainFS embed.FS

// MountBrain serves the brain view at the server root (and /brain), its
// two module scripts, and the vendored renderer. Assets are embedded so the view works with
// no network; the page is public chrome and every API call it makes
// carries the bearer token, exactly like /ui.
func (s *Server) MountBrain() {
	serve := func(name, ctype string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, err := brainFS.ReadFile(name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", ctype)
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(b)
		}
	}
	page := serve("ui/brain.html", "text/html; charset=utf-8")
	s.mux.Get("/", page)      // the server's front door
	s.mux.Get("/brain", page) // stable alias for links and docs
	s.mux.Get("/brain/brain.js", serve("ui/brain.js", "text/javascript; charset=utf-8"))
	s.mux.Get("/brain/brain-core.js", serve("ui/brain-core.js", "text/javascript; charset=utf-8"))
	vendor, _ := fs.Sub(brainFS, "ui/vendor")
	s.mux.Get("/brain/vendor/{file}", func(w http.ResponseWriter, r *http.Request) {
		name := path.Base(chi.URLParam(r, "file"))
		b, err := fs.ReadFile(vendor, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		ctype := "text/plain; charset=utf-8"
		if strings.HasSuffix(name, ".js") {
			ctype = "text/javascript; charset=utf-8"
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(b)
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
