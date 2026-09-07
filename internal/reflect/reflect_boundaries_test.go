package reflect

// R02 correction-round regressions, folded permanently from the
// reviewer's reproduced preflight failures (/reviews/R02/preflight1-
// boundaries, /reviews/R02/preflight2-fanout) plus the fanout resource
// bounds they require. Each test is red-proof against the unpatched
// draft: a Deadline that never reaches the model context, a model error
// that discards gathered evidence, an expansion failure that leaves
// unshown facts citable, and high-degree fanout that bypasses every
// outer cap.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
)

// contextClient honors its ctx like a compliant provider: a blocked
// call wakes on ctx.Done() and reports whether cancellation arrived.
type contextClient struct {
	blocked  time.Duration
	canceled bool
}

func (c *contextClient) Chat(ctx context.Context, _ []llm.Turn, _ []llm.Tool) (*llm.Result, error) {
	select {
	case <-ctx.Done():
		c.canceled = true
		return nil, ctx.Err()
	case <-time.After(c.blocked):
		return &llm.Result{ToolCalls: []llm.ToolCall{{ID: "c1", Name: toolDone, Args: json.RawMessage(`{"answer":""}`)}}, PromptTokens: 10, CompletionTokens: 5}, nil
	}
}
func (c *contextClient) Model() string { return "context-client" }

// TestDeadlineCancelsInFlightModelCall: Opts.Deadline propagates as a
// context timeout, so a compliant blocking Chat stops at the cap
// instead of running past the caller's bound.
func TestDeadlineCancelsInFlightModelCall(t *testing.T) {
	c := &contextClient{blocked: 200 * time.Millisecond}
	_, err := New(newTestStore(t), c).ReflectWith(t.Context(), "ns", "q", Opts{ExpandEvidence: true, Deadline: 20 * time.Millisecond})
	if err == nil {
		t.Fatal("deadline expiry returned nil error, want the context error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if !c.canceled {
		t.Fatal("configured Deadline did not cancel the model context; compliant call outlived the cap")
	}
}

// TestModelErrorPreservesShownEvidence: a provider failure after a
// successful evidence round must NOT discard the gathered evidence,
// shown bodies, round records or measured usage - spent work is never
// dropped with the error.
func TestModelErrorPreservesShownEvidence(t *testing.T) {
	s := newTestStore(t)
	f, err := s.Write(t.Context(), memory.WriteInput{Namespace: "ns", Key: "/db", Body: "postgres database"})
	if err != nil {
		t.Fatal(err)
	}
	fail := errors.New("provider failed")
	c := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "recall", Name: toolRecall, Args: json.RawMessage(`{"query":"postgres"}`)}})
		},
		func([]llm.Turn) (*llm.Result, error) { return nil, fail },
	}}
	ans, err := New(s, c).ReflectWith(t.Context(), "ns", "postgres", Opts{ExpandEvidence: true})
	if !errors.Is(err, fail) {
		t.Fatalf("err = %v, want the provider failure", err)
	}
	if len(ans.Evidence) != 1 || ans.Evidence[0] != f.ID || len(ans.ShownFacts) != 1 || len(ans.Rounds) != 1 {
		t.Fatalf("provider error discarded prior shown evidence/rounds: evidence=%v facts=%d rounds=%d", ans.Evidence, len(ans.ShownFacts), len(ans.Rounds))
	}
	if ans.StopReason != stopModelError {
		t.Fatalf("stop reason = %q, want %q", ans.StopReason, stopModelError)
	}
}

