package nativewake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- fake punk server --------------------------------------------------

// fakeMessage mirrors the wire shape; the runner must only use metadata.
type fakeMessage struct {
	Seq         int64  `json:"seq"`
	ID          string `json:"id"`
	Namespace   string `json:"namespace"`
	Sender      string `json:"sender"`
	Recipient   string `json:"recipient"`
	Body        string `json:"body"`
	CreatedAt   string `json:"created_at"`
	AckedAt     string `json:"acked_at,omitempty"`
	LeasedUntil string `json:"leased_until,omitempty"`
}

type fakeDiag struct {
	Agent           string `json:"agent"`
	Client          string `json:"client"`
	DeliveryMode    string `json:"delivery_mode"`
	State           string `json:"state"`
	LastError       string `json:"last_error"`
	NextAttemptAt   string `json:"next_attempt_at"`
	PendingAckCount int    `json:"pending_ack_count"`
	WakeCount       int    `json:"wake_count"`
}

// fakeServer is a real HTTP/SSE server over httptest: the runner talks
// HTTP and parses SSE on the wire, not through stubs. It records every
// route the runner touches so tests can prove the runner never mutates
// message state (no ACK, no release, no leases, no sends).
type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	messages      []fakeMessage // seq ascending, any namespace/state
	seq           int64
	diags         []fakeDiag
	routes        []string // "METHOD path?query" log, in order
	streams       int      // open SSE connections right now
	sseTotal      int
	hintSubs      map[chan struct{}]bool // open SSE hint subscribers
	silence       atomic.Bool            // hold streams open without any frame or ping
	authKey       string
	registrations int
}

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{t: t, hintSubs: map[chan struct{}]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// nsFromPath extracts the namespace segment of /v1/namespaces/<ns>/...
// and returns the remainder ("messages", "messages/log", ...).
func nsFromPath(path string) (ns, rest string, ok bool) {
	const prefix = "/v1/namespaces/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	tail := strings.TrimPrefix(path, prefix)
	i := strings.IndexByte(tail, '/')
	if i <= 0 {
		return "", "", false
	}
	return tail[:i], tail[i+1:], true
}

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.routes = append(f.routes, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	f.mu.Unlock()
	if f.authKey != "" && r.Header.Get("Authorization") != "Bearer "+f.authKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ns, rest, ok := nsFromPath(r.URL.Path)
	if !ok {
		http.Error(w, "no such route", http.StatusNotFound)
		return
	}
	switch {
	case rest == "members" && r.Method == http.MethodPost:
		f.mu.Lock()
		f.registrations++
		f.mu.Unlock()
		writeTestJSON(w, http.StatusOK, map[string]string{"status": "registered"})
	case rest == "messages/events":
		f.serveSSE(w, r, ns)
	case rest == "messages" && r.Method == http.MethodGet:
		// Read-only inbox: unacked, unleased, recipient-exact, seq ASC.
		agent := r.URL.Query().Get("agent")
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 100 {
			limit = 100
		}
		f.mu.Lock()
		var msgs []fakeMessage
		for _, m := range f.messages {
			if m.Namespace == ns && m.Recipient == agent && m.AckedAt == "" && !fakeLeaseActive(m) {
				msgs = append(msgs, m) // stored seq ascending
				if len(msgs) == limit {
					break
				}
			}
		}
		f.mu.Unlock()
		if msgs == nil {
			msgs = []fakeMessage{}
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"messages": msgs})
	case rest == "messages/count":
		// Unread count includes actively leased rows, like the real route.
		agent := r.URL.Query().Get("agent")
		f.mu.Lock()
		var n int64
		for _, m := range f.messages {
			if m.Namespace == ns && m.Recipient == agent && m.AckedAt == "" {
				n++
			}
		}
		f.mu.Unlock()
		writeTestJSON(w, http.StatusOK, map[string]int64{"unread": n})
	case rest == "messages/log" && r.Method == http.MethodGet:
		// Operator log: every row involving the agent (sender OR
		// recipient), any state, seq DESC, before-cursor pagination.
		agent := r.URL.Query().Get("agent")
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 500 {
			limit = 100
		}
		before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		f.mu.Lock()
		var msgs []fakeMessage
		for i := len(f.messages) - 1; i >= 0; i-- {
			m := f.messages[i]
			if m.Namespace == ns && (m.Recipient == agent || m.Sender == agent) && (before == 0 || m.Seq < before) {
				msgs = append(msgs, m)
				if len(msgs) == limit {
					break
				}
			}
		}
		f.mu.Unlock()
		var nextBefore int64
		if len(msgs) > 0 {
			nextBefore = msgs[len(msgs)-1].Seq
		}
		if msgs == nil {
			msgs = []fakeMessage{}
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"messages": msgs, "next_before": nextBefore})
	case rest == "messages/diagnostics" && r.Method == http.MethodPost:
		var d fakeDiag
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
			http.Error(w, "bad diag", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.diags = append(f.diags, d)
		f.mu.Unlock()
		writeTestJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
	case rest == "messages/ack" || rest == "messages/release" ||
		(rest == "messages" && r.Method != http.MethodGet):
		// Mutation routes the runner must never touch. Answer 200 so the
		// test fails on the route log, not on a retry storm.
		f.t.Errorf("runner hit forbidden mutation route: %s %s", r.Method, r.URL.Path)
		writeTestJSON(w, http.StatusOK, map[string]int64{"n": 0})
	default:
		http.Error(w, "no such route", http.StatusNotFound)
	}
}

