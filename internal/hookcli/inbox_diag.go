package hookcli

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Subprocess delivery diagnostics (shared HTTP contract in
// docs/superpowers/specs/2026-09-28-messaging-reliability-design.md).
//
// After its address is registered, one hook invocation reports ONE
// best-effort observation to POST /v1/namespaces/<ns>/messages/diagnostics,
// from a deferred flush that runs after the ACK outcome is known, so the
// reported state is always the final one and no report ever precedes an
// ACK. The report describes the hook's own observation: it never claims
// model receipt or completion, never changes stdout bytes, ACK timing or
// ordering, and a failed report never fails delivery. The POST runs on
// its own short context (inboxDiagTimeout), so it adds at most that much
// latency to one hook invocation and never borrows a (possibly expired)
// wait deadline. Old servers answer 404 (errNotSupported) and are left
// silent. A hook that never confirmed registration reports nothing: the
// server would reject the snapshot, and an unregistered address has no
// honest delivery state to describe.

// inboxDiagTimeout bounds the one diagnostic POST per invocation; a var
// so tests can shorten it.
var inboxDiagTimeout = 200 * time.Millisecond

// inboxDiagnostic is the POST body, field for field the shared contract
// (region.MessageDiagnosticInput). Empty optional fields clear the
// server's previous values.
type inboxDiagnostic struct {
	Agent           string `json:"agent"`
	Client          string `json:"client"`
	DeliveryMode    string `json:"delivery_mode"`
	State           string `json:"state"`
	LastAttemptAt   string `json:"last_attempt_at,omitempty"`
	NextAttemptAt   string `json:"next_attempt_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	PendingAckCount int    `json:"pending_ack_count,omitempty"`
	WakeCount       int    `json:"wake_count,omitempty"`
}

// reportDiagnostic POSTs one snapshot. errNotSupported (a pre-diagnostics
// server) is returned for the caller to silence; every other error is a
// generic failure the caller may note without payload details.
func (a inboxAPI) reportDiagnostic(d inboxDiagnostic) error {
	ctx, cancel := context.WithTimeout(context.Background(), inboxDiagTimeout)
	defer cancel()
	return inboxDo(ctx, http.MethodPost, a.nsURL("/messages/diagnostics"), a.key, d, nil)
}

// diagDeliveryMode advertises the client's capability: a documented
// continuation contract is hook_continuation, everything else catch_up.
// idle_wake never appears here: it belongs to the native wake listener's
// own reports (internal/nativewake runner), not to the hook's delivery
// observation, and a requested wake listener never changes the hook's
// own delivery mode.
func (r *inboxRun) diagDeliveryMode() string {
	if r.c.AllowContinue {
		return "hook_continuation"
	}
	return "catch_up"
}

// armDiagnostic starts the per-invocation observation. It is called only
// after membership is confirmed (registered now or in a previous hook),
// with the wake count being the continuation slots currently inside the
// sliding window. The default state is the honest resting state of every
// subprocess client: delivery resumes on the next prompt.
func (r *inboxRun) armDiagnostic(wakeCount int) {
	r.diag = &inboxDiagnostic{
		Agent:         r.api.address,
		Client:        r.c.Name,
		DeliveryMode:  r.diagDeliveryMode(),
		State:         "waiting_for_next_prompt",
		LastAttemptAt: inboxNow().UTC().Format(time.RFC3339),
		WakeCount:     wakeCount,
	}
}

// diagOutcome records the invocation's final observation; the last call
// before flush wins. state must be one of the contract's allowed words
// and lastError a short machine reason (never a raw error, which could
// carry URLs or bodies).
func (r *inboxRun) diagOutcome(state, lastError string, pendingAck int) {
	if r.diag == nil {
		return
	}
	r.diag.State = state
	r.diag.LastError = lastError
	r.diag.PendingAckCount = pendingAck
	r.diag.LastAttemptAt = inboxNow().UTC().Format(time.RFC3339)
}

// diagWakeUsed counts one freshly reserved continuation slot.
func (r *inboxRun) diagWakeUsed() {
	if r.diag != nil {
		r.diag.WakeCount++
	}
}

// diagWakeRefund takes the count back when a reserved slot was
// successfully refunded (the continuation never reached the client).
func (r *inboxRun) diagWakeRefund() {
	if r.diag != nil && r.diag.WakeCount > 0 {
		r.diag.WakeCount--
	}
}

// flushDiagnostic sends the armed observation, best effort. A server
// without the route stays silent; any other failure is one generic
// stderr note - the raw error can embed the server URL, which the
// diagnostics contract bans from reports, so it never reaches stderr
// from here either. Delivery, stdout and ACK state are already settled
// when this runs (deferred from deliver), so it can change nothing but
// the server's diagnostic snapshot.
func (r *inboxRun) flushDiagnostic() {
	if r.diag == nil || r.diag.State == "" {
		return
	}
	if err := r.api.reportDiagnostic(*r.diag); err != nil && !errors.Is(err, errNotSupported) {
		r.note("diagnostic report failed (delivery and ACK state are unaffected)")
	}
}
