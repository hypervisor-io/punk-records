package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hypervisor-io/punk-records/internal/task"
)

type submitTaskIn struct {
	ExternalRef string            `json:"external_ref"`
	Source      string            `json:"source"`
	Kind        string            `json:"kind"`
	Labels      map[string]string `json:"labels"`
}

type taskOut struct {
	Task     *task.Task   `json:"task"`
	Created  bool         `json:"created"`
	Decision any          `json:"decision,omitempty"`
	Events   []task.Event `json:"events,omitempty"`
}

// handleSubmitTask ingests a task, dedups by open external_ref, routes it
// immediately, and returns the task with the routing decision.
func (s *Server) handleSubmitTask(w http.ResponseWriter, r *http.Request) {
	var in submitTaskIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(in.Source) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source is required"})
		return
	}
	tk, created, err := s.ledger.Submit(r.Context(), task.SubmitInput{
		ExternalRef: in.ExternalRef,
		Source:      in.Source,
		Kind:        in.Kind,
		Labels:      in.Labels,
		Budget:      s.defaultBudget,
		Actor:       "api",
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
		// re-read: routing mutates agent assignment/status
		if fresh, _, err := s.ledger.Get(r.Context(), tk.ID); err == nil {
			out.Task = fresh
		}
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, out)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	tk, events, err := s.ledger.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, taskOut{Task: tk, Events: events})
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.ledger.List(r.Context(), r.URL.Query().Get("status"), queryLimit(r))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}
