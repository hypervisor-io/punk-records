package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/hypervisor-io/punk-records/internal/a2a"
	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/task"
)

// a2aBlockTimeout caps how long a blocking message/send waits for a
// terminal state before returning the task in its current (non-terminal)
// state. Clients can then poll tasks/get or resubscribe.
const a2aBlockTimeout = 30 * time.Second

// handleA2A is the single JSON-RPC 2.0 endpoint for the A2A protocol.
// Streaming methods (message/stream, tasks/resubscribe) upgrade to SSE;
// all others answer with a JSON-RPC response.
func (s *Server) handleA2A(w http.ResponseWriter, r *http.Request) {
	var req a2a.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRPCError(w, nil, a2a.ErrParse, "parse error", nil)
		return
	}
	if req.JSONRPC != "2.0" {
		writeRPCError(w, req.ID, a2a.ErrInvalidRequest, "jsonrpc must be \"2.0\"", nil)
		return
	}
	switch req.Method {
	case a2a.MethodMessageSend:
		s.a2aMessageSend(w, r, req)
	case a2a.MethodMessageStream:
		s.a2aMessageStream(w, r, req)
	case a2a.MethodTasksGet:
		s.a2aTasksGet(w, r, req)
	case a2a.MethodTasksCancel:
		s.a2aTasksCancel(w, r, req)
	case a2a.MethodTasksResubscribe:
		s.a2aResubscribe(w, r, req)
	case a2a.MethodPushConfigSet:
		s.a2aPushSet(w, r, req)
	case a2a.MethodPushConfigGet:
		s.a2aPushGet(w, r, req)
	case a2a.MethodPushConfigList:
		s.a2aPushList(w, r, req)
	case a2a.MethodPushConfigDelete:
		s.a2aPushDelete(w, r, req)
	case a2a.MethodExtendedCard:
		writeRPCResult(w, req.ID, s.buildAgentCard())
	default:
		writeRPCError(w, req.ID, a2a.ErrMethodNotFound, "method not found: "+req.Method, nil)
	}
}

// ---- message/send ----

func (s *Server) a2aMessageSend(w http.ResponseWriter, r *http.Request, req a2a.Request) {
	var p a2a.SendMessageParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPCError(w, req.ID, a2a.ErrInvalidParams, "invalid params", nil)
		return
	}
	t, rerr := s.a2aIngest(r.Context(), p)
	if rerr != nil {
		writeRPCError(w, req.ID, rerr.Code, rerr.Message, rerr.Data)
		return
	}
	if p.Configuration != nil && p.Configuration.Blocking {
		s.a2aWaitTerminal(r.Context(), t.ID, a2aBlockTimeout)
	}
	hist := 0
	if p.Configuration != nil {
		hist = p.Configuration.HistoryLength
	}
	tk, events, err := s.ledger.Get(r.Context(), t.ID)
	if err != nil {
		writeRPCError(w, req.ID, a2a.ErrInternal, err.Error(), nil)
		return
	}
	writeRPCResult(w, req.ID, a2a.TaskFromLedger(tk, events, hist))
}

// a2aIngest creates a new task from a message, or appends the message to an
// existing task when the message carries a taskId. Returns the task.
func (s *Server) a2aIngest(ctx context.Context, p a2a.SendMessageParams) (*task.Task, *a2a.RPCError) {
	msg := p.Message
	if len(msg.Parts) == 0 {
		return nil, &a2a.RPCError{Code: a2a.ErrInvalidParams, Message: "message.parts is required"}
	}
	if msg.MessageID == "" {
		msg.MessageID = randID("msg")
	}
	msg.Kind = a2a.KindMessage
	msg.Role = a2a.RoleUser

	// continue an existing task
	if msg.TaskID != "" {
		t, _, err := s.ledger.Get(ctx, msg.TaskID)
		if err != nil {
			return nil, &a2a.RPCError{Code: a2a.ErrTaskNotFound, Message: "task not found: " + msg.TaskID}
		}
		msg.ContextID = t.Labels[a2a.ContextLabel]
		if _, err := s.ledger.Append(ctx, t.ID, task.EventMessage, "a2a", msg); err != nil {
			return nil, &a2a.RPCError{Code: a2a.ErrInternal, Message: err.Error()}
		}
		return t, nil
	}

	// new task
	ctxID := msg.ContextID
	if ctxID == "" {
		ctxID = randID("ctx")
	}
	msg.ContextID = ctxID
	labels := map[string]string{a2a.ContextLabel: ctxID}
	kind := "investigate"
	if k, ok := msg.Metadata["kind"].(string); ok && k != "" {
		kind = k
	}
	t, created, err := s.ledger.Submit(ctx, task.SubmitInput{
		ExternalRef: "a2a:msg:" + msg.MessageID,
		Source:      "a2a",
		Kind:        kind,
		Labels:      labels,
		Budget:      s.defaultBudget,
		Actor:       "a2a",
	})
	if err != nil {
		return nil, &a2a.RPCError{Code: a2a.ErrInternal, Message: err.Error()}
	}
	msg.TaskID = t.ID
	if _, err := s.ledger.Append(ctx, t.ID, task.EventMessage, "a2a", msg); err != nil {
		return nil, &a2a.RPCError{Code: a2a.ErrInternal, Message: err.Error()}
	}
	if created && s.router != nil {
		if _, err := s.router.Route(ctx, t); err == nil {
			if fresh, _, err := s.ledger.Get(ctx, t.ID); err == nil {
				t = fresh
			}
		}
	}
	return t, nil
}

