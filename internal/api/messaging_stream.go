package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/region"
)

// handleMessageStream is the operator view of a whole namespace's
// message traffic (T2), distinct from handleMessageEvents which streams
// one agent's inbox hints. It never carries bodies: a `message` event
// names the id and recipient, an `ack` event names the acknowledging
// agent and the ids it acked. A sender lookup for the message event
// would need a storage round trip per event, which is not cheap enough
// to do inline on the bus delivery path, so sender is omitted; clients
// that need it re-fetch from GET /messages/log.
//
// Same authorization boundary as handleMessageEvents: streamAllowed
// (credential active AND namespace read grant) is rechecked before the
// first frame, before every delivery and on every keepalive tick, so a
// mid-stream revocation ends the stream instead of continuing to notify.
func (s *Server) handleMessageStream(w http.ResponseWriter, r *http.Request) {
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
	// Subscribe before the initial authorization check, matching
	// handleMessageEvents: an event published between check and
	// subscribe must not be missed once the stream is confirmed allowed.
	events, cancel := s.bus.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	subject := verifiedSubject(r)
	keyID := verifiedKeyID(r)
	write := func(name string, v any) bool {
		raw, err := json.Marshal(v)
		if err != nil {
			return true
		}
		if _, err := w.Write([]byte("event: " + name + "\ndata: " + string(raw) + "\n\n")); err != nil {
			return false
		}
		fl.Flush()
		return true
	}

	if !s.streamAllowed(r.Context(), keyID, subject, ns, authz.OpRead) {
		return
	}
	if !write("ready", map[string]string{"namespace": ns}) {
		return
	}

	keep := time.NewTicker(messageKeepalive)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keep.C:
			if !s.streamAllowed(r.Context(), keyID, subject, ns, authz.OpRead) {
				return
			}
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			fl.Flush()
		case e, open := <-events:
			if !open {
				return
			}
			// Bus key is ns+":"+agent for both kinds this stream cares
			// about (MessageEventKey / MessageAckEvent reuse the same
			// shape); an exact namespace match, not a raw string prefix
			// (see splitBusKey), keeps a textual-prefix namespace from
			// leaking into another's stream.
			bns, agent := splitBusKey(e.Key)
			if bns != ns {
				continue
			}
			switch e.Kind {
			case region.MessageEventKind:
				if !s.streamAllowed(r.Context(), keyID, subject, ns, authz.OpRead) {
					return
				}
				if !write("message", map[string]string{"id": e.Data["id"], "recipient": agent}) {
					return
				}
			case region.MessageAckEventKind:
				if !s.streamAllowed(r.Context(), keyID, subject, ns, authz.OpRead) {
					return
				}
				var ids []string
				if raw := e.Data["ids"]; raw != "" {
					ids = strings.Split(raw, ",")
				}
				if !write("ack", map[string]any{"agent": agent, "ids": ids}) {
					return
				}
			}
		}
	}
}