func fakeLeaseActive(m fakeMessage) bool {
	if m.LeasedUntil == "" {
		return false
	}
	until, err := time.Parse(time.RFC3339Nano, m.LeasedUntil)
	return err == nil && until.After(time.Now())
}

func (f *fakeServer) serveSSE(w http.ResponseWriter, r *http.Request, ns string) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", http.StatusInternalServerError)
		return
	}
	f.mu.Lock()
	f.streams++
	f.sseTotal++
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.streams--
		f.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl.Flush() // headers reach the client even when the stream stays silent
	if f.silence.Load() {
		<-r.Context().Done()
		return
	}
	hints := make(chan struct{}, 16)
	f.mu.Lock()
	f.hintSubs[hints] = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.hintSubs, hints)
		f.mu.Unlock()
	}()
	// Initial hint, like the real server: reconnecting readers recover
	// unacknowledged messages without any cursor.
	if f.hasUnread(ns, r.URL.Query().Get("agent")) {
		fmt.Fprintf(w, "event: inbox\ndata: {\"agent\":%q}\n\n", r.URL.Query().Get("agent"))
		fl.Flush()
	}
	ping := time.NewTicker(20 * time.Millisecond)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-hints:
			fmt.Fprintf(w, "event: inbox\ndata: {\"agent\":%q}\n\n", r.URL.Query().Get("agent"))
			fl.Flush()
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func writeTestJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeServer) hasUnread(ns, agent string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.messages {
		if m.Namespace == ns && m.Recipient == agent && m.AckedAt == "" {
			return true
		}
	}
	return false
}

// putMsg appends one message and publishes a hint to open streams, like
// the real bus after a durable write.
func (f *fakeServer) putMsg(m fakeMessage) {
	f.mu.Lock()
	f.seq++
	m.Seq = f.seq
	if m.Namespace == "" {
		m.Namespace = "team"
	}
	if m.Recipient == "" {
		m.Recipient = "claude:s1"
	}
	m.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	f.messages = append(f.messages, m)
	for ch := range f.hintSubs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	f.mu.Unlock()
}

func (f *fakeServer) put(id, sender, body string) {
	f.putMsg(fakeMessage{ID: id, Sender: sender, Body: body})
}

// putNS addresses one message in another namespace (recipient stays the
// session address, which is namespace-independent).
func (f *fakeServer) putNS(ns, id, sender, body string) {
	f.putMsg(fakeMessage{ID: id, Namespace: ns, Sender: sender, Body: body})
}

func (f *fakeServer) diagStates() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, d := range f.diags {
		out = append(out, d.State)
	}
	return out
}

func (f *fakeServer) lastDiag() (fakeDiag, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.diags) == 0 {
		return fakeDiag{}, false
	}
	return f.diags[len(f.diags)-1], true
}

func (f *fakeServer) forbiddenHit(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rt := range f.routes {
		if strings.Contains(rt, "/messages/ack") || strings.Contains(rt, "/messages/release") {
			t.Fatalf("runner hit mutation route: %s", rt)
		}
		if strings.HasPrefix(rt, "POST ") && strings.Contains(rt, "/messages?") {
			t.Fatalf("runner sent a message: %s", rt)
		}
		if strings.Contains(rt, "lease_seconds") {
			t.Fatalf("runner requested a lease: %s", rt)
		}
	}
}

func (f *fakeServer) streamCount() (open, total int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streams, f.sseTotal
}

func (f *fakeServer) registrationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registrations
}

// ---- fake transport ----------------------------------------------------

type wakeCall struct {
	Text string
}

type fakeTransport struct {
	mu       sync.Mutex
	probeErr error
	probeFn  func() error // dynamic probe (liveness changes mid-run)
	wakeErr  error        // fixed error for every Wake
	wakeFn   func(call int) (Outcome, error)
	calls    []wakeCall
	closed   bool
	wakeCh   chan struct{}
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{wakeCh: make(chan struct{}, 64)}
}

func (ft *fakeTransport) Probe(context.Context) error {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if ft.probeFn != nil {
		return ft.probeFn()
	}
	return ft.probeErr
}

func (ft *fakeTransport) Wake(_ context.Context, text string) (Outcome, error) {
	ft.mu.Lock()
	ft.calls = append(ft.calls, wakeCall{Text: text})
	n := len(ft.calls)
	err := ft.wakeErr
	fn := ft.wakeFn
	ft.mu.Unlock()
	ft.wakeCh <- struct{}{}
	if fn != nil {
		return fn(n)
	}
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Accepted: true}, nil
}

