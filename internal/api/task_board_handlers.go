package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/taskboard"
)

const (
	boardWaitMax = 300 * time.Second
)

// boardStateOpen is the board-only ?state filter for every task not yet
// done; it is not a task state and set_task_status never accepts it.
const boardStateOpen = "open"

// boardPageMax caps one page of board rows.
const boardPageMax = 500

// boardPage is the paging block of a board reply: total is the row count
// after the state filter and before paging.
type boardPage struct {
	Total  int `json:"total"`
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
}

// boardPaging reads ?limit (0 = every row, capped at boardPageMax) and
// ?offset (default 0); either one malformed or negative is an error.
func boardPaging(r *http.Request) (limit, offset int, err error) {
	q := r.URL.Query()
	if raw := q.Get("limit"); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil || limit < 0 {
			return 0, 0, fmt.Errorf("limit must be a non-negative number")
		}
		if limit > boardPageMax {
			limit = boardPageMax
		}
	}
	if raw := q.Get("offset"); raw != "" {
		if offset, err = strconv.Atoi(raw); err != nil || offset < 0 {
			return 0, 0, fmt.Errorf("offset must be a non-negative number")
		}
	}
	return limit, offset, nil
}

// handleTaskBoard is GET /v1/namespaces/{ns}/tasks: the joined task board.
// ?state= filters rows (a task state, or open for every state but done);
// ?limit and ?offset page them (total reports the
// filtered count); ?wait=<seconds> long-polls for a change first (capped
// at boardWaitMax) and adds changed and changes to the reply.
func (s *Server) handleTaskBoard(w http.ResponseWriter, r *http.Request) {
	ns := chi.URLParam(r, "ns")
	state := r.URL.Query().Get("state")
	if state != "" && state != boardStateOpen && !taskStateValid(state) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("state must be one of %s, %s", strings.Join(memory.TaskStates, ", "), boardStateOpen))
		return
	}
	limit, offset, err := boardPaging(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
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
		// A02: the initial GET is path-enforced by the A01 hook, but the
		// long-poll outlives that check: a credential revoked during the
		// wait answers 401, a grant revoked during the wait answers 403
		// - never a fresh board.
		if !s.credentialActive(r.Context(), verifiedKeyID(r)) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "credential revoked"})
			return
		}
		if !s.authorizeResolved(w, r, ns, authz.OpRead) {
			return
		}
	}
	b, err := taskboard.Build(r.Context(), s.mem, s.region, ns)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if state != "" {
		kept := b.Tasks[:0:0]
		for _, t := range b.Tasks {
			if t.State == state || (state == boardStateOpen && t.State != "done") {
				kept = append(kept, t)
			}
		}
		b.Tasks = kept
	}
	// Paging applies after the state filter: total is the filtered row
	// count, counts stay whole-board, and a page past the end is empty.
	page := boardPage{Total: len(b.Tasks), Limit: limit, Offset: offset}
	if offset > len(b.Tasks) {
		offset = len(b.Tasks)
	}
	b.Tasks = b.Tasks[offset:]
	if limit > 0 && limit < len(b.Tasks) {
		b.Tasks = b.Tasks[:limit]
	}
	if !waited {
		writeJSON(w, http.StatusOK, struct {
			taskboard.Board
			boardPage
		}{b, page})
		return
	}
	if keys == nil {
		keys = []string{}
	}
	writeJSON(w, http.StatusOK, struct {
		taskboard.Board
		boardPage
		Changed bool     `json:"changed"`
		Changes []string `json:"changes"`
	}{b, page, len(keys) > 0, keys})
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

// handleNamespaceIndex is GET /v1/namespaces: the directory the operator
// console lists before opening a board. It is the one /v1 route that
// spans namespaces, so the A01 path hook cannot gate it; every row is
// filtered here against the verified subject's read grant, exactly as a
// GET of that namespace would be. The summaries are cached for 20s
// (see overviewTTL). ?tasks=1 keeps only namespaces that
// carry /tasks facts.
func (s *Server) handleNamespaceIndex(w http.ResponseWriter, r *http.Request) {
	sums, err := s.namespaceSummaries(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	onlyTasks := r.URL.Query().Get("tasks") == "1"
	subject := verifiedSubject(r)
	out := make([]memory.NamespaceSummary, 0, len(sums))
	for _, sum := range sums {
		if onlyTasks && sum.Tasks == 0 {
			continue
		}
		if !s.allowNS(r.Context(), subject, sum.Name, authz.OpRead) {
			continue
		}
		out = append(out, sum)
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespaces": out})
}