// TestFailedExpansionDoesNotAuthorizeUnshownFacts: execExpand must not
// register facts into the evidence set before its whole payload
// succeeds. Here the first query retrieves a fact, the second hits an
// entity-lineage cycle and the call fails, so the model receives only
// error text - the retrieved-but-unshown fact must stay uncitable, and
// the run must carry no evidence at all.
func TestFailedExpansionDoesNotAuthorizeUnshownFacts(t *testing.T) {
	s := newTestStore(t)
	f, err := s.Write(t.Context(), memory.WriteInput{Namespace: "ns", Key: "/db", Body: "postgres database"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range [][2]string{{"/entities/service/a", "/entities/service/b"}, {"/entities/service/b", "/entities/service/a"}} {
		if _, err := s.Write(t.Context(), memory.WriteInput{Namespace: "ns", Key: p[0], Body: "alias", Attributes: map[string]any{"merged_into": p[1]}}); err != nil {
			t.Fatal(err)
		}
	}
	done, _ := json.Marshal(map[string]any{"answer": "database", "citations": []string{f.ID}})
	shown := false
	c := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "expand", Name: toolExpand, Args: json.RawMessage(`{"queries":["postgres","/entities/service/a"]}`)}})
		},
		func(turns []llm.Turn) (*llm.Result, error) {
			last := turns[len(turns)-1]
			if last.Role != "tool" {
				t.Fatal("expected tool result")
			}
			var payload struct {
				Facts []memory.Fact `json:"facts"`
			}
			if json.Unmarshal([]byte(last.Content), &payload) == nil {
				for _, fact := range payload.Facts {
					if fact.ID == f.ID {
						shown = true
					}
				}
			}
			return result([]llm.ToolCall{{ID: "done", Name: toolDone, Args: done}})
		},
	}}
	ans, err := New(s, c).ReflectWith(t.Context(), "ns", "postgres", Opts{ExpandEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if !shown && (len(ans.Citations) != 0 || len(ans.Evidence) != 0) {
		t.Fatalf("failed expansion returned no fact payload, yet unshown facts are citable: citations=%v evidence=%v", ans.Citations, ans.Evidence)
	}
}

// fanoutStore builds a hub key with the given degree, every neighbor
// body ~9KB, so unbounded enumeration would serialize far past any
// sane per-call output budget as degree grows.
func fanoutStore(t *testing.T, degree int) *memory.Store {
	t.Helper()
	s := newTestStore(t)
	ctx := t.Context()
	for i := 0; i <= degree; i++ {
		key := fmt.Sprintf("/nodes/%03d", i)
		if _, err := s.Write(ctx, memory.WriteInput{Namespace: "ns", Key: key, Body: strings.Repeat("evidence ", 1024)}); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			if err := s.AddLink(ctx, "ns", "/nodes/000", key, "relates_to"); err != nil {
				t.Fatal(err)
			}
		}
	}
	return s
}

// TestExpandFanoutBoundedAsDegreeGrows is the fanout red proof: the
// old draft enumerated every incident neighbor and serialized every
// body (degree 64 returned 65 facts / ~616KB). With the resource
// bounds, the returned payload stays within the per-call fact/byte
// caps at EVERY degree - the bound does not grow with the graph -
// truncation is reported explicitly, and only payload facts become
// citable. Synthetic fixture, mechanism evidence only.
func TestExpandFanoutBoundedAsDegreeGrows(t *testing.T) {
	for _, degree := range []int{8, 64} {
		t.Run(fmt.Sprint(degree), func(t *testing.T) {
			s := fanoutStore(t, degree)
			exp := newExpansion(Opts{ExpandEvidence: true}, 4)
			args, _ := json.Marshal(map[string]any{"keys": []string{"/nodes/000"}})
			out, err := New(s, nil).execExpand(t.Context(), "ns", llm.ToolCall{ID: "e1", Name: toolExpand, Args: args}, map[string]memory.Fact{}, exp)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) > expandMaxBytes {
				t.Fatalf("degree %d: payload = %d bytes, want <= %d", degree, len(out), expandMaxBytes)
			}
			var payload struct {
				Facts       []memory.Fact `json:"facts"`
				Relations   []string      `json:"relations"`
				Truncated   bool          `json:"truncated"`
				TruncatedBy string        `json:"truncated_by"`
			}
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload.Facts) > expandMaxFacts {
				t.Fatalf("degree %d: %d facts returned, want <= %d", degree, len(payload.Facts), expandMaxFacts)
			}
			if len(payload.Relations) > expandMaxRelations {
				t.Fatalf("degree %d: %d relations returned, want <= %d", degree, len(payload.Relations), expandMaxRelations)
			}
			if !payload.Truncated || payload.TruncatedBy == "" {
				t.Fatalf("degree %d: truncation not reported in payload (truncated=%v by=%q)", degree, payload.Truncated, payload.TruncatedBy)
			}
			if exp.truncated == "" {
				t.Fatalf("degree %d: round metadata lost the truncation bound", degree)
			}
			// Only shown facts authorize citations: exactly the payload
			// facts may be registered into the evidence set.
			if len(payload.Facts) != exp.factsShown {
				t.Fatalf("degree %d: %d facts shown but %d registered citable", degree, len(payload.Facts), exp.factsShown)
			}
		})
	}
}