func (ft *fakeTransport) Close() error {
	ft.mu.Lock()
	ft.closed = true
	ft.mu.Unlock()
	return nil
}

func (ft *fakeTransport) wakeCount() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return len(ft.calls)
}

func (ft *fakeTransport) texts() []string {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	var out []string
	for _, c := range ft.calls {
		out = append(out, c.Text)
	}
	return out
}

func (ft *fakeTransport) isClosed() bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.closed
}

// ---- helpers ------------------------------------------------------------

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// timingOverrides is a test-only partial replacement of timing.
type timingOverrides struct {
	coalesce, diagInterval, reconnectMin, reconnectMax, watchdog time.Duration
	busyRetry, wakeRetry, fetchRetry, controlPoll, requestBounds time.Duration
	livenessProbe, lockWait, lockRetry                           time.Duration
}

// setTestTimings applies overrides and returns a restore function.
func setTestTimings(o timingOverrides) (restore func()) {
	old := timing
	timing = runnerTimings{
		coalesce:      o.coalesce,
		diagInterval:  o.diagInterval,
		reconnectMin:  o.reconnectMin,
		reconnectMax:  o.reconnectMax,
		watchdog:      o.watchdog,
		busyRetry:     o.busyRetry,
		wakeRetry:     o.wakeRetry,
		fetchRetry:    o.fetchRetry,
		controlPoll:   o.controlPoll,
		requestBounds: o.requestBounds,
		livenessProbe: o.livenessProbe,
		lockWait:      o.lockWait,
		lockRetry:     o.lockRetry,
	}
	return func() { timing = old }
}

// fastTiming shortens every runner timing knob for hermetic tests.
func fastTiming(t *testing.T) {
	t.Helper()
	restore := setTestTimings(timingOverrides{
		coalesce:      5 * time.Millisecond,
		diagInterval:  5 * time.Millisecond,
		reconnectMin:  5 * time.Millisecond,
		reconnectMax:  20 * time.Millisecond,
		watchdog:      150 * time.Millisecond,
		busyRetry:     30 * time.Millisecond,
		wakeRetry:     30 * time.Millisecond,
		fetchRetry:    20 * time.Millisecond,
		controlPoll:   10 * time.Millisecond,
		requestBounds: 2 * time.Second,
		livenessProbe: 60 * time.Millisecond,
		lockWait:      300 * time.Millisecond,
		lockRetry:     10 * time.Millisecond,
	})
	t.Cleanup(restore)
}

func testConfig(t *testing.T, url string) Config {
	t.Helper()
	return Config{
		Client:    "claude",
		SessionID: "s1",
		Namespace: "team",
		URL:       url,
		APIKey:    "sekret-test-key",
		StateDir:  t.TempDir(),
		MaxWakes:  5,
		Window:    600 * time.Second,
		Cooldown:  50 * time.Millisecond,
	}
}

func runInBackground(t *testing.T, cfg Config, tr Transport) (cancel context.CancelFunc, errCh chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh = make(chan error, 1)
	done := make(chan struct{})
	go func() {
		errCh <- Run(ctx, cfg, tr)
		close(done)
	}()
	// Always leave a fully-exited Run behind: its goroutines read the
	// shared timing knobs that the next test's cleanup restores.
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cancel, errCh
}

// ---- tests --------------------------------------------------------------

