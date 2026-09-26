package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/region"
)

// Agent messaging HTTP transport (task M3). The region store is the
// durable inbox; these handlers are a thin JSON/SSE surface over the
// shared contract. The in-process bus carries wake hints only - it is
// lossy by design, so the SSE stream reconciles against durable storage
// on a bounded timer and unread storage stays authoritative.
//
// Authorization: every route is classNSPath - the {ns} parameter is
// enforced per request by the A01 middleware hook (GET/HEAD need read,
// other methods write), and the SSE stream revalidates credential and
// grant before every delivery plus on every keepalive tick (revocation
// closes the stream instead of continuing to notify).

// messageMaxBatch bounds read limits and ACK batches (design: batches
// of 100); it aliases the store bound so the surface can never drift.
const messageMaxBatch = region.MaxMessageBatch

// messageKeepalive is how often an SSE comment is written on an idle
// stream; messageReconcile is how often the stream rechecks durable
// storage for unacknowledged messages independent of the lossy bus.
// Vars (not consts) so tests can shorten them instead of waiting out
// the real intervals.
var (
	messageKeepalive = 15 * time.Second
	messageReconcile = 30 * time.Second
)

// messageLimit normalizes a read limit: absent/invalid means the batch
// bound; larger values clamp down to it.
func messageLimit(r *http.Request) int {
	n := queryLimit(r)
	if n <= 0 || n > messageMaxBatch {
		return messageMaxBatch
	}
	return n
}