// TestExpandRunBudgetStopsRun pins the run-wide totals: repeated maxed
// expansions against distinct hubs accumulate toward
// expandRunMaxBytes, and once a run-wide bound (not just the per-call
// bound) stopped a call, the run ends with the partial answer and stop
// reason expansion-budget, with no further model call.
func TestExpandRunBudgetStopsRun(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	// Four hubs x 16 leaves x ~9KB bodies: each expand of one hub
	// returns ~3 facts (~29KB) against the 32KB per-call byte cap, so
	// the 96KB run cap binds by the fourth call.
	hubs := []string{"/nodes/000", "/nodes/100", "/nodes/200", "/nodes/300"}
	for h, hub := range hubs {
		for i := 0; i <= 16; i++ {
			key := fmt.Sprintf("%s/%03d", hub, i)
			if _, err := s.Write(ctx, memory.WriteInput{Namespace: "acme", Key: key, Body: strings.Repeat("evidence ", 1024)}); err != nil {
				t.Fatal(err)
			}
			if i > 0 {
				if err := s.AddLink(ctx, "acme", hub, key, "relates_to"); err != nil {
					t.Fatal(err)
				}
			}
		}
		_ = h
	}
	script := &scriptedLLM{t: t}
	for i, hub := range hubs {
		hub, id := hub, fmt.Sprintf("c%d", i)
		script.queue = append(script.queue, func([]llm.Turn) (*llm.Result, error) {
			args, _ := json.Marshal(map[string]any{"keys": []string{hub}})
			return result([]llm.ToolCall{{ID: id, Name: toolExpand, Args: args}})
		})
	}
	// One spare response: if the run budget binds early the model is
	// never called again, and scriptedLLM fails only on overrun.
	script.queue = append(script.queue, func([]llm.Turn) (*llm.Result, error) {
		return result([]llm.ToolCall{{ID: "spare", Name: toolExpand, Args: json.RawMessage(`{"keys":["/nodes/000"]}`)}})
	})

	ans, err := New(s, script).ReflectWith(ctx, "acme", "q", Opts{ExpandEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if ans.StopReason != stopExpandBudget {
		t.Fatalf("stop reason = %q, want %q (rounds: %+v)", ans.StopReason, stopExpandBudget, ans.Rounds)
	}
	if script.calls != len(ans.Rounds) {
		t.Fatalf("model calls = %d, rounds = %d: the budget stop must save the spare call", script.calls, len(ans.Rounds))
	}
	last := ans.Rounds[len(ans.Rounds)-1]
	if !last.Truncated || last.TruncatedBy != "bytes" {
		t.Fatalf("last round truncated=%v by=%q, want the run-wide byte bound", last.Truncated, last.TruncatedBy)
	}
}

// TestCanceledDuringChatStopsBeforePendingTools pins the cancellation
// checks between operations: a client that cancels its own context and
// returns multiple tool calls must be stopped before ANY pending tool
// executes, with the explicit canceled reason on the partial answer.
type cancelingClient struct {
	cancel context.CancelFunc
	calls  int
}

func (c *cancelingClient) Chat(_ context.Context, _ []llm.Turn, _ []llm.Tool) (*llm.Result, error) {
	c.calls++
	if c.calls == 1 {
		c.cancel()
		args1, _ := json.Marshal(map[string]any{"query": "postgres"})
		args2, _ := json.Marshal(map[string]any{"query": "unrelated"})
		return &llm.Result{ToolCalls: []llm.ToolCall{
			{ID: "t1", Name: toolRecall, Args: args1},
			{ID: "t2", Name: toolRecall, Args: args2},
		}, PromptTokens: 10, CompletionTokens: 5}, nil
	}
	return result([]llm.ToolCall{{ID: "done", Name: toolDone, Args: json.RawMessage(`{"answer":"x"}`)}})
}
func (c *cancelingClient) Model() string { return "canceling" }

func TestCanceledDuringChatStopsBeforePendingTools(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Write(t.Context(), memory.WriteInput{Namespace: "acme", Key: "/db", Body: "postgres database"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ans, err := New(s, &cancelingClient{cancel: cancel}).ReflectWith(ctx, "acme", "q", Opts{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if ans.StopReason != stopCanceled {
		t.Fatalf("stop reason = %q, want %q", ans.StopReason, stopCanceled)
	}
	if len(ans.Evidence) != 0 {
		t.Fatalf("evidence = %v, want none: cancellation must land before the pending tools execute", ans.Evidence)
	}
}

// TestExpandCanceledInsideRetrievalLoops pins the context checks inside
// the expansion retrieval loops: with the context already canceled, an
// expand call must fail fast (before and during key/neighbor reads),
// register nothing into the evidence set, and flag the round errored.
func TestExpandCanceledInsideRetrievalLoops(t *testing.T) {
	s := fanoutStore(t, 4)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // already dead: every ctx check must fire

	exp := newExpansion(Opts{ExpandEvidence: true}, 4)
	args, _ := json.Marshal(map[string]any{"keys": []string{"/nodes/000"}})
	seen := map[string]memory.Fact{}
	_, err := New(s, nil).execExpand(ctx, "ns", llm.ToolCall{ID: "e1", Name: toolExpand, Args: args}, seen, exp)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !exp.expandErr {
		t.Fatal("canceled expansion did not flag the round errored")
	}
	if len(seen) != 0 || exp.factsShown != 0 {
		t.Fatalf("canceled expansion registered evidence: seen=%v factsShown=%d", seen, exp.factsShown)
	}
}

// TestPartialToolBatchRetainsRound (reviewer red proof, folded): a tool
// cap firing mid-batch must finalize the started round exactly once -
// the first recall's evidence, measured tokens and round record
// survive; the skipped second call claims nothing.
func TestPartialToolBatchRetainsRound(t *testing.T) {
	s := newTestStore(t)
	f, err := s.Write(t.Context(), memory.WriteInput{Namespace: "ns", Key: "/db", Body: "postgres database"})
	if err != nil {
		t.Fatal(err)
	}
	c := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) {
			return &llm.Result{PromptTokens: 9, CompletionTokens: 1, ToolCalls: []llm.ToolCall{
				{ID: "first", Name: toolRecall, Args: json.RawMessage(`{"query":"postgres"}`)},
				{ID: "second", Name: toolRecall, Args: json.RawMessage(`{"query":"postgres"}`)},
			}}, nil
		},
	}}
	ans, err := New(s, c).ReflectWith(t.Context(), "ns", "postgres", Opts{ExpandEvidence: true, MaxToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ans.StopReason != stopMaxToolCalls || len(ans.Evidence) != 1 || ans.Evidence[0] != f.ID {
		t.Fatalf("wrong stop/evidence: stop=%q evidence=%v", ans.StopReason, ans.Evidence)
	}
	if len(ans.Rounds) != 1 || len(ans.Rounds[0].EvidenceAdded) != 1 || ans.Rounds[0].EvidenceAdded[0] != f.ID || ans.Rounds[0].Tokens != 10 {
		t.Fatalf("partial tool batch dropped completed work from round ledger: tokens=%d rounds=%+v", ans.Tokens, ans.Rounds)
	}
}

// TestDoneWithOtherToolsRetainsRound: a done call sharing a response
// with an already-executed tool finalizes that round's record too - the
// answer's ledger carries the tool work the model actually saw.
func TestDoneWithOtherToolsRetainsRound(t *testing.T) {
	s := newTestStore(t)
	f, err := s.Write(t.Context(), memory.WriteInput{Namespace: "ns", Key: "/db", Body: "postgres database"})
	if err != nil {
		t.Fatal(err)
	}
	done, _ := json.Marshal(map[string]any{"answer": "database", "citations": []string{f.ID}})
	c := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) {
			return &llm.Result{PromptTokens: 9, CompletionTokens: 1, ToolCalls: []llm.ToolCall{
				{ID: "recall", Name: toolRecall, Args: json.RawMessage(`{"query":"postgres"}`)},
				{ID: "done", Name: toolDone, Args: done},
			}}, nil
		},
	}}
	ans, err := New(s, c).ReflectWith(t.Context(), "ns", "postgres", Opts{ExpandEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if ans.StopReason != "" || len(ans.Citations) != 1 || ans.Citations[0] != f.ID {
		t.Fatalf("answer = %+v, want the done call's answer with the valid citation", ans)
	}
	if len(ans.Rounds) != 1 || len(ans.Rounds[0].EvidenceAdded) != 1 || ans.Rounds[0].Tokens != 10 {
		t.Fatalf("done-with-tools round lost the executed recall: rounds=%+v", ans.Rounds)
	}
}

// TestExpandOversizedSingleBodySkippedWhole: a fact whose serialized
// form exceeds the per-call byte cap is skipped ENTIRELY (bodies are
// what citations resolve against - never cut mid-body), the truncation
// metadata names the byte bound, and the skipped fact is not citable
// while a small neighbor still is.
func TestExpandOversizedSingleBodySkippedWhole(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	big, err := s.Write(ctx, memory.WriteInput{Namespace: "ns", Key: "/hub", Body: strings.Repeat("x", 2*expandMaxBytes)})
	if err != nil {
		t.Fatal(err)
	}
	small, err := s.Write(ctx, memory.WriteInput{Namespace: "ns", Key: "/node/small", Body: "small neighbor fact"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddLink(ctx, "ns", "/hub", small.Key, "relates_to"); err != nil {
		t.Fatal(err)
	}

	exp := newExpansion(Opts{ExpandEvidence: true}, 4)
	args, _ := json.Marshal(map[string]any{"keys": []string{"/hub"}})
	seen := map[string]memory.Fact{}
	out, err := New(s, nil).execExpand(ctx, "ns", llm.ToolCall{ID: "e1", Name: toolExpand, Args: args}, seen, exp)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > expandMaxBytes {
		t.Fatalf("payload = %d bytes, want <= %d", len(out), expandMaxBytes)
	}
	var payload struct {
		Facts       []memory.Fact `json:"facts"`
		Truncated   bool          `json:"truncated"`
		TruncatedBy string        `json:"truncated_by"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Truncated || payload.TruncatedBy != "bytes" {
		t.Fatalf("truncated=%v by=%q, want the byte bound named", payload.Truncated, payload.TruncatedBy)
	}
	for _, f := range payload.Facts {
		if f.ID == big.ID {
			t.Fatal("oversized fact serialized into the payload; want skipped whole")
		}
	}
	if _, citable := seen[big.ID]; citable {
		t.Fatal("oversized fact registered citable despite never being shown")
	}
	shownSmall := false
	for _, f := range payload.Facts {
		if f.ID == small.ID {
			shownSmall = true
		}
	}
	if !shownSmall || exp.factsShown != 1 {
		t.Fatalf("small neighbor fact missing from payload: shown=%v factsShown=%d", shownSmall, exp.factsShown)
	}
}

// TestExpansionRoundTokensMeasuredNotEstimated pins the usage-honesty
// contract: a round's Tokens field carries only MEASURED provider usage.
// A response reporting zero usage records zero (counted in
// UsageUnknown), never a conservative estimate - no token preflight is
// claimed from missing usage. The byte budgets bound PAYLOAD size and
// are never mixed into token accounting.
func TestExpansionRoundTokensMeasuredNotEstimated(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Write(t.Context(), memory.WriteInput{Namespace: "ns", Key: "/db", Body: "postgres database"}); err != nil {
		t.Fatal(err)
	}
	c := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		// Round 1: no usage reported - measured zero, counted unknown.
		func([]llm.Turn) (*llm.Result, error) {
			return &llm.Result{ToolCalls: []llm.ToolCall{{ID: "r1", Name: toolRecall, Args: json.RawMessage(`{"query":"postgres"}`)}}}, nil
		},
		// Round 2: real measured usage.
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "r2", Name: toolRecall, Args: json.RawMessage(`{"query":"postgres"}`)}})
		},
		// Round 3: done.
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "d1", Name: toolDone, Args: json.RawMessage(`{"answer":"database"}`)}})
		},
	}}
	ans, err := New(s, c).ReflectWith(t.Context(), "ns", "postgres", Opts{ExpandEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Rounds) != 3 {
		t.Fatalf("rounds = %d, want 3", len(ans.Rounds))
	}
	if ans.Rounds[0].Tokens != 0 {
		t.Fatalf("unreported-usage round tokens = %d, want 0 (measured zero, never estimated)", ans.Rounds[0].Tokens)
	}
	if ans.Rounds[1].Tokens != 15 || ans.Rounds[2].Tokens != 15 {
		t.Fatalf("measured rounds = %d/%d, want 15/15", ans.Rounds[1].Tokens, ans.Rounds[2].Tokens)
	}
	if ans.UsageUnknown != 1 {
		t.Fatalf("UsageUnknown = %d, want 1 (the unreported call)", ans.UsageUnknown)
	}
}

// TestRunBudgetStopsWithinToolBatch (reviewer red proof, folded): one
// model response carrying 256 expand calls - each pulling a ~30KB fact -
// must stop retrieving INSIDE the batch once the run-wide byte budget is
// spent (roughly the fourth call), not after the batch ends. Skipped
// calls record nothing; the round finalizes with exactly the work that
// executed; the model is never called again.
func TestRunBudgetStopsWithinToolBatch(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Write(t.Context(), memory.WriteInput{Namespace: "ns", Key: "/budget", Body: strings.Repeat("x", 30000)}); err != nil {
		t.Fatal(err)
	}
	calls := make([]llm.ToolCall, 256)
	for i := range calls {
		calls[i] = llm.ToolCall{ID: fmt.Sprint(i), Name: toolExpand, Args: json.RawMessage(`{"keys":["/budget"]}`)}
	}
	script := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) {
			return &llm.Result{ToolCalls: calls, PromptTokens: 9, CompletionTokens: 1}, nil
		},
		func([]llm.Turn) (*llm.Result, error) {
			t.Fatal("model called again after the run budget stopped the batch")
			return nil, nil
		},
	}}
	ans, err := New(s, script).ReflectWith(t.Context(), "ns", "budget", Opts{ExpandEvidence: true, MaxRounds: 256, MaxToolCalls: 256})
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Rounds) != 1 {
		t.Fatalf("rounds = %d, want 1", len(ans.Rounds))
	}
	if n := len(ans.Rounds[0].Keys); n > 4 {
		t.Fatalf("executed %d expansions after run budget should stop at fourth call; stop=%s", n, ans.StopReason)
	}
	if ans.StopReason != stopExpandBudget {
		t.Fatalf("stop reason = %q, want %q", ans.StopReason, stopExpandBudget)
	}
	if script.calls != 1 {
		t.Fatalf("model called %d times, want 1 (the budget stop saves the next call)", script.calls)
	}
	// Skipped calls claim nothing: the round's recorded keys equal the
	// expansions that actually executed, and each recorded key was
	// really queried.
	for _, k := range ans.Rounds[0].Keys {
		if k != "/budget" {
			t.Fatalf("round records unexpected key %q", k)
		}
	}
	if len(ans.Rounds[0].EvidenceAdded) == 0 {
		t.Fatalf("round lost the evidence the executed calls gathered: %+v", ans.Rounds[0])
	}
}