func TestRunHintWakesOnce(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	f.put("m1", "planner", "peer body must never be forwarded")
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	cfg.Cooldown = time.Hour // every later hint sees the same unread set
	cancel, errCh := runInBackground(t, cfg, tr)
	defer cancel()

	waitFor(t, "one wake", func() bool { return tr.wakeCount() == 1 })
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run: %v", err)
	}

	text := tr.texts()[0]
	for _, want := range []string{"team", "claude:s1", "untrusted", "read_messages", "notification"} {
		if !strings.Contains(text, want) {
			t.Fatalf("nudge missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "peer body") || strings.Contains(text, cfg.APIKey) {
		t.Fatalf("nudge leaked peer content or credentials:\n%s", text)
	}
	if !tr.isClosed() {
		t.Fatal("transport not closed on stop")
	}
	f.forbiddenHit(t)
	if f.registrationCount() < 1 {
		t.Fatal("runner never registered its address")
	}
	d, ok := f.lastDiag()
	if !ok {
		t.Fatal("no diagnostics reported")
	}
	if d.DeliveryMode != "idle_wake" || d.Agent != "claude:s1" || d.Client != "claude" {
		t.Fatalf("diag = %+v", d)
	}
	states := f.diagStates()
	if !contains(states, "ready") {
		t.Fatalf("no ready observation in %v", states)
	}
	// No credentials in any diagnostic.
	f.mu.Lock()
	for _, d := range f.diags {
		raw, _ := json.Marshal(d)
		if strings.Contains(string(raw), cfg.APIKey) {
			t.Fatalf("diagnostic leaked credential: %s", raw)
		}
	}
	f.mu.Unlock()
	// No credentials or bodies in persistent state.
	raw, err := os.ReadFile(filepath.Join(stateSubdir(cfg, "claude:s1"), wakeStateFile))
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	if strings.Contains(string(raw), cfg.APIKey) || strings.Contains(string(raw), "peer body") {
		t.Fatalf("state file leaked: %s", raw)
	}
}

func TestRunCoalescesBurst(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	f.put("m1", "planner", "a")
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	// Long cooldown: every hint after the first sees an unchanged inbox.
	cfg.Cooldown = time.Hour
	cancel, _ := runInBackground(t, cfg, tr)
	defer cancel()

	waitFor(t, "first wake", func() bool { return tr.wakeCount() == 1 })
	// Reconnect produces another initial hint for the same unread set.
	f.srv.CloseClientConnections()
	time.Sleep(150 * time.Millisecond)
	if n := tr.wakeCount(); n != 1 {
		t.Fatalf("burst of hints produced %d wakes, want 1", n)
	}
}

func TestRunAllowlistFilters(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	f.put("m1", "stranger", "held back")
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	cfg.SenderPrefixes = []string{"planner"}
	cancel, _ := runInBackground(t, cfg, tr)
	defer cancel()

	waitFor(t, "sender_filtered diagnostic", func() bool {
		return contains(f.diagStates(), "sender_filtered")
	})
	if n := tr.wakeCount(); n != 0 {
		t.Fatalf("woke for a filtered sender (%d wakes)", n)
	}
	// An allowed sender joining the unread set wakes.
	f.put("m2", "planner", "ok")
	waitFor(t, "wake after allowed sender arrives", func() bool { return tr.wakeCount() >= 1 })
	f.forbiddenHit(t)
}

func TestRunSameUnreadCooldown(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	f.put("m1", "planner", "a")
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	cfg.Cooldown = 120 * time.Millisecond
	cancel, _ := runInBackground(t, cfg, tr)
	defer cancel()

	waitFor(t, "first wake", func() bool { return tr.wakeCount() == 1 })
	// Force a reconnect: the fresh initial hint names the same unread set.
	f.srv.CloseClientConnections()
	time.Sleep(40 * time.Millisecond) // within cooldown
	if n := tr.wakeCount(); n != 1 {
		t.Fatalf("unchanged unread re-woke inside cooldown: %d", n)
	}
	// After cooldown the persistent unread may wake again (bounded by budget).
	waitFor(t, "post-cooldown wake", func() bool { return tr.wakeCount() >= 2 })
}

func TestRunBudgetCapAndExpiry(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	cfg.MaxWakes = 2
	cfg.Window = 150 * time.Millisecond
	cfg.Cooldown = time.Millisecond // cooldown never the limiter here
	cancel, _ := runInBackground(t, cfg, tr)
	defer cancel()

	f.put("m1", "planner", "a")
	waitFor(t, "wake 1", func() bool { return tr.wakeCount() >= 1 })
	f.put("m2", "planner", "b")
	waitFor(t, "wake 2", func() bool { return tr.wakeCount() >= 2 })
	f.put("m3", "planner", "c")
	time.Sleep(60 * time.Millisecond) // window not yet expired
	if n := tr.wakeCount(); n != 2 {
		t.Fatalf("cap exceeded: %d wakes", n)
	}
	waitFor(t, "wake_budget_exhausted diagnostic", func() bool {
		return contains(f.diagStates(), "wake_budget_exhausted")
	})
	// Cap expiry reschedules without any new message or hint.
	waitFor(t, "wake after window expiry", func() bool { return tr.wakeCount() >= 3 })
	f.forbiddenHit(t)
}

func TestRunBudgetSurvivesRestart(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	cfg := testConfig(t, f.srv.URL)
	cfg.MaxWakes = 2
	cfg.Window = 200 * time.Millisecond
	cfg.Cooldown = time.Millisecond

	f.put("m1", "planner", "a")
	tr1 := newFakeTransport()
	cancel1, errCh1 := runInBackground(t, cfg, tr1)
	waitFor(t, "wake 1", func() bool { return tr1.wakeCount() >= 1 })
	f.put("m2", "planner", "b")
	waitFor(t, "wake 2", func() bool { return tr1.wakeCount() >= 2 })
	cancel1()
	if err := <-errCh1; err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Daemon restart: same StateDir, fresh process state. The reserved
	// budget survived, so the reconnect hint cannot wake again yet.
	tr2 := newFakeTransport()
	cancel2, _ := runInBackground(t, cfg, tr2)
	defer cancel2()
	time.Sleep(80 * time.Millisecond) // inside the original window
	if n := tr2.wakeCount(); n != 0 {
		t.Fatalf("restart forgot reserved budget: %d wakes", n)
	}
	// Window expiry frees a slot and permits later delivery.
	waitFor(t, "wake after quota expiry", func() bool { return tr2.wakeCount() >= 1 })
}

func TestRunBusyDefers(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	f.put("m1", "planner", "a")
	tr := newFakeTransport()
	var calls atomic.Int64
	tr.wakeFn = func(int) (Outcome, error) {
		if calls.Add(1) == 1 {
			return Outcome{}, ErrBusy
		}
		return Outcome{Accepted: true}, nil
	}
	cfg := testConfig(t, f.srv.URL)
	cancel, _ := runInBackground(t, cfg, tr)
	defer cancel()

	waitFor(t, "waiting_for_idle diagnostic", func() bool {
		return contains(f.diagStates(), "waiting_for_idle")
	})
	// Busy defers without consuming budget and retries without a new hint.
	waitFor(t, "retry succeeds", func() bool { return tr.wakeCount() >= 2 })
	d, ok := f.lastDiag()
	if ok && d.State == "wake_budget_exhausted" {
		t.Fatal("busy consumed wake budget")
	}
}

func TestRunUnavailableStops(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	tr := newFakeTransport()
	tr.probeErr = ErrUnavailable
	cfg := testConfig(t, f.srv.URL)

	err := Run(context.Background(), cfg, tr)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Run = %v, want ErrUnavailable", err)
	}
	waitFor(t, "disabled diagnostic", func() bool {
		return contains(f.diagStates(), "disabled")
	})
	if !tr.isClosed() {
		t.Fatal("transport not closed")
	}
	_, total := f.streamCount()
	if total != 0 {
		t.Fatal("SSE opened despite unavailable target")
	}
}

