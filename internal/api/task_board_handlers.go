package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/taskboard"
)

const (
	boardWaitMax = 300 * time.Second
)

// handleTaskBoard is GET /v1/namespaces/{ns}/tasks: the joined task board.
// ?state= filters rows; ?wait=<seconds> long-polls for a change first
// (capped at boardWaitMax) and adds changed and changes to the reply.
func (s *Server) handleTaskBoard(w http.ResponseWriter, r *http.Request) {
	ns := chi.URLParam(r, "ns")
	state := r.URL.Query().Get("state")
	if state != "" && !taskStateValid(state) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("state must be one of %s", strings.Join(memory.TaskStates, ", ")))
		return
	}
	var keys []string
	waited := false
	if raw := r.URL.Query().Get("wait"); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs < 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("wait must be a number of seconds"))
			return
		}
		timeout := time.Duration(secs) * time.Second
		if timeout > boardWaitMax {
			timeout = boardWaitMax
		}
		keys = taskboard.WaitForChange(r.Context(), s.bus, ns, timeout)
		waited = true
	}
	b, err := taskboard.Build(r.Context(), s.mem, s.region, ns)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if state != "" {
		kept := b.Tasks[:0:0]
		for _, t := range b.Tasks {
			if t.State == state {
				kept = append(kept, t)
			}
		}
		b.Tasks = kept
	}
	if !waited {
		writeJSON(w, http.StatusOK, b)
		return
	}
	if keys == nil {
		keys = []string{}
	}
	writeJSON(w, http.StatusOK, struct {
		taskboard.Board
		Changed bool     `json:"changed"`
		Changes []string `json:"changes"`
	}{b, len(keys) > 0, keys})
}

type taskStatusIn struct {
	State     string `json:"state"`
	Summary   string `json:"summary"`
	Sha       string `json:"sha,omitempty"`
	Tests     string `json:"tests,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Deviation string `json:"deviation,omitempty"`
	Agent     string `json:"agent,omitempty"`
}

// handleTaskStatus is POST /v1/namespaces/{ns}/tasks/{id}/status: the HTTP
// twin of the set_task_status MCP tool, for pi and shells.
func (s *Server) handleTaskStatus(w http.ResponseWriter, r *http.Request) {
	ns, id := chi.URLParam(r, "ns"), chi.URLParam(r, "id")
	var in taskStatusIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !taskStateValid(in.State) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("state must be one of %s", strings.Join(memory.TaskStates, ", ")))
		return
	}
	in.Summary = strings.TrimSpace(in.Summary)
	if in.Summary == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("summary is required"))
		return
	}
	if in.Agent == "" {
		in.Agent = "api"
	}
	body := memory.RenderTaskStatus(in.State, in.Summary, in.Sha, in.Tests, in.Phase, in.Deviation)
	attrs := map[string]any{"state": in.State, "summary": in.Summary, "agent": in.Agent}
	for k, v := range map[string]string{"sha": in.Sha, "tests": in.Tests, "phase": in.Phase, "deviation": in.Deviation} {
		if v != "" {
			attrs[k] = v
		}
	}
	f, err := s.mem.Write(r.Context(), memory.WriteInput{
		Namespace: ns, Key: "/tasks/" + id + "/status", Body: body, Attributes: attrs,
		Author: in.Agent, Writer: in.Agent, Importance: 0.6,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	released := false
	if s.region != nil && (in.State == "done" || in.State == "blocked") {
		if err := s.region.ReleaseWork(r.Context(), ns, "/tasks/"+id, in.Agent); err == nil {
			released = true
		}
	}
	writeJSON(w, http.StatusCreated, struct {
		*memory.Fact
		Released bool `json:"released_claim"`
	}{f, released})
}

func taskStateValid(s string) bool {
	for _, st := range memory.TaskStates {
		if s == st {
			return true
		}
	}
	return false
}
