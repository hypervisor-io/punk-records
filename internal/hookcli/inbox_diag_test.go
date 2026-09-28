package hookcli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// diagFake is a minimal messaging HTTP surface for the subprocess
// diagnostics tests: namespace lookup, members, unread read, ack,
// release, and the shared diagnostics route. It records request order so
// tests can prove the diagnostic POST always follows the ACK outcome,
// and it can play a pre-diagnostics server (diag404) or a slow one
// (diagSleep). Kept separate from fakeInbox so these tests never edit
// the existing contract fixtures.
type diagFake struct {
	t         *testing.T
	mu        sync.Mutex
	msgs      []InboxMessage
	acked     map[string]bool
	members   map[string]bool
	diags     []inboxDiagnostic
	requests  []string
	failAck   bool
	failFetch bool
	noMembers bool
	diag404   bool
	diagSleep time.Duration
}

func newDiagFake(t *testing.T) (*diagFake, *httptest.Server) {
	f := &diagFake{t: t, acked: map[string]bool{}, members: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *diagFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	switch {
	case r.URL.Path == "/v1/agent/namespace":
		_ = json.NewEncoder(w).Encode(map[string]string{"namespace": "ns1"})
	case strings.HasSuffix(r.URL.Path, "/members") && r.Method == http.MethodPost:
		if f.noMembers {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		var in struct{ Agent string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.members[in.Agent] = true
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "registered"})
	case strings.HasSuffix(r.URL.Path, "/messages/diagnostics") && r.Method == http.MethodPost:
		if f.diag404 {
			http.NotFound(w, r)
			return
		}
		if f.diagSleep > 0 {
			time.Sleep(f.diagSleep)
		}
		var d inboxDiagnostic
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.diags = append(f.diags, d)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "recorded"})
	case strings.HasSuffix(r.URL.Path, "/messages/ack"):
		if f.failAck {
			http.Error(w, "ack broken", http.StatusInternalServerError)
			return
		}
		var in fakeAck
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		for _, id := range in.IDs {
			f.acked[id] = true
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]int{"acked": len(in.IDs)})
	case strings.HasSuffix(r.URL.Path, "/messages/release"):
		_ = json.NewEncoder(w).Encode(map[string]int{"released": 0})
	case strings.HasSuffix(r.URL.Path, "/messages") && r.Method == http.MethodGet:
		if f.failFetch {
			http.Error(w, "read broken", http.StatusInternalServerError)
			return
		}
		q := r.URL.Query()
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []InboxMessage{}
		for _, m := range f.msgs {
			if m.Recipient == q.Get("agent") && !f.acked[m.ID] {
				out = append(out, m)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": out})
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func (f *diagFake) onlyDiag(t *testing.T) inboxDiagnostic {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.diags) != 1 {
		t.Fatalf("expected exactly one diagnostic report, got %d", len(f.diags))
	}
	return f.diags[0]
}

func (f *diagFake) diagCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.diags)
}

// requestOrder returns the index of the first request whose path ends
// with suffix, -1 when absent.
func (f *diagFake) requestOrder(suffix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.requests {
		if strings.HasSuffix(strings.SplitN(r, " ", 2)[1], suffix) {
			return i
		}
	}
	return -1
}

func diagOpts(srv *httptest.Server, mode string) InboxOpts {
	return InboxOpts{Client: "fake", Mode: mode, BaseURL: srv.URL, Namespace: "ns1", Enabled: true}
}

func TestInboxDiagSuccessfulDeliveryReportsWaitingForNextPrompt(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newDiagFake(t)
	f.msgs = append(f.msgs, msg("m1", "other:x", "hello"))
	fakeReplyClient(t, "fake", true, "{}")

	out, _ := runInbox(t, diagOpts(srv, "context"), fakeStdin)
	if !strings.Contains(out, "[PUNK INBOX]") {
		t.Fatalf("delivery stdout changed: %q", out)
	}
	d := f.onlyDiag(t)
	if d.Agent != "fake:s1" || d.Client != "fake" {
		t.Fatalf("diagnostic identity = %q/%q, want fake:s1/fake", d.Agent, d.Client)
	}
	if d.DeliveryMode != "hook_continuation" {
		t.Fatalf("delivery_mode = %q, want hook_continuation (AllowContinue client)", d.DeliveryMode)
	}
	if d.State != "waiting_for_next_prompt" {
		t.Fatalf("state = %q, want waiting_for_next_prompt", d.State)
	}
	if d.LastError != "" || d.PendingAckCount != 0 {
		t.Fatalf("clean delivery carried failure fields: %+v", d)
	}
	if _, err := time.Parse(time.RFC3339, d.LastAttemptAt); err != nil {
		t.Fatalf("last_attempt_at %q is not RFC3339: %v", d.LastAttemptAt, err)
	}
	if ack, diag := f.requestOrder("/messages/ack"), f.requestOrder("/messages/diagnostics"); ack < 0 || diag < 0 || diag < ack {
		t.Fatalf("diagnostic must follow the ACK outcome: ack at %d, diagnostic at %d", ack, diag)
	}
}