func TestRunReconnectRecoversUnread(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	// Long cooldown: wake 2 can only come from the reconnect's initial
	// hint naming a CHANGED unread set, never from maintenance re-scans.
	cfg.Cooldown = 10 * time.Second
	cancel, _ := runInBackground(t, cfg, tr)
	defer cancel()

	// Message lands while no stream delivers a hint (dropped silently).
	f.put("m1", "planner", "a")
	waitFor(t, "first wake", func() bool { return tr.wakeCount() == 1 })
	// Drop the stream; a new message's hint is lost with it.
	f.srv.CloseClientConnections()
	f.put("m2", "planner", "b")
	// Reconnect sends an initial hint and the backlog is reconciled.
	waitFor(t, "wake after reconnect", func() bool { return tr.wakeCount() >= 2 })
	_, total := f.streamCount()
	if total < 2 {
		t.Fatalf("streams = %d, want reconnect", total)
	}
}

func TestRunWatchdogReconnectsSilentStream(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	f.silence.Store(true) // stream opens but never frames or pings
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	cancel, _ := runInBackground(t, cfg, tr)
	defer cancel()

	waitFor(t, "watchdog reconnect", func() bool {
		_, total := f.streamCount()
		return total >= 2
	})
}

func TestRunControlMarkerStops(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	cfg.Generation = "gen-7"
	cfg.ControlPath = filepath.Join(t.TempDir(), "control")
	if err := os.WriteFile(cfg.ControlPath, []byte("gen-7"), 0o600); err != nil {
		t.Fatal(err)
	}
	cancel, errCh := runInBackground(t, cfg, tr)
	defer cancel()
	waitFor(t, "stream open", func() bool {
		open, _ := f.streamCount()
		return open == 1
	})

	// Marker content changes: this worker's generation is superseded.
	if err := os.WriteFile(cfg.ControlPath, []byte("gen-8"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Run after marker change = %v", err)
	}
	if !tr.isClosed() {
		t.Fatal("transport not closed on marker stop")
	}
	waitFor(t, "stream closed", func() bool {
		open, _ := f.streamCount()
		return open == 0
	})
}

func TestRunControlMarkerRemoved(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	cfg.Generation = "gen-1"
	cfg.ControlPath = filepath.Join(t.TempDir(), "control")
	if err := os.WriteFile(cfg.ControlPath, []byte("gen-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	cancel, errCh := runInBackground(t, cfg, tr)
	defer cancel()
	waitFor(t, "stream open", func() bool {
		open, _ := f.streamCount()
		return open == 1
	})
	if err := os.Remove(cfg.ControlPath); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Run after marker removal = %v", err)
	}
}

func TestRunWakeUnavailableTerminates(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	f.put("m1", "planner", "a")
	tr := newFakeTransport()
	tr.wakeErr = ErrUnavailable
	cfg := testConfig(t, f.srv.URL)

	err := Run(context.Background(), cfg, tr)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Run = %v, want ErrUnavailable", err)
	}
}

func TestRunDefiniteWakeFailureRetriesBounded(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	f.put("m1", "planner", "a")
	tr := newFakeTransport()
	var calls atomic.Int64
	tr.wakeFn = func(int) (Outcome, error) {
		if calls.Add(1) == 1 {
			return Outcome{}, errors.New("socket write broke")
		}
		return Outcome{Accepted: true}, nil
	}
	cfg := testConfig(t, f.srv.URL)
	cancel, _ := runInBackground(t, cfg, tr)
	defer cancel()

	waitFor(t, "delivery_failed diagnostic", func() bool {
		return contains(f.diagStates(), "delivery_failed")
	})
	waitFor(t, "retry wakes", func() bool { return tr.wakeCount() == 2 })
	// The ambiguous first write consumed budget (conservative accounting):
	// two attempts are reserved even though only one handoff succeeded.
	raw, err := os.ReadFile(filepath.Join(stateSubdir(cfg, "claude:s1"), wakeStateFile))
	if err != nil {
		t.Fatal(err)
	}
	var st wakeState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Attempts) != 2 {
		t.Fatalf("attempts = %v, want 2 reserved", st.Attempts)
	}
}

