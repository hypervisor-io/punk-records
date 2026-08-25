package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hypervisor-io/punk-records/internal/memory"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type errorString string

func (e errorString) Error() string { return string(e) }

var errNotFound = errorString("not found")

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func queryLimit(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return n
}

func queryMaxTokens(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("max_tokens"))
	return n
}

type rememberIn struct {
	Key        string         `json:"key"`
	Body       string         `json:"body"`
	Attributes map[string]any `json:"attributes"`
	Author     string         `json:"author"`
	Importance float64        `json:"importance"`
}

func (s *Server) handleRemember(w http.ResponseWriter, r *http.Request) {
	var in rememberIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	f, err := s.mem.Write(r.Context(), memory.WriteInput{
		Namespace: chi.URLParam(r, "ns"), Key: in.Key, Body: in.Body,
		Attributes: in.Attributes, Author: in.Author, Writer: in.Author,
		Importance: in.Importance,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) handleRecall(w http.ResponseWriter, r *http.Request) {
	ns, prefix, limit := chi.URLParam(r, "ns"), r.URL.Query().Get("prefix"), queryLimit(r)
	maxTokens := queryMaxTokens(r)
	if asOfRaw := r.URL.Query().Get("as_of"); asOfRaw != "" {
		asOf, err := time.Parse(time.RFC3339, asOfRaw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		facts, err := s.mem.RecallAsOf(r.Context(), ns, prefix, asOf, limit)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, memory.TokenBudget(facts, maxTokens))
		return
	}
	facts, err := s.mem.Recall(r.Context(), ns, prefix, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, memory.TokenBudget(facts, maxTokens))
}

func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	err := s.mem.Forget(r.Context(), chi.URLParam(r, "ns"), key, r.URL.Query().Get("author"))
	switch {
	case errors.Is(err, memory.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case err != nil:
		writeErr(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "tombstoned", "key": key})
	}
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	ns, q, limit := chi.URLParam(r, "ns"), r.URL.Query().Get("q"), queryLimit(r)
	maxTokens := queryMaxTokens(r)
	if sinceRaw, untilRaw := r.URL.Query().Get("since"), r.URL.Query().Get("until"); sinceRaw != "" || untilRaw != "" {
		if sinceRaw == "" || untilRaw == "" {
			writeErr(w, http.StatusBadRequest, errors.New("memory: since requires until"))
			return
		}
		since, err := time.Parse(time.RFC3339, sinceRaw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		until, err := time.Parse(time.RFC3339, untilRaw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		facts, err := s.mem.WindowedSearch(r.Context(), ns, q, since, until, limit)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, memory.TokenBudget(facts, maxTokens))
		return
	}
	// Auto-parsed temporal phrases only apply to the plain search path.
	// WindowedSearch is FTS-only, so hybrid/interleave callers keep their
	// chosen path even when the query text contains a recognized temporal
	// phrase (e.g. a bare year) — otherwise they'd silently lose vector
	// fusion/scoring.
	if fusion, mode := r.URL.Query().Get("fusion"), r.URL.Query().Get("mode"); fusion != "interleave" && mode != "hybrid" {
		if from, to, cleaned, ok := memory.ParseTemporal(q, time.Now()); ok {
			facts, err := s.mem.WindowedSearch(r.Context(), ns, cleaned, from, to, limit)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, http.StatusOK, memory.TokenBudget(facts, maxTokens))
			return
		}
	}
	if r.URL.Query().Get("fusion") == "interleave" {
		scored, err := s.mem.InterleaveSearch(r.Context(), ns, q, limit)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, memory.TokenBudgetScored(scored, maxTokens))
		return
	}
	if r.URL.Query().Get("mode") == "hybrid" {
		halfLife := time.Duration(0)
		if h := r.URL.Query().Get("half_life_hours"); h != "" {
			n, err := strconv.Atoi(h)
			if err != nil || n < 0 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad half_life_hours"})
				return
			}
			halfLife = time.Duration(n) * time.Hour
		}
		if sc := r.URL.Query().Get("scored"); sc == "1" || sc == "true" {
			var scored []memory.ScoredFact
			var err error
			if exp := r.URL.Query().Get("expand"); (exp == "1" || exp == "true") && s.expander != nil {
				// HybridSearchExpanded already applies its own rerank pass
				// internally over the merged candidates; routing here
				// avoids a second, redundant rerank pass.
				scored, err = s.mem.HybridSearchExpanded(r.Context(), ns, q, limit, halfLife, s.expander)
			} else {
				scored, err = s.mem.HybridSearchReranked(r.Context(), ns, q, limit, halfLife)
			}
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, http.StatusOK, memory.TokenBudgetScored(scored, maxTokens))
			return
		}
		facts, err := s.mem.HybridSearch(r.Context(), ns, q, limit, halfLife)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, memory.TokenBudget(facts, maxTokens))
		return
	}
	facts, err := s.mem.Search(r.Context(), ns, q, limit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, memory.TokenBudget(facts, maxTokens))
}

// handleMemoryEvents streams memory changes for a namespace prefix over
// SSE, fed by the outbox tailer through the bus (P12.3): the wake-on-
// change surface consumers poll-free.
func (s *Server) handleMemoryEvents(w http.ResponseWriter, r *http.Request) {
	if s.bus == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "event bus not wired"})
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	ns := chi.URLParam(r, "ns")
	prefix := r.URL.Query().Get("prefix")
	events, cancel := s.bus.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	keyPrefix := ns + ":" + prefix
	for {
		select {
		case <-r.Context().Done():
			return
		case e, open := <-events:
			if !open {
				return
			}
			if e.Kind != "memory" || !strings.HasPrefix(e.Key, keyPrefix) {
				continue
			}
			raw, err := json.Marshal(e)
			if err != nil {
				continue
			}
			if _, err := w.Write([]byte("event: memory\ndata: " + string(raw) + "\n\n")); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.mem.ListKeys(r.Context(), chi.URLParam(r, "ns"), r.URL.Query().Get("prefix"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	p, err := s.mem.Profile(r.Context(), chi.URLParam(r, "ns"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	d, err := s.mem.Diagnose(r.Context(), chi.URLParam(r, "ns"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}
