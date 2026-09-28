package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/hypervisor-io/punk-records/internal/region"
)

// Messaging delivery diagnostics (shared HTTP contract in
// docs/superpowers/specs/2026-09-28-messaging-reliability-design.md).
// Bridges report one latest observation per member; operators read them
// back with server-computed staleness. Both routes are classNSPath: the
// A01 middleware requires read for GET and write for POST on {ns}.
// Snapshots are observations, never delivery or ACK truth: GET touches no
// membership, lease or message state, and POST is not a liveness sighting.

// maxDiagnosticBody caps a report body. The largest valid report is well
// under 1 KiB; 4 KiB leaves room for additive fields.
const maxDiagnosticBody = 4 << 10

// messageDiagnosticIn is the POST body. Namespace is accepted only so a
// body naming a different namespace than the authorized path is rejected;
// unknown fields are ignored for additive compatibility.
type messageDiagnosticIn struct {
	Namespace string `json:"namespace"`
	region.MessageDiagnosticInput
}

func (s *Server) handleRecordMessageDiagnostic(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDiagnosticBody)
	dec := json.NewDecoder(r.Body)
	var in messageDiagnosticIn
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("api: diagnostic body must be one JSON object within 4 KiB with typed fields"))
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, errors.New("api: diagnostic body must be a single JSON object"))
		return
	}
	ns := chi.URLParam(r, "ns")
	if in.Namespace != "" && in.Namespace != ns {
		writeErr(w, http.StatusBadRequest, errors.New("api: body namespace conflicts with path namespace"))
		return
	}
	in.MessageDiagnosticInput.Namespace = ns
	if err := s.region.RecordMessageDiagnostic(r.Context(), in.MessageDiagnosticInput); err != nil {
		writeMessageErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

func (s *Server) handleListMessageDiagnostics(w http.ResponseWriter, r *http.Request) {
	list, err := s.region.MessageDiagnostics(r.Context(), chi.URLParam(r, "ns"), r.URL.Query().Get("agent"))
	if err != nil {
		writeMessageErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"diagnostics": list})
}