func TestRunConfigValidation(t *testing.T) {
	good := testConfig(t, "http://127.0.0.1:1")
	bads := []Config{
		{Client: "", SessionID: "s1", Namespace: "team", URL: "http://x", StateDir: t.TempDir()},
		{Client: "Claude Bad", SessionID: "s1", Namespace: "team", URL: "http://x", StateDir: t.TempDir()},
		{Client: "claude", SessionID: "", Namespace: "team", URL: "http://x", StateDir: t.TempDir()},
		{Client: "claude", SessionID: "s 1", Namespace: "team", URL: "http://x", StateDir: t.TempDir()},
		{Client: "claude", SessionID: "s1", Namespace: "", URL: "http://x", StateDir: t.TempDir()},
		{Client: "claude", SessionID: "s1", Namespace: "te:am", URL: "http://x", StateDir: t.TempDir()},
		{Client: "claude", SessionID: "s1", Namespace: "te/am", URL: "http://x", StateDir: t.TempDir()},
		{Client: "claude", SessionID: "s1", Namespace: "team", URL: "", StateDir: t.TempDir()},
	}
	for i, cfg := range bads {
		if err := Run(context.Background(), cfg, newFakeTransport()); err == nil {
			t.Fatalf("case %d: invalid config accepted", i)
		}
	}
	// Valid config against a dead server retries but stays cancellable.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = Run(ctx, good, newFakeTransport())
}

func TestRunContextCancelCleanup(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	cancel, errCh := runInBackground(t, cfg, tr)
	waitFor(t, "stream open", func() bool {
		open, _ := f.streamCount()
		return open == 1
	})
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run on cancel = %v", err)
	}
	if !tr.isClosed() {
		t.Fatal("transport not closed")
	}
	waitFor(t, "stream closed", func() bool {
		open, _ := f.streamCount()
		return open == 0
	})
}

func TestRunUnconfirmedHandoff(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	f.put("m1", "planner", "a")
	tr := newFakeTransport()
	tr.wakeFn = func(int) (Outcome, error) {
		return Outcome{Accepted: true, Unconfirmed: true}, nil
	}
	cfg := testConfig(t, f.srv.URL)
	cancel, _ := runInBackground(t, cfg, tr)
	defer cancel()

	waitFor(t, "handoff_unconfirmed diagnostic", func() bool {
		return contains(f.diagStates(), "handoff_unconfirmed")
	})
}

func TestRunRegistersExactAddress(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	f.authKey = "sekret-test-key"
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	cancel, _ := runInBackground(t, cfg, tr)
	defer cancel()
	waitFor(t, "registration", func() bool { return f.registrationCount() == 1 })
	f.forbiddenHit(t)
}

func TestRunNilTransport(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1")
	if err := Run(context.Background(), cfg, nil); err == nil {
		t.Fatal("nil transport must error, not panic")
	}
}

func TestRunWakesDisabledStillMonitors(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	f.put("m1", "planner", "a")
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	cfg.MaxWakes = 0 // explicit zero: monitor + report, never wake
	cancel, _ := runInBackground(t, cfg, tr)
	defer cancel()

	waitFor(t, "disabled diagnostic", func() bool {
		return contains(f.diagStates(), "disabled")
	})
	waitFor(t, "stream open", func() bool {
		open, _ := f.streamCount()
		return open >= 1
	})
	time.Sleep(50 * time.Millisecond)
	if n := tr.wakeCount(); n != 0 {
		t.Fatalf("disabled config woke %d times", n)
	}
	d, ok := f.lastDiag()
	if ok && d.LastError != "wakes_disabled" {
		t.Fatalf("diag last_error = %q", d.LastError)
	}
}