// ---- tasks/get ----

func (s *Server) a2aTasksGet(w http.ResponseWriter, r *http.Request, req a2a.Request) {
	var p a2a.TaskQueryParams
	if err := json.Unmarshal(req.Params, &p); err != nil || p.ID == "" {
		writeRPCError(w, req.ID, a2a.ErrInvalidParams, "invalid params: id required", nil)
		return
	}
	tk, events, err := s.ledger.Get(r.Context(), p.ID)
	if err != nil {
		writeRPCError(w, req.ID, a2a.ErrTaskNotFound, "task not found: "+p.ID, nil)
		return
	}
	writeRPCResult(w, req.ID, a2a.TaskFromLedger(tk, events, p.HistoryLength))
}

// ---- tasks/cancel ----

func (s *Server) a2aTasksCancel(w http.ResponseWriter, r *http.Request, req a2a.Request) {
	var p a2a.TaskIDParams
	if err := json.Unmarshal(req.Params, &p); err != nil || p.ID == "" {
		writeRPCError(w, req.ID, a2a.ErrInvalidParams, "invalid params: id required", nil)
		return
	}
	tk, _, err := s.ledger.Get(r.Context(), p.ID)
	if err != nil {
		writeRPCError(w, req.ID, a2a.ErrTaskNotFound, "task not found: "+p.ID, nil)
		return
	}
	if err := s.ledger.SetStatus(r.Context(), p.ID, task.StatusCanceled, "a2a", "a2a cancel"); err != nil {
		var bad *task.ErrBadTransition
		if errors.As(err, &bad) {
			writeRPCError(w, req.ID, a2a.ErrTaskNotCancelable, "task not cancelable in state "+tk.Status, nil)
			return
		}
		writeRPCError(w, req.ID, a2a.ErrInternal, err.Error(), nil)
		return
	}
	tk, events, err := s.ledger.Get(r.Context(), p.ID)
	if err != nil {
		writeRPCError(w, req.ID, a2a.ErrInternal, err.Error(), nil)
		return
	}
	writeRPCResult(w, req.ID, a2a.TaskFromLedger(tk, events, 0))
}

// ---- streaming (message/stream + tasks/resubscribe) ----

func (s *Server) a2aMessageStream(w http.ResponseWriter, r *http.Request, req a2a.Request) {
	var p a2a.SendMessageParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPCError(w, req.ID, a2a.ErrInvalidParams, "invalid params", nil)
		return
	}
	if s.bus == nil {
		writeRPCError(w, req.ID, a2a.ErrUnsupportedOperation, "streaming not wired", nil)
		return
	}
	// subscribe before ingest so we cannot miss the first transition
	events, cancel := s.bus.Subscribe()
	defer cancel()
	t, rerr := s.a2aIngest(r.Context(), p)
	if rerr != nil {
		writeRPCError(w, req.ID, rerr.Code, rerr.Message, rerr.Data)
		return
	}
	s.a2aStream(w, r, req.ID, t.ID, events)
}