func TestInboxDiagCatchUpCapabilityWhenNoContinuation(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newDiagFake(t)
	fakeReplyClient(t, "fake", false, "{}") // no continuation contract
	runInbox(t, diagOpts(srv, "context"), fakeStdin)
	if d := f.onlyDiag(t); d.DeliveryMode != "catch_up" {
		t.Fatalf("delivery_mode = %q, want catch_up", d.DeliveryMode)
	}
}

func TestInboxDiagEmptyInboxReportsWaitingForNextPrompt(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newDiagFake(t)
	fakeReplyClient(t, "fake", true, "{}")
	runInbox(t, diagOpts(srv, "context"), fakeStdin)
	if d := f.onlyDiag(t); d.State != "waiting_for_next_prompt" {
		t.Fatalf("state = %q, want waiting_for_next_prompt", d.State)
	}
}

func TestInboxDiagSenderFilteredWhenAllowlistHoldsBack(t *testing.T) {
	inboxTestEnv(t)
	t.Setenv("PUNK_MESSAGING_FROM", "allowed")
	f, srv := newDiagFake(t)
	f.msgs = append(f.msgs, msg("m1", "stranger:x", "held"))
	fakeReplyClient(t, "fake", true, "{}")
	runInbox(t, diagOpts(srv, "context"), fakeStdin)
	d := f.onlyDiag(t)
	if d.State != "sender_filtered" {
		t.Fatalf("state = %q, want sender_filtered", d.State)
	}
	if d.LastError != "" {
		t.Fatalf("sender filtering is not an error, got last_error %q", d.LastError)
	}
}

func TestInboxDiagAckFailureReportsHandoffUnconfirmed(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newDiagFake(t)
	f.failAck = true
	f.msgs = append(f.msgs, msg("m1", "other:x", "hello"))
	fakeReplyClient(t, "fake", true, "{}")

	out, errw := runInbox(t, diagOpts(srv, "context"), fakeStdin)
	if !strings.Contains(out, "[PUNK INBOX]") {
		t.Fatalf("ACK failure must not suppress the printed delivery: %q", out)
	}
	if !strings.Contains(errw, "ack 1 message(s)") {
		t.Fatalf("existing ACK-failure note missing from stderr: %q", errw)
	}
	d := f.onlyDiag(t)
	if d.State != "handoff_unconfirmed" || d.LastError != "ack_failed" || d.PendingAckCount != 1 {
		t.Fatalf("diagnostic = %+v, want handoff_unconfirmed/ack_failed/pending 1", d)
	}
	if ack, diag := f.requestOrder("/messages/ack"), f.requestOrder("/messages/diagnostics"); diag < ack {
		t.Fatalf("diagnostic must follow the failed ACK: ack at %d, diagnostic at %d", ack, diag)
	}
}

// decliningClient prints its reply but never places the delivery in a
// consumed field, so nothing is ACKed.
func decliningClient(t *testing.T) {
	t.Helper()
	withInboxClient(t, InboxClient{
		Name:          "fake",
		Parse:         parseSessionIDPayload,
		AllowContinue: true,
		Reply: func(req InboxReplyRequest) InboxReply {
			return InboxReply{Out: []byte("{}\n"), Delivered: false}
		},
	})
}

func TestInboxDiagDeclinedHandoffReportsDeliveryFailed(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newDiagFake(t)
	f.msgs = append(f.msgs, msg("m1", "other:x", "hello"))
	decliningClient(t)
	runInbox(t, diagOpts(srv, "context"), fakeStdin)
	d := f.onlyDiag(t)
	if d.State != "delivery_failed" || d.LastError != "handoff_failed" {
		t.Fatalf("diagnostic = %+v, want delivery_failed/handoff_failed", d)
	}
}

func TestInboxDiagCapExhaustedHandoffReportsWakeBudget(t *testing.T) {
	inboxTestEnv(t)
	t.Setenv("PUNK_MESSAGING_MAX_CONTINUE", "0") // cap admits nothing
	f, srv := newDiagFake(t)
	f.msgs = append(f.msgs, msg("m1", "other:x", "hello"))
	decliningClient(t)
	runInbox(t, diagOpts(srv, "continue"), fakeStdin)
	d := f.onlyDiag(t)
	if d.State != "wake_budget_exhausted" || d.LastError != "continuation_cap" {
		t.Fatalf("diagnostic = %+v, want wake_budget_exhausted/continuation_cap", d)
	}
}

func TestInboxDiagRefundedContinuationNotCounted(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newDiagFake(t)
	f.msgs = append(f.msgs, msg("m1", "other:x", "hello"))
	decliningClient(t) // reserves a slot, declines the handoff, slot is refunded
	runInbox(t, diagOpts(srv, "continue"), fakeStdin)
	d := f.onlyDiag(t)
	if d.State != "delivery_failed" || d.LastError != "handoff_failed" {
		t.Fatalf("diagnostic = %+v, want delivery_failed/handoff_failed", d)
	}
	if d.WakeCount != 0 {
		t.Fatalf("refunded continuation still counted: wake_count=%d, want 0", d.WakeCount)
	}
}