func TestRunLivenessProbeStopsDeadHost(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t) // inbox stays EMPTY the whole run
	tr := newFakeTransport()
	var dead atomic.Bool
	tr.probeFn = func() error {
		if dead.Load() {
			return ErrUnavailable
		}
		return nil
	}
	cfg := testConfig(t, f.srv.URL)
	cancel, errCh := runInBackground(t, cfg, tr)
	defer cancel()
	waitFor(t, "stream open", func() bool {
		open, _ := f.streamCount()
		return open >= 1
	})

	dead.Store(true) // host dies without firing any SessionEnd hook
	if err := <-errCh; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Run = %v, want ErrUnavailable", err)
	}
	if !tr.isClosed() {
		t.Fatal("transport not closed after host death")
	}
	waitFor(t, "stream closed", func() bool {
		open, _ := f.streamCount()
		return open == 0
	})
}

func TestRunStaleGenerationNeverHandsOff(t *testing.T) {
	fastTiming(t)
	// Background poll effectively disabled: only the SYNCHRONOUS
	// pre-handoff check can stop this worker.
	defer setTestTimings(timingOverrides{
		coalesce: 5 * time.Millisecond, diagInterval: 5 * time.Millisecond,
		reconnectMin: 5 * time.Millisecond, reconnectMax: 20 * time.Millisecond,
		watchdog: 150 * time.Millisecond, busyRetry: 30 * time.Millisecond,
		wakeRetry: 30 * time.Millisecond, fetchRetry: 20 * time.Millisecond,
		controlPoll: time.Hour, requestBounds: 2 * time.Second,
		livenessProbe: time.Hour,
	})()
	f := newFakeServer(t)
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	cfg.Generation = "gen-1"
	cfg.ControlPath = filepath.Join(t.TempDir(), "control")
	if err := os.WriteFile(cfg.ControlPath, []byte("gen-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	cancel, errCh := runInBackground(t, cfg, tr)
	defer cancel()
	waitFor(t, "stream open", func() bool {
		open, _ := f.streamCount()
		return open == 1
	})

	// Superseded between hint and handoff; then mail arrives.
	if err := os.WriteFile(cfg.ControlPath, []byte("gen-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.put("m1", "planner", "a")
	if err := <-errCh; err != nil {
		t.Fatalf("Run = %v, want clean stop", err)
	}
	if n := tr.wakeCount(); n != 0 {
		t.Fatalf("stale generation woke %d times", n)
	}
	// The quota ledger must not carry a reservation from a stale worker.
	raw, err := os.ReadFile(filepath.Join(stateSubdir(cfg, "claude:s1"), wakeStateFile))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err == nil {
		var st wakeState
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatal(err)
		}
		if len(st.Attempts) != 0 {
			t.Fatalf("stale worker reserved attempts: %v", st.Attempts)
		}
	}
}

func TestRunWrongGenerationAtStartIsInert(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	cfg.Generation = "gen-9"
	cfg.ControlPath = filepath.Join(t.TempDir(), "control")
	if err := os.WriteFile(cfg.ControlPath, []byte("gen-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), cfg, tr); err != nil {
		t.Fatalf("Run = %v, want nil clean stop", err)
	}
	if f.registrationCount() != 0 {
		t.Fatal("stale worker registered")
	}
	if !tr.isClosed() {
		t.Fatal("transport not closed")
	}
}

func TestRunStateDirLockConflict(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	cfg := testConfig(t, f.srv.URL)
	tr1 := newFakeTransport()
	cancel, _ := runInBackground(t, cfg, tr1)
	defer cancel()
	waitFor(t, "first worker registered", func() bool { return f.registrationCount() >= 1 })

	// A second Run on the same StateDir (overlapping generation) fails
	// immediately instead of double-spending the quota.
	tr2 := newFakeTransport()
	err := Run(context.Background(), cfg, tr2)
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("second Run = %v, want lock conflict", err)
	}
	if !tr2.isClosed() {
		t.Fatal("loser's transport not closed")
	}
}

func TestRunLockWaitSurvivesReplacement(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	cfg := testConfig(t, f.srv.URL)
	tr1 := newFakeTransport()
	cancel1, _ := runInBackground(t, cfg, tr1)
	waitFor(t, "first worker registered", func() bool { return f.registrationCount() >= 1 })

	// Replacement worker starts while the previous one still holds the
	// lock: bounded wait, then clean acquisition after the exit.
	tr2 := newFakeTransport()
	cancel2, errCh2 := runInBackground(t, cfg, tr2)
	defer cancel2()
	time.Sleep(30 * time.Millisecond) // tr2 is inside the lock wait
	select {
	case err := <-errCh2:
		t.Fatalf("replacement failed during lock wait: %v", err)
	default:
	}
	cancel1()
	f.put("m1", "planner", "a")
	waitFor(t, "replacement registered", func() bool { return f.registrationCount() >= 2 })
	waitFor(t, "replacement wakes", func() bool { return tr2.wakeCount() == 1 })
}

func TestRunNamespaceBudgetIsolation(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	cfgA := testConfig(t, f.srv.URL)
	cfgA.MaxWakes = 1
	cfgA.Window = 400 * time.Millisecond
	cfgA.Cooldown = time.Millisecond
	cfgB := cfgA
	cfgB.Namespace = "other"

	// A: one wake exhausts A's whole budget.
	f.putNS("team", "a1", "planner", "x")
	trA1 := newFakeTransport()
	cancelA1, errChA1 := runInBackground(t, cfgA, trA1)
	waitFor(t, "A wake", func() bool { return trA1.wakeCount() == 1 })
	cancelA1()
	if err := <-errChA1; err != nil {
		t.Fatalf("A run: %v", err)
	}
	// B: same StateDir, different namespace - fresh budget, A's intact.
	f.putNS("other", "b1", "planner", "x")
	trB := newFakeTransport()
	cancelB, errChB := runInBackground(t, cfgB, trB)
	waitFor(t, "B wake", func() bool { return trB.wakeCount() == 1 })
	cancelB()
	if err := <-errChB; err != nil {
		t.Fatalf("B run: %v", err)
	}
	// A again: the reconnect hint meets the preserved exhausted budget.
	trA2 := newFakeTransport()
	cancelA2, _ := runInBackground(t, cfgA, trA2)
	defer cancelA2()
	waitFor(t, "A budget exhaustion observed", func() bool {
		d, ok := f.lastDiag()
		return ok && d.State == "wake_budget_exhausted"
	})
	time.Sleep(60 * time.Millisecond)
	if n := trA2.wakeCount(); n != 0 {
		t.Fatalf("returning to namespace A reset its budget: %d wakes", n)
	}
}

func TestRunAllowlistStarvationLogScan(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	// A full inbox page of denied senders hides newer allowed mail.
	for i := 0; i < 100; i++ {
		f.put("denied-"+strconv.Itoa(i), "stranger", "held")
	}
	f.put("allowed-1", "planner", "wake for this")
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	cfg.SenderPrefixes = []string{"planner"}
	cancel, _ := runInBackground(t, cfg, tr)
	defer cancel()

	waitFor(t, "starved allowed mail wakes via log scan", func() bool { return tr.wakeCount() >= 1 })
	f.mu.Lock()
	var logHits int
	for _, rt := range f.routes {
		if strings.Contains(rt, "/messages/log") {
			logHits++
		}
	}
	f.mu.Unlock()
	if logHits == 0 {
		t.Fatal("starvation scan never paged the operator log")
	}
	if d, ok := f.lastDiag(); ok && d.State == "sender_filtered" {
		t.Fatal("allowed mail was found but still reported filtered")
	}
	f.forbiddenHit(t)
}

func TestRunLeasedUnreadIsNotSenderFiltered(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	// Actively leased to an inbox hook: unread, but already being
	// delivered - the listener must not wake and must not blame filtering.
	f.putMsg(fakeMessage{ID: "m1", Sender: "planner", Body: "in flight",
		LeasedUntil: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	cancel, _ := runInBackground(t, cfg, tr)
	defer cancel()

	waitFor(t, "waiting_for_next_prompt diagnostic", func() bool {
		return contains(f.diagStates(), "waiting_for_next_prompt")
	})
	time.Sleep(50 * time.Millisecond)
	if n := tr.wakeCount(); n != 0 {
		t.Fatalf("woke for leased mail: %d", n)
	}
	if contains(f.diagStates(), "sender_filtered") {
		t.Fatal("leased-in-flight mail misreported as sender_filtered")
	}
}

func TestRunLogScanBudgetLimit(t *testing.T) {
	fastTiming(t)
	f := newFakeServer(t)
	// More denied mail than the scan budget: the listener reports the
	// limit instead of paging forever.
	for i := 0; i < 1001; i++ {
		f.put("denied-"+strconv.Itoa(i), "stranger", "held")
	}
	tr := newFakeTransport()
	cfg := testConfig(t, f.srv.URL)
	cfg.SenderPrefixes = []string{"planner"}
	cancel, _ := runInBackground(t, cfg, tr)
	defer cancel()

	waitFor(t, "scan_limit diagnostic", func() bool {
		d, ok := f.lastDiag()
		return ok && d.LastError == "scan_limit"
	})
	if d, _ := f.lastDiag(); d.State != "sender_filtered" {
		t.Fatalf("state = %q, want sender_filtered with scan_limit", d.State)
	}
	if n := tr.wakeCount(); n != 0 {
		t.Fatalf("woke for denied mail: %d", n)
	}
	f.forbiddenHit(t)
}

func TestNudgeTextNoPeerData(t *testing.T) {
	n := nudgeText("team", "claude:s1", 3)
	if !strings.Contains(n, "3") || !strings.Contains(n, "team") || !strings.Contains(n, "claude:s1") {
		t.Fatalf("nudge = %q", n)
	}
	for _, want := range []string{"notification", "untrusted", "read_messages"} {
		if !strings.Contains(n, want) {
			t.Fatalf("nudge missing %q: %q", want, n)
		}
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