func (s *Server) a2aResubscribe(w http.ResponseWriter, r *http.Request, req a2a.Request) {
	var p a2a.TaskIDParams
	if err := json.Unmarshal(req.Params, &p); err != nil || p.ID == "" {
		writeRPCError(w, req.ID, a2a.ErrInvalidParams, "invalid params: id required", nil)
		return
	}
	if s.bus == nil {
		writeRPCError(w, req.ID, a2a.ErrUnsupportedOperation, "streaming not wired", nil)
		return
	}
	if _, _, err := s.ledger.Get(r.Context(), p.ID); err != nil {
		writeRPCError(w, req.ID, a2a.ErrTaskNotFound, "task not found: "+p.ID, nil)
		return
	}
	events, cancel := s.bus.Subscribe()
	defer cancel()
	s.a2aStream(w, r, req.ID, p.ID, events)
}

// a2aStream writes the initial Task snapshot then one JSON-RPC response per
// change as an SSE event, closing after the terminal status-update. Each
// SSE data line is a full JSON-RPC Response whose result is the A2A event.
func (s *Server) a2aStream(w http.ResponseWriter, r *http.Request, id json.RawMessage, taskID string, events <-chan bus.Event) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeRPCError(w, id, a2a.ErrInternal, "streaming unsupported", nil)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	sentArtifacts := map[string]bool{}
	emit := func(result any) bool {
		raw, err := json.Marshal(a2a.Response{JSONRPC: "2.0", ID: id, Result: result})
		if err != nil {
			return true
		}
		if _, err := w.Write([]byte("data: " + string(raw) + "\n\n")); err != nil {
			return false
		}
		fl.Flush()
		return true
	}

	// initial snapshot + any artifacts that already exist
	tk, evs, err := s.ledger.Get(r.Context(), taskID)
	if err != nil {
		writeRPCError(w, id, a2a.ErrInternal, err.Error(), nil)
		return
	}
	snap := a2a.TaskFromLedger(tk, evs, 0)
	for _, a := range snap.Artifacts {
		sentArtifacts[a.ArtifactID] = true
	}
	if !emit(snap) {
		return
	}
	if a2a.StateFromLedger(tk.Status).Terminal() {
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case e, open := <-events:
			if !open {
				return
			}
			if e.Kind != "task_status" || e.Key != taskID {
				continue
			}
			// new artifacts (findings) since last tick
			if _, latest, err := s.ledger.Get(r.Context(), taskID); err == nil {
				a2aTask := a2a.TaskFromLedger(tk, latest, 0)
				for _, a := range a2aTask.Artifacts {
					if sentArtifacts[a.ArtifactID] {
						continue
					}
					sentArtifacts[a.ArtifactID] = true
					if !emit(a2a.ArtifactUpdateEvent{Kind: a2a.KindArtifactUpdate, TaskID: taskID, ContextID: snap.ContextID, Artifact: a, LastChunk: true}) {
						return
					}
				}
			}
			state := a2a.StateFromLedger(e.Data["status"])
			final := state.Terminal()
			if !emit(a2a.StatusUpdateEvent{
				Kind: a2a.KindStatusUpdate, TaskID: taskID, ContextID: snap.ContextID,
				Status: a2a.TaskStatus{State: state, Timestamp: time.Now().UTC().Format(time.RFC3339)},
				Final:  final,
			}) {
				return
			}
			if final {
				return
			}
		}
	}
}

// a2aWaitTerminal blocks until the task reaches a terminal state, the
// timeout elapses, or the request is canceled.
func (s *Server) a2aWaitTerminal(ctx context.Context, taskID string, timeout time.Duration) {
	if s.bus == nil {
		return
	}
	if tk, _, err := s.ledger.Get(ctx, taskID); err == nil && a2a.StateFromLedger(tk.Status).Terminal() {
		return
	}
	events, cancel := s.bus.Subscribe()
	defer cancel()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case e, open := <-events:
			if !open {
				return
			}
			if e.Kind == "task_status" && e.Key == taskID && a2a.StateFromLedger(e.Data["status"]).Terminal() {
				return
			}
		}
	}
}

// ---- helpers ----

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, a2a.Response{JSONRPC: "2.0", ID: id, Result: result})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string, data any) {
	// JSON-RPC transports errors in the body with HTTP 200.
	writeJSON(w, http.StatusOK, a2a.Response{JSONRPC: "2.0", ID: id, Error: &a2a.RPCError{Code: code, Message: msg, Data: data}})
}

func randID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}