func TestInboxDiagFetchFailureReportsDeliveryFailed(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newDiagFake(t)
	f.failFetch = true
	fakeReplyClient(t, "fake", true, "{}")
	runInbox(t, diagOpts(srv, "context"), fakeStdin)
	d := f.onlyDiag(t)
	if d.State != "delivery_failed" || d.LastError != "fetch_failed" {
		t.Fatalf("diagnostic = %+v, want delivery_failed/fetch_failed", d)
	}
}

func TestInboxDiagOldServerStaysSilentAndDelivers(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newDiagFake(t)
	f.diag404 = true // pre-diagnostics server
	f.msgs = append(f.msgs, msg("m1", "other:x", "hello"))
	fakeReplyClient(t, "fake", true, "{}")

	out, errw := runInbox(t, diagOpts(srv, "context"), fakeStdin)
	if !strings.Contains(out, "[PUNK INBOX]") || !f.acked["m1"] {
		t.Fatalf("a 404 diagnostics route must not change delivery: out=%q acked=%v", out, f.acked)
	}
	if strings.Contains(errw, "diagnostic") {
		t.Fatalf("old servers must not produce diagnostic noise: %q", errw)
	}
}

func TestInboxDiagReportTimeoutFailsOpen(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newDiagFake(t)
	f.diagSleep = 300 * time.Millisecond
	f.msgs = append(f.msgs, msg("m1", "other:x", "hello"))
	fakeReplyClient(t, "fake", true, "{}")
	prev := inboxDiagTimeout
	inboxDiagTimeout = 50 * time.Millisecond
	t.Cleanup(func() { inboxDiagTimeout = prev })

	out, errw := runInbox(t, diagOpts(srv, "context"), fakeStdin)
	if !strings.Contains(out, "[PUNK INBOX]") || !f.acked["m1"] {
		t.Fatalf("a slow diagnostics route must not change delivery: out=%q acked=%v", out, f.acked)
	}
	if !strings.Contains(errw, "diagnostic report failed") {
		t.Fatalf("expected a generic diagnostic failure note, stderr: %q", errw)
	}
}

func TestInboxDiagNoReportWithoutRegistration(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newDiagFake(t)
	f.noMembers = true // registration refused
	fakeReplyClient(t, "fake", true, "{}")
	runInbox(t, diagOpts(srv, "context"), fakeStdin)
	if n := f.diagCount(); n != 0 {
		t.Fatalf("an unregistered hook reported %d diagnostics; none are honest", n)
	}
}

func TestInboxDiagCanCarrySkipReportsNothing(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newDiagFake(t)
	withInboxClient(t, InboxClient{
		Name:          "fake",
		Parse:         parseSessionIDPayload,
		AllowContinue: true,
		CanCarry:      func(InboxPayload, bool) bool { return false },
		Reply:         func(InboxReplyRequest) InboxReply { return InboxReply{Out: []byte("{}\n")} },
	})
	runInbox(t, diagOpts(srv, "context"), fakeStdin)
	if n := f.diagCount(); n != 0 {
		t.Fatalf("a CanCarry skip reported %d diagnostics; the path stays request-free", n)
	}
	if got := f.requestOrder("/members"); got >= 0 {
		t.Fatalf("CanCarry skip must not register/fetch/lease; saw members request at %d", got)
	}
}

func TestInboxDiagDisabledMakesNoRequests(t *testing.T) {
	inboxTestEnv(t)
	t.Setenv("PUNK_MESSAGING", "0") // kill switch, even over --messaging
	f, srv := newDiagFake(t)
	fakeReplyClient(t, "fake", true, "{}")
	out, _ := runInbox(t, diagOpts(srv, "context"), fakeStdin)
	if strings.Contains(out, "PUNK INBOX") {
		t.Fatalf("disabled hook delivered: %q", out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 0 {
		t.Fatalf("disabled hook made requests: %v", f.requests)
	}
}

func TestInboxDiagTokensMatchServerValidation(t *testing.T) {
	// The server's region.checkDiagToken admits only short lowercase
	// machine tokens; pin every token this hook can emit against the same
	// shape so a report can never 400 on its own fields.
	tokens := map[string]string{
		"client fake":             "fake",
		"client claude-code":      "claude-code",
		"mode hook_continuation":  "hook_continuation",
		"mode catch_up":           "catch_up",
		"reason ack_failed":       "ack_failed",
		"reason fetch_failed":     "fetch_failed",
		"reason handoff_failed":   "handoff_failed",
		"reason render_cap":       "render_cap",
		"reason continuation_cap": "continuation_cap",
	}
	extra := "._-_:-" // union of the client and last_error punctuation sets
	for name, tok := range tokens {
		for i := 0; i < len(tok); i++ {
			c := tok[i]
			ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || (i > 0 && strings.IndexByte(extra, c) >= 0)
			if !ok {
				t.Fatalf("%s: byte %q of %q fails server token validation", name, c, tok)
			}
		}
		if n := len(tok); n > 64 {
			t.Fatalf("%s: %d bytes exceeds the 64-byte server bound", name, n)
		}
	}
	if strconv.IntSize < 32 { // keep the compiler honest about int use above
		t.Fatal("unexpected platform")
	}
}