// writeMessageErr maps the store's sentinel errors to HTTP status.
func writeMessageErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, region.ErrMessageInvalid):
		writeErr(w, http.StatusBadRequest, err)
	case errors.Is(err, region.ErrMessageNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, region.ErrMessageConflict):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, region.ErrMessageBacklog):
		writeErr(w, http.StatusTooManyRequests, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

type registerMemberIn struct {
	Agent string `json:"agent"`
	Role  string `json:"role"`
}

// handleRegisterMember joins an agent to the namespace's region
// (idempotent upsert, matching region.Register).
func (s *Server) handleRegisterMember(w http.ResponseWriter, r *http.Request) {
	var in registerMemberIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ns := chi.URLParam(r, "ns")
	if in.Agent == "" {
		writeErr(w, http.StatusBadRequest, errors.New("api: agent is required"))
		return
	}
	if err := s.region.Register(r.Context(), ns, in.Agent, in.Role); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"namespace": ns, "agent": in.Agent, "status": "registered",
	})
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	members, err := s.region.MemberStatuses(r.Context(), chi.URLParam(r, "ns"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if members == nil {
		members = []region.MemberStatus{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": members})
}

// handleSendMessage accepts a MessageInput without namespace (the path
// parameter is the namespace; a conflicting body namespace is rejected
// so the authorized segment and the stored row can never disagree) and
// defaults an empty sender to the verified credential subject, the same
// identity default the MCP tools apply. After the durable write it
// publishes the lossy wake hint ("send surfaces publish"): the hint
// names the recipient, never the body.
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	var in region.MessageInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ns := chi.URLParam(r, "ns")
	if in.Namespace != "" && in.Namespace != ns {
		writeErr(w, http.StatusBadRequest, errors.New("api: body namespace conflicts with path namespace"))
		return
	}
	in.Namespace = ns
	if in.Sender == "" {
		in.Sender = verifiedSubject(r)
	}
	msg, err := s.region.SendMessage(r.Context(), in)
	if err != nil {
		writeMessageErr(w, err)
		return
	}
	if s.bus != nil {
		s.bus.Publish(region.MessageEvent(msg))
	}
	writeJSON(w, http.StatusCreated, msg)
}

func messageReadOptions(r *http.Request) (string, region.MessageReadOptions, error) {
	q := r.URL.Query()
	agent := q.Get("agent")
	opts := region.MessageReadOptions{Limit: messageLimit(r), Box: q.Get("box"), ID: q.Get("id"), LeasedBy: q.Get("leased_by")}
	if opts.Box == "sent" {
		if sender := q.Get("sender"); sender != "" {
			agent = sender
		}
	} else if q.Get("sender") != "" {
		return "", opts, fmt.Errorf("%w: sender requires box=sent", region.ErrMessageInvalid)
	}
	if q.Has("lease_seconds") {
		n, err := strconv.Atoi(q.Get("lease_seconds"))
		if err != nil || n <= 0 {
			return "", opts, fmt.Errorf("%w: lease_seconds must be 1 to 300", region.ErrMessageInvalid)
		}
		opts.LeaseSeconds = n
	}
	return agent, opts, opts.Validate(chi.URLParam(r, "ns"), agent)
}

func (s *Server) handleReadMessages(w http.ResponseWriter, r *http.Request) {
	agent, opts, err := messageReadOptions(r)
	if err != nil {
		writeMessageErr(w, err)
		return
	}
	ns := chi.URLParam(r, "ns")
	// A lease mutates delivery state; a read-only grant must not suppress
	// delivery for other consumers even though this is a GET endpoint.
	if opts.LeaseSeconds > 0 && !s.authorizeResolved(w, r, ns, authz.OpWrite) {
		return
	}
	// A read is a sighting: hook clients have no stream, so last_seen_at
	// is what tells other agents the address is still being read.
	_ = s.region.Touch(r.Context(), ns, agent)
	msgs, err := s.region.ReadMessagesWithOptions(r.Context(), ns, agent, opts)
	if err != nil {
		writeMessageErr(w, err)
		return
	}
	if msgs == nil {
		msgs = []region.Message{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

// messageLogOptions parses the /messages/log query: agent (matches
// sender or recipient), limit (region.ListMessages clamps it) and
// before (seq cursor for paging backward).
func messageLogOptions(r *http.Request) (region.MessageLogOptions, error) {
	q := r.URL.Query()
	opts := region.MessageLogOptions{Agent: q.Get("agent"), Limit: queryLimit(r)}
	if q.Has("before") {
		n, err := strconv.ParseInt(q.Get("before"), 10, 64)
		if err != nil {
			return opts, fmt.Errorf("%w: before must be a number", region.ErrMessageInvalid)
		}
		opts.BeforeSeq = n
	}
	return opts, nil
}

// handleMessageLog is the read-only operator view of a namespace's
// messages: every row including acknowledged ones, newest first. It
// never leases and never acknowledges. Authorization mirrors
// handleListMembers: the {ns} path segment behind the A01 middleware
// hook (GET needs read).
func (s *Server) handleMessageLog(w http.ResponseWriter, r *http.Request) {
	opts, err := messageLogOptions(r)
	if err != nil {
		writeMessageErr(w, err)
		return
	}
	ns := chi.URLParam(r, "ns")
	msgs, err := s.region.ListMessages(r.Context(), ns, opts)
	if err != nil {
		writeMessageErr(w, err)
		return
	}
	if msgs == nil {
		msgs = []region.Message{}
	}
	var nextBefore int64
	if len(msgs) > 0 {
		nextBefore = msgs[len(msgs)-1].Seq
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "next_before": nextBefore})
}

func (s *Server) handleCountMessages(w http.ResponseWriter, r *http.Request) {
	n, err := s.region.CountUnreadMessages(r.Context(), chi.URLParam(r, "ns"), r.URL.Query().Get("agent"))
	if err != nil {
		writeMessageErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"unread": n})
}

type ackMessagesIn struct {
	Agent    string   `json:"agent"`
	IDs      []string `json:"ids"`
	LeasedBy string   `json:"leased_by,omitempty"`
}

// handleAckMessages acknowledges explicitly supplied ids for one
// recipient. ACK is idempotent and scoped to namespace+recipient; it
// means received, not task completed. After a successful ack it
// publishes a MessageAckEvent (ids and recipient only, never bodies) so
// the namespace message stream (T2) can notify operators.
func (s *Server) handleAckMessages(w http.ResponseWriter, r *http.Request) {
	var in ackMessagesIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.Agent == "" {
		writeErr(w, http.StatusBadRequest, errors.New("api: agent is required"))
		return
	}
	if len(in.IDs) > messageMaxBatch {
		writeErr(w, http.StatusBadRequest, errors.New("api: ids exceeds batch bound"))
		return
	}
	ns := chi.URLParam(r, "ns")
	n, err := s.region.AckMessagesWithLease(r.Context(), ns, in.Agent, in.IDs, in.LeasedBy)
	if err != nil {
		writeMessageErr(w, err)
		return
	}
	if s.bus != nil && len(in.IDs) > 0 {
		s.bus.Publish(region.MessageAckEvent(ns, in.Agent, in.IDs))
	}
	writeJSON(w, http.StatusOK, map[string]int64{"acked": n})
}

func (s *Server) handleReleaseMessages(w http.ResponseWriter, r *http.Request) {
	var in ackMessagesIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	n, err := s.region.ReleaseMessageLeases(r.Context(), chi.URLParam(r, "ns"), in.Agent, in.IDs, in.LeasedBy)
	if err != nil {
		writeMessageErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"released": n})
}

// handleMessageEvents streams inbox-change hints for one agent over
// SSE. It never carries message bodies: clients retrieve full unread
// messages via GET. The stream subscribes to the bus BEFORE the initial
// backlog check (a message arriving between check and subscribe is not
// missed), always sends one initial hint so reconnecting readers recover
// unacknowledged messages without any cursor, revalidates credential
// and grant before every delivery (mid-stream revocation closes the
// stream), keepalives an idle connection while revalidating the
// credential and grant, and reconciles against durable storage on a bounded timer
// because the bus is lossy.
func (s *Server) handleMessageEvents(w http.ResponseWriter, r *http.Request) {
	agent, opts, err := messageReadOptions(r)
	if err != nil {
		writeMessageErr(w, err)
		return
	}
	if opts.Box == "sent" || opts.ID != "" || opts.LeaseSeconds != 0 {
		writeMessageErr(w, fmt.Errorf("%w: events only supports an unleased inbox", region.ErrMessageInvalid))
		return
	}
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
	events, cancel := s.bus.Subscribe()
	defer cancel()
	// An open stream is the liveness signal member discovery reports as
	// "listening"; it is released with the stream, and the connect also
	// counts as a sighting for hook-only readers of the member list.
	detach := s.region.Attach(ns, agent)
	defer detach()
	_ = s.region.Touch(r.Context(), ns, agent)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	subject := verifiedSubject(r)
	keyID := verifiedKeyID(r)
	hint := func() bool {
		raw, err := json.Marshal(map[string]string{"agent": agent})
		if err != nil {
			return true
		}
		if _, err := w.Write([]byte("event: inbox\ndata: " + string(raw) + "\n\n")); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	// hasUnread is the durable catch-up check; the bus only hints.
	hasUnread := func() bool {
		msgs, err := s.region.ReadMessages(r.Context(), ns, agent, 1)
		if err != nil {
			s.log.Warn("api: message reconciliation read failed", "ns", ns, "agent", agent, "err", err)
			return false
		}
		return len(msgs) > 0
	}

	// Initial hint always: reconnecting readers retrieve unacknowledged
	// messages from storage, which is authoritative. Deliver only while
	// the credential and grant still hold.
	if !s.streamAllowed(r.Context(), keyID, subject, ns, authz.OpRead) {
		return
	}
	if !hint() {
		return
	}

	keep := time.NewTicker(messageKeepalive)
	defer keep.Stop()
	reconcile := time.NewTicker(messageReconcile)
	defer reconcile.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keep.C:
			// Revoke either credential or grant even on a completely idle inbox.
			if !s.streamAllowed(r.Context(), keyID, subject, ns, authz.OpRead) {
				return
			}
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			fl.Flush()
		case <-reconcile.C:
			// Loss/multiprocess recovery: durable storage, not the bus,
			// decides whether a hint is owed.
			if !hasUnread() {
				continue
			}
			if !s.streamAllowed(r.Context(), keyID, subject, ns, authz.OpRead) {
				return
			}
			if !hint() {
				return
			}
		case e, open := <-events:
			if !open {
				return
			}
			// Bus key is ns+":"+recipient. Agent names may contain ':'
			// (opencode:<sessionID>); namespaces never do, so the first
			// colon separates unambiguously.
			bns, recipient := splitBusKey(e.Key)
			if e.Kind != region.MessageEventKind || bns != ns || recipient != agent {
				continue
			}
			if !s.streamAllowed(r.Context(), keyID, subject, ns, authz.OpRead) {
				return // credential or grant revoked mid-stream: stop delivering
			}
			if !hint() {
				return
			}
		}
	}
}
