package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestAwaitLimiterCapsAndReleases unit-tests the slot bookkeeping: refusal
// at the cap, independent keys, reuse after release, and an over-release
// that must not widen the cap.
func TestAwaitLimiterCapsAndReleases(t *testing.T) {
	l := newAwaitLimiter(2)
	if !l.acquire("alice") || !l.acquire("alice") {
		t.Fatal("the first two acquires must succeed")
	}
	if l.acquire("alice") {
		t.Fatal("a third acquire must be refused at cap 2")
	}
	if !l.acquire("bob") {
		t.Fatal("a different caller must be independent")
	}
	l.release("alice")
	if !l.acquire("alice") {
		t.Fatal("a released slot must be reusable")
	}
	l.release("alice")
	l.release("alice")
	l.release("alice") // over-release: must be a no-op
	if !l.acquire("alice") || !l.acquire("alice") {
		t.Fatal("cap must be intact after an over-release")
	}
	if l.acquire("alice") {
		t.Fatal("over-release must not have widened the cap")
	}
}

// TestAwaitMessagesConcurrencyCap proves the await_messages tool is wired to
// the per-caller cap: with a cap of 1, two concurrent waits from the same
// caller cannot both hold a slot - exactly one is refused - and once both
// return a fresh wait is admitted again (the slots are released, not spent).
//
// Two concurrent calls rather than a "start one, then start another" pair,
// because the second form races the first call's goroutine to the limiter
// and can refuse the wrong one; this form is order-independent.
func TestAwaitMessagesConcurrencyCap(t *testing.T) {
	cs := messagingSession(t, func(d *Deps) {
		d.Bus = bus.New()
		d.AwaitLimit = 1
	})
	registerMessageMembers(t, cs, "ns", "one", "two")

	type callResult struct {
		res *mcp.CallToolResult
		err error
	}
	results := make(chan callResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "await_messages",
				Arguments: map[string]any{"namespace": "ns", "agent": "two", "timeout_seconds": 1},
			})
			results <- callResult{res, err}
		}()
	}

	var refused, admitted int
	for i := 0; i < 2; i++ {
		r := <-results
		switch {
		case r.err != nil:
			if strings.Contains(r.err.Error(), "too many concurrent") {
				refused++
			} else {
				t.Fatalf("unexpected await error: %v", r.err)
			}
		case r.res != nil && r.res.IsError:
			body := text(t, r.res)
			if strings.Contains(body, "too many concurrent") {
				refused++
			} else {
				t.Fatalf("unexpected await tool error: %s", body)
			}
		default:
			admitted++
		}
	}
	if refused != 1 || admitted != 1 {
		t.Fatalf("cap 1 with two concurrent waits: got %d refused, %d admitted; want exactly one of each", refused, admitted)
	}

	// Both waits have returned, so both slots are free: a fresh wait must be
	// admitted rather than permanently refused.
	out, denied, otherErr := call(t, cs, "await_messages", map[string]any{"namespace": "ns", "agent": "two", "timeout_seconds": 1})
	if denied || otherErr {
		t.Fatalf("await after the cap drained was refused: %q", out)
	}
}
