package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/bus"
)

// C08: MCP task waits must complete below client deadlines. Read-only tool
// history showed await_tasks calls failing at 59.9s because the client
// cancels at 60s while the server happily waits longer (default 60s, max
// 300s). These tests pin the conservative 45s default, the preserved
// explicit waits and cap, and the cancellation cleanup, over a disposable
// StreamableHTTP server (never the live coordination server).

// clientDeadline is the observed tool-call deadline of the affected MCP
// clients (calls failed at 59.927-59.987s), used as the bound every
// omitted-timeout wait must stay under.
const clientDeadline = 60 * time.Second

// TestAwaitTasksDefaultLeavesClientDeadlineMargin is the deterministic
// red proof: an omitted timeout_seconds normalizes to awaitDefault, and a
// 60s default leaves zero margin for the response to travel under a 60s
// client deadline. The default must be 45s, the server max must stay 300s
// for clients that genuinely support longer waits.
func TestAwaitTasksDefaultLeavesClientDeadlineMargin(t *testing.T) {
	if awaitDefault != 45*time.Second {
		t.Fatalf("awaitDefault = %v, want 45s: an omitted timeout_seconds normalizes to this value, and %v leaves no response margin under a %v client deadline", awaitDefault, awaitDefault, clientDeadline)
	}
	if margin := clientDeadline - awaitDefault; margin < 15*time.Second {
		t.Fatalf("default wait %v leaves only %v under the %v client deadline, want at least 15s for the board response to arrive", awaitDefault, margin, clientDeadline)
	}
	if awaitMax != 300*time.Second {
		t.Fatalf("awaitMax = %v, want 300s preserved for clients that support longer waits", awaitMax)
	}
}

// TestAwaitTimeoutNormalization pins the omitted-timeout default and the
// preserved explicit waits deterministically: no sleeping, just the
// normalization the await_tasks handler feeds to taskboard.WaitForChange.
func TestAwaitTimeoutNormalization(t *testing.T) {
	cases := []struct {
		seconds int
		want    time.Duration
	}{
		// omitted or nonsense -> the conservative default
		{0, 45 * time.Second},
		{-5, 45 * time.Second},
		// explicit waits survive, including longer ones for clients that
		// genuinely support them, capped at the server maximum
		{1, 1 * time.Second},
		{45, 45 * time.Second},
		{55, 55 * time.Second},
		{299, 299 * time.Second},
		{300, 300 * time.Second},
		{301, 300 * time.Second},
		{100000, 300 * time.Second},
	}
	for _, c := range cases {
		if got := awaitTimeout(c.seconds); got != c.want {
			t.Fatalf("awaitTimeout(%d) = %v, want %v", c.seconds, got, c.want)
		}
	}
}

// httpCall records one served request: the StreamableHTTP handler returns
// from ServeHTTP only when the tool call it carries has completed, so an
// in-flight await is observable from the outside as a request that spans
// the cancellation instant.
type httpCall struct{ start, end time.Time }

// boardGate wraps the MCP HTTP handler and records when each request
// started and finished.
type boardGate struct {
	mu    sync.Mutex
	calls []httpCall
}

func (g *boardGate) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		g.mu.Lock()
		g.calls = append(g.calls, httpCall{start: start, end: time.Now()})
		g.mu.Unlock()
	})
}

// finishedInFlight reports whether a request that started before cut and
// was still running at cut finished by endBy.
func (g *boardGate) finishedInFlight(cut, endBy time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.calls {
		if c.start.Before(cut) && !c.end.Before(cut) && !c.end.After(endBy) {
			return true
		}
	}
	return false
}

func (g *boardGate) dump() []httpCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]httpCall(nil), g.calls...)
}

// streamableBoardSession mounts the real server (with a live bus, so
// await_tasks genuinely waits) on a disposable httptest StreamableHTTP
// endpoint and connects a real client to it.
func streamableBoardSession(t *testing.T, tweaks ...func(*Deps)) (*mcp.ClientSession, *bus.Bus, *boardGate) {
	t.Helper()
	deps, _ := newTestDeps(t)
	b := bus.New()
	deps.Bus = b
	for _, tweak := range tweaks {
		tweak(&deps)
	}
	srv := New(deps)
	g := &boardGate{}
	ts := httptest.NewServer(g.wrap(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)))
	t.Cleanup(ts.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, b, g
}

// TestAwaitTasksNoChangeWaitCompletesBelowClientDeadline is the C08
// acceptance proof over a real transport: with nothing changing under
// /tasks, an await_tasks call that omits timeout_seconds must return the
// unchanged board strictly below the 60s client deadline. Before C08 this
// wait ran the full 60s default and the response could not arrive in time.
func TestAwaitTasksNoChangeWaitCompletesBelowClientDeadline(t *testing.T) {
	cs, _, _ := streamableBoardSession(t)
	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "remember",
		Arguments: map[string]any{"namespace": "ns", "key": "/tasks/A", "body": "first"}})
	if err != nil || res.IsError {
		t.Fatalf("remember: %v %s", err, text(t, res))
	}

	start := time.Now()
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "await_tasks", Arguments: map[string]any{"namespace": "ns"}})
	elapsed := time.Since(start)
	if err != nil || res.IsError {
		t.Fatalf("await_tasks: %v %s", err, text(t, res))
	}
	var out struct {
		Changed bool     `json:"changed"`
		Changes []string `json:"changes"`
		Next    string   `json:"next"`
	}
	if err := json.Unmarshal([]byte(text(t, res)), &out); err != nil {
		t.Fatalf("decode %q: %v", text(t, res), err)
	}
	if out.Changed || len(out.Changes) != 0 || out.Next != "A" {
		t.Fatalf("no-change wait = %+v, want the unchanged board", out)
	}
	t.Logf("no-change wait with omitted timeout returned after %v", elapsed)
	if elapsed >= clientDeadline {
		t.Fatalf("no-change wait took %v, at or past the %v client deadline: the omitted timeout normalizes to %v and the response cannot arrive in time", elapsed, clientDeadline, awaitDefault)
	}
	if elapsed < 40*time.Second {
		t.Fatalf("no-change wait returned after %v, want the full default wait of about %v", elapsed, awaitDefault)
	}
}

// TestAwaitTasksCancellationFreesWaiterAndSession is the short real
// transport cancellation proof: cancel an in-flight await after a few
// hundred milliseconds; the client call must return promptly, the server
// must stop serving that request within moments (the waiter does not sit
// out the remaining default wait), and the same session must keep working
// - a subsequent list_tasks returns the board.
func TestAwaitTasksCancellationFreesWaiterAndSession(t *testing.T) {
	cs, _, gate := streamableBoardSession(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "await_tasks", Arguments: map[string]any{"namespace": "ns"}})
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	cut := time.Now()
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled await_tasks returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled await_tasks did not return to the client within 5s")
	}

	// The await's POST was in flight at the cancellation instant; it must
	// finish within moments, proving the server-side wait was aborted and
	// its bus subscription released instead of running out the default.
	by := cut.Add(5 * time.Second)
	freed := false
	for !freed && time.Now().Before(by) {
		freed = gate.finishedInFlight(cut, by)
		if !freed {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !freed {
		t.Fatalf("server kept the cancelled await request open past 5s; served calls: %v", gate.dump())
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_tasks", Arguments: map[string]any{"namespace": "ns"}})
	if err != nil || res.IsError {
		t.Fatalf("list_tasks after cancellation: %v %s", err, text(t, res))
	}
}
