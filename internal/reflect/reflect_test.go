package reflect

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// scriptedLLM returns queued results in order (modeled on
// internal/agent/runtime_test.go's scripted fake client).
type scriptedLLM struct {
	t     *testing.T
	queue []func(turns []llm.Turn) (*llm.Result, error)
	calls int
}

func (s *scriptedLLM) Chat(_ context.Context, turns []llm.Turn, _ []llm.Tool) (*llm.Result, error) {
	if s.calls >= len(s.queue) {
		s.t.Fatalf("scripted LLM exhausted after %d calls", s.calls)
	}
	fn := s.queue[s.calls]
	s.calls++
	return fn(turns)
}
func (s *scriptedLLM) Model() string { return "scripted" }

func newTestStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "reflect.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	return memory.New(db, nil)
}

func result(toolCalls []llm.ToolCall) (*llm.Result, error) {
	return &llm.Result{ToolCalls: toolCalls, PromptTokens: 10, CompletionTokens: 5}, nil
}

func TestReflectValidatesCitationsAndDropsFabricated(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	model, err := s.RememberModel(ctx, "acme", "db-topology", "postgres is primary, redis is cache", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, memory.WriteInput{
		Namespace: "acme", Key: "/observations/db-incidents", Body: "connection pool exhausted twice this week",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, memory.WriteInput{
		Namespace: "acme", Key: "/raw/incident-42", Body: "pool size hit max at 14:02 during deploy",
	}); err != nil {
		t.Fatal(err)
	}

	fabricatedID := "not-a-real-id-0000"
	script := &scriptedLLM{t: t}
	script.queue = []func([]llm.Turn) (*llm.Result, error){
		// round 1: check mental models
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c1", Name: toolListModels, Args: json.RawMessage(`{}`)}})
		},
		// round 2: drop to raw recall
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c2", Name: toolRecall, Args: json.RawMessage(`{"query":"connection pool"}`)}})
		},
		// round 3: done, citing one real model ID and one fabricated ID
		func([]llm.Turn) (*llm.Result, error) {
			args, _ := json.Marshal(map[string]any{
				"answer":    "postgres is primary; the pool has been exhausted repeatedly",
				"citations": []string{model.ID, fabricatedID},
			})
			return result([]llm.ToolCall{{ID: "c3", Name: toolDone, Args: args}})
		},
	}

	e := New(s, script)
	ans, err := e.Reflect(ctx, "acme", "why does the db pool keep exhausting?")
	if err != nil {
		t.Fatal(err)
	}
	if ans.Text != "postgres is primary; the pool has been exhausted repeatedly" {
		t.Fatalf("text = %q", ans.Text)
	}
	if ans.Iterations != 3 {
		t.Fatalf("iterations = %d, want 3", ans.Iterations)
	}
	foundReal, foundFake := false, false
	for _, c := range ans.Citations {
		if c == model.ID {
			foundReal = true
		}
		if c == fabricatedID {
			foundFake = true
		}
	}
	if !foundReal {
		t.Fatalf("citations missing real model ID: %v", ans.Citations)
	}
	if foundFake {
		t.Fatalf("fabricated citation survived: %v", ans.Citations)
	}
	if len(ans.Citations) != 1 {
		t.Fatalf("citations = %v, want exactly the real ID", ans.Citations)
	}
	wantTokens := 3 * (10 + 5)
	if ans.Tokens != wantTokens {
		t.Fatalf("tokens = %d, want %d", ans.Tokens, wantTokens)
	}
}

func TestReflectNeverCallsDoneReturnsBestEffort(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	script := &scriptedLLM{t: t}
	// always calls list_models, never done -- must not loop forever and
	// must not hard-error.
	for i := 0; i < 10; i++ {
		script.queue = append(script.queue, func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c", Name: toolListModels, Args: json.RawMessage(`{}`)}})
		})
	}

	e := New(s, script)
	e.maxIter = 3 // bound small so the test doesn't need 6 scripted rounds
	ans, err := e.Reflect(ctx, "acme", "anything?")
	if err != nil {
		t.Fatal(err)
	}
	if ans.Iterations != 3 {
		t.Fatalf("iterations = %d, want bounded at 3", ans.Iterations)
	}
	if script.calls != 3 {
		t.Fatalf("client called %d times, want exactly 3 (bounded, not exhausted)", script.calls)
	}
}

// TestReflectWithLevelBoundsIterations pins the level ladder: minimal
// gives the loop 2 rounds, so a model that never calls done stops there
// with a best-effort answer instead of running the default 6.
func TestReflectWithLevelBoundsIterations(t *testing.T) {
	mem := newTestStore(t)
	textOnly := func(_ []llm.Turn) (*llm.Result, error) {
		return &llm.Result{Content: "thinking...", PromptTokens: 1, CompletionTokens: 1}, nil
	}
	client := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){textOnly, textOnly}}
	eng := New(mem, client)
	ans, err := eng.ReflectWith(context.Background(), "ns", "q", Opts{Level: "minimal"})
	if err != nil {
		t.Fatal(err)
	}
	if ans.Iterations != 2 || client.calls != 2 {
		t.Fatalf("minimal level: iterations=%d calls=%d, want 2/2", ans.Iterations, client.calls)
	}
	if ans.Text != "thinking..." {
		t.Fatalf("best-effort text = %q", ans.Text)
	}
	// Unknown level degrades to the default bound, not an error.
	client2 := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func(_ []llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "1", Name: toolDone, Args: json.RawMessage(`{"answer":"done"}`)}})
		},
	}}
	ans, err = New(mem, client2).ReflectWith(context.Background(), "ns", "q", Opts{Level: "vibes"})
	if err != nil || ans.Text != "done" {
		t.Fatalf("unknown level: %+v %v", ans, err)
	}
}

// newGraphStore builds the R02 multi-hop fixture: an incident fact (a)
// whose body matches a db-outage query, an outage fact (b) that also
// matches it, and a firmware fact (c) that matches neither token and is
// reachable ONLY through the described b->c relation. Bridge discovery
// cannot pull c in (one seed, needs two), so the initial recall can
// never see it: answering with c requires a second evidence hop.
func newGraphStore(t *testing.T) (*memory.Store, memory.Fact, memory.Fact, memory.Fact) {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()
	a, err := s.Write(ctx, memory.WriteInput{
		Namespace: "acme", Key: "/incidents/db", Body: "the db degraded overnight after the /net/outage",
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Write(ctx, memory.WriteInput{
		Namespace: "acme", Key: "/net/outage", Body: "switch outage affected the db; remedied by the firmware change",
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Write(ctx, memory.WriteInput{
		Namespace: "acme", Key: "/net/firmware", Body: "firmware 2.3.1 rollback restored the ports",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddLinkDescribed(ctx, "acme", b.Key, c.Key, "relates_to", 1.0, "remedied by"); err != nil {
		t.Fatal(err)
	}
	return s, *a, *b, *c
}

// TestExpandMultiHopReachesSecondHop is R02 red proof 1 (SYNTHETIC
// fixture, loop-MECHANISM evidence only): under the scripted policy the
// plain loop's single recall round cannot see past its horizon and
// abstains, while the opt-in expand tool reaches the linked fact in one
// more round and the answer can cite it. This proves the loop plumbing,
// NOT that a baseline is inherently unable - a different policy (another
// recall of the pointer term) can reach it too; the fair harness-level
// comparison with the same evidence-responsive policy on both arms lives
// in internal/membench.
func TestExpandMultiHopReachesSecondHop(t *testing.T) {
	s, a, b, c := newGraphStore(t)
	ctx := context.Background()

	// Expansion OFF control: the loop sees only a and b, cannot reach c,
	// and abstains rather than inventing the missing hop.
	off := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c1", Name: toolRecall, Args: json.RawMessage(`{"query":"db outage"}`)}})
		},
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c2", Name: toolDone, Args: json.RawMessage(`{"answer":"","abstain":true}`)}})
		},
	}}
	ansOff, err := New(s, off).ReflectWith(ctx, "acme", "what fixed the db?", Opts{})
	if err != nil {
		t.Fatal(err)
	}
	if !ansOff.Abstained {
		t.Fatalf("expansion-off: abstained = false, want true (c is beyond the recall horizon)")
	}
	for _, ev := range ansOff.Evidence {
		if ev == c.ID {
			t.Fatalf("expansion-off: firmware fact %s in evidence without expansion", c.ID)
		}
	}

	// Expansion ON: recall, then one expand over the outage key, then
	// done citing the firmware fact the second hop retrieved.
	on := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c1", Name: toolRecall, Args: json.RawMessage(`{"query":"db outage"}`)}})
		},
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c2", Name: toolExpand, Args: json.RawMessage(`{"keys":["/net/outage"]}`)}})
		},
		func([]llm.Turn) (*llm.Result, error) {
			args, _ := json.Marshal(map[string]any{
				"answer":    "the firmware rollback restored the db",
				"citations": []string{c.ID},
			})
			return result([]llm.ToolCall{{ID: "c3", Name: toolDone, Args: args}})
		},
	}}
	ansOn, err := New(s, on).ReflectWith(ctx, "acme", "what fixed the db?", Opts{ExpandEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if ansOn.Abstained {
		t.Fatal("expansion-on: abstained, want an answer")
	}
	if len(ansOn.Citations) != 1 || ansOn.Citations[0] != c.ID {
		t.Fatalf("citations = %v, want exactly [%s]", ansOn.Citations, c.ID)
	}
	seen := map[string]bool{}
	for _, id := range ansOn.Evidence {
		if seen[id] {
			t.Fatalf("evidence has duplicate %s: %v", id, ansOn.Evidence)
		}
		seen[id] = true
	}
	for _, want := range []string{a.ID, b.ID, c.ID} {
		if !seen[want] {
			t.Fatalf("evidence missing %s: %v", want, ansOn.Evidence)
		}
	}
	if len(ansOn.Rounds) != 3 {
		t.Fatalf("rounds = %+v, want 3 (recall round + expansion round + the done round)", ansOn.Rounds)
	}
	r := ansOn.Rounds[1]
	if len(r.EvidenceAdded) != 1 || r.EvidenceAdded[0] != c.ID {
		t.Fatalf("round evidence_added = %v, want [%s]", r.EvidenceAdded, c.ID)
	}
	if len(r.Keys) != 1 || r.Keys[0] != "/net/outage" {
		t.Fatalf("round keys = %v, want [/net/outage]", r.Keys)
	}
	if r.Tokens != 15 {
		t.Fatalf("round tokens = %d, want 15 (one 10+5 model call)", r.Tokens)
	}
	if r.LatencyMS < 0 {
		t.Fatalf("round latency = %d, want >= 0", r.LatencyMS)
	}
	if len(ansOn.Rounds[0].EvidenceAdded) != 2 {
		t.Fatalf("first round evidence_added = %v, want the two recall hits", ansOn.Rounds[0].EvidenceAdded)
	}
	if ansOn.StopReason != "" {
		t.Fatalf("stop reason = %q, want none", ansOn.StopReason)
	}
}

// TestExpandStopsWhenNoNewEvidence is R02 red proof 2: a repeated
// identical expansion that adds zero new evidence IDs stops the loop
// immediately - the model is never called again (Cognee's convergence
// rule, adapted). The stop is proven against GENUINELY repeated
// retrieval, not just repeated query text: the second round's tool
// result still contained the same underlying facts, so retrieval
// returned evidence twice and only the NEW-evidence count hit zero.
func TestExpandStopsWhenNoNewEvidence(t *testing.T) {
	s, _, _, c := newGraphStore(t)
	ctx := context.Background()

	var secondPayload string
	script := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c1", Name: toolExpand, Args: json.RawMessage(`{"keys":["/net/outage"]}`)}})
		},
		func(turns []llm.Turn) (*llm.Result, error) {
			// Capture the second expand's actual tool result: the
			// repeated retrieval must still have returned the facts.
			last := turns[len(turns)-1]
			if last.Role == "tool" {
				secondPayload = last.Content
			}
			return result([]llm.ToolCall{{ID: "c2", Name: toolExpand, Args: json.RawMessage(`{"keys":["/net/outage"]}`)}})
		},
	}}
	ans, err := New(s, script).ReflectWith(ctx, "acme", "q", Opts{ExpandEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if script.calls != 2 {
		t.Fatalf("model called %d times, want 2 (stopped before a third call)", script.calls)
	}
	if ans.StopReason != "no-new-evidence" {
		t.Fatalf("stop reason = %q, want no-new-evidence", ans.StopReason)
	}
	if len(ans.Rounds) != 2 {
		t.Fatalf("rounds = %d, want 2", len(ans.Rounds))
	}
	if added := ans.Rounds[1].EvidenceAdded; len(added) != 0 {
		t.Fatalf("second round evidence_added = %v, want empty", added)
	}
	var payload struct {
		Facts []memory.Fact `json:"facts"`
	}
	if err := json.Unmarshal([]byte(secondPayload), &payload); err != nil {
		t.Fatalf("second expand tool result = %q: %v", secondPayload, err)
	}
	returned := false
	for _, f := range payload.Facts {
		if f.ID == c.ID {
			returned = true
		}
	}
	if !returned {
		t.Fatalf("second retrieval returned %q: the no-new-evidence stop must follow a genuine re-retrieval of the same facts, not an empty read", secondPayload)
	}
	seen := map[string]bool{}
	for _, id := range ans.Evidence {
		if seen[id] {
			t.Fatalf("evidence has duplicate %s: %v", id, ans.Evidence)
		}
		seen[id] = true
	}
	if !seen[c.ID] {
		t.Fatalf("evidence missing the expanded fact %s: %v", c.ID, ans.Evidence)
	}
}

// TestExpandRejectsFabricatedRelationIDs is R02 red proof 3: relation
// context lines carry no citable IDs, and a fact KEY is not a fact ID.
// A model citing either is dropped by citation validation; only the
// real retrieved fact ID survives. Model-proposed queries and keys never
// become facts: the store holds exactly the fixture facts afterwards.
func TestExpandRejectsFabricatedRelationIDs(t *testing.T) {
	s, _, _, c := newGraphStore(t)
	ctx := context.Background()

	script := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c1", Name: toolExpand, Args: json.RawMessage(`{"queries":["firmware rollback fix"],"keys":["/net/outage"]}`)}})
		},
		func([]llm.Turn) (*llm.Result, error) {
			args, _ := json.Marshal(map[string]any{
				"answer": "the firmware rollback restored the ports",
				"citations": []string{
					c.ID,
					"rel:/net/outage->/net/firmware", // relation line: not a fact ID
					"/net/firmware",                  // the KEY, not the fact ID
				},
			})
			return result([]llm.ToolCall{{ID: "c2", Name: toolDone, Args: args}})
		},
	}}
	ans, err := New(s, script).ReflectWith(ctx, "acme", "q", Opts{ExpandEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Citations) != 1 || ans.Citations[0] != c.ID {
		t.Fatalf("citations = %v, want exactly [%s]", ans.Citations, c.ID)
	}
	keys, err := s.ListKeys(ctx, "acme", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 {
		t.Fatalf("store has %d keys after expansion, want 3: model suggestions must never become facts (%v)", len(keys), keys)
	}
}

// TestExpandTokenBudgetExhaustionPartialWithReason is R02 red proof 4:
// exhausting the token budget mid-expansion returns the partial answer
// with everything gathered so far, the sentinel error, and an explicit
// stop reason. MaxTokens stays the existing PRE-CALL MEASURED-USAGE
// gate: two 15-token calls spend the 30-token budget and the gate
// blocks the third call BEFORE it is made, so the provider sees two
// calls, not three. A call already in flight may still overshoot the
// bound by its own usage - this is a gate, not a strict provider spend
// ceiling - and unreported usage counts in UsageUnknown.
func TestExpandTokenBudgetExhaustionPartialWithReason(t *testing.T) {
	s, _, _, _ := newGraphStore(t)
	ctx := context.Background()

	script := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c1", Name: toolExpand, Args: json.RawMessage(`{"keys":["/net/outage"]}`)}})
		},
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c2", Name: toolExpand, Args: json.RawMessage(`{"keys":["/incidents/db"]}`)}})
		},
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c3", Name: toolRecall, Args: json.RawMessage(`{"query":"q"}`)}})
		},
	}}
	// The fixture isolates budget exhaustion: each expansion round adds
	// genuinely new evidence (b,c then a), so the no-new-evidence rule
	// cannot fire before the budget gate does.
	ans, err := New(s, script).ReflectWith(ctx, "acme", "q", Opts{ExpandEvidence: true, MaxTokens: 30})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if ans.StopReason != "token-budget" {
		t.Fatalf("stop reason = %q, want token-budget", ans.StopReason)
	}
	if len(ans.Evidence) == 0 {
		t.Fatalf("evidence = %v, want the partial set gathered before exhaustion", ans.Evidence)
	}
	if script.calls != 2 {
		t.Fatalf("model called %d times, want 2 (the pre-call gate blocks the third call before it is spent; a gate check is not a model call)", script.calls)
	}
	if len(ans.Rounds) != 2 {
		t.Fatalf("rounds = %d, want 2 (the blocked call never became a round)", len(ans.Rounds))
	}
}

// TestExpandMaxRoundsCap pins the expansion-round cap: once MaxRounds
// expand calls have executed, a further expand call is refused and the
// loop stops with the partial answer.
func TestExpandMaxRoundsCap(t *testing.T) {
	s, _, _, _ := newGraphStore(t)
	ctx := context.Background()
	d, err := s.Write(ctx, memory.WriteInput{
		Namespace: "acme", Key: "/net/switch", Body: "the switch power supply was replaced",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddLink(ctx, "acme", "/net/firmware", d.Key, "relates_to"); err != nil {
		t.Fatal(err)
	}

	script := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c1", Name: toolExpand, Args: json.RawMessage(`{"keys":["/net/outage"]}`)}})
		},
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c2", Name: toolExpand, Args: json.RawMessage(`{"keys":["/net/firmware"]}`)}})
		},
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c3", Name: toolExpand, Args: json.RawMessage(`{"keys":["/net/switch"]}`)}})
		},
	}}
	ans, err := New(s, script).ReflectWith(ctx, "acme", "q", Opts{ExpandEvidence: true, MaxRounds: 2})
	if err != nil {
		t.Fatal(err)
	}
	if ans.StopReason != "max-rounds" {
		t.Fatalf("stop reason = %q, want max-rounds", ans.StopReason)
	}
	if len(ans.Rounds) != 3 {
		t.Fatalf("rounds = %d, want 3 (two executed + the capped attempt recorded with no claimed work)", len(ans.Rounds))
	}
	capped := ans.Rounds[2]
	if len(capped.EvidenceAdded) != 0 || len(capped.Keys) != 0 || len(capped.Queries) != 0 {
		t.Fatalf("capped round claims work it never executed: %+v", capped)
	}
	found := false
	for _, id := range ans.Evidence {
		if id == d.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("evidence missing %s: %v", d.ID, ans.Evidence)
	}
}

// TestMaxToolCallsCap pins the caller's tool-execution cap: it bounds
// every tool execution (not just expansion), the loop stops with the
// partial answer once it is hit, and the model may still spend its
// final call on done.
func TestMaxToolCallsCap(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	script := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c1", Name: toolListModels, Args: json.RawMessage(`{}`)}})
		},
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c2", Name: toolRecall, Args: json.RawMessage(`{"query":"q"}`)}})
		},
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c3", Name: toolListModels, Args: json.RawMessage(`{}`)}})
		},
	}}
	ans, err := New(s, script).ReflectWith(ctx, "acme", "q", Opts{MaxToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	if ans.StopReason != "max-tool-calls" {
		t.Fatalf("stop reason = %q, want max-tool-calls", ans.StopReason)
	}
	if ans.Iterations != 3 {
		t.Fatalf("iterations = %d, want 3", ans.Iterations)
	}
}

// stepClock hands out scripted instants (last one repeats), so the
// deadline check is deterministic instead of racing a real clock.
type stepClock struct{ times []time.Time }

func (c *stepClock) Now() time.Time {
	t := c.times[0]
	if len(c.times) > 1 {
		c.times = c.times[1:]
	}
	return t
}

// TestDeadlineStopsWithReason pins the caller's time cap: once the
// deadline has passed, the loop stops before the next model call and
// returns the partial answer with an explicit stop reason.
func TestDeadlineStopsWithReason(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	clk := &stepClock{times: []time.Time{t0, t0, t0.Add(2 * time.Second)}}
	script := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c1", Name: toolListModels, Args: json.RawMessage(`{}`)}})
		},
		func([]llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "c2", Name: toolListModels, Args: json.RawMessage(`{}`)}})
		},
	}}
	e := New(s, script)
	e.now = clk.Now
	ans, err := e.ReflectWith(ctx, "acme", "q", Opts{Deadline: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if ans.StopReason != "deadline" {
		t.Fatalf("stop reason = %q, want deadline", ans.StopReason)
	}
	if script.calls != 1 {
		t.Fatalf("model called %d times, want 1 (deadline hit before the second call)", script.calls)
	}
}

// TestReflectWithSchemaStructuredAnswer pins Opts.Schema: the done tool
// carries the caller's schema for its answer, and a JSON-object answer
// lands in Answer.Structured (with Text as its compact serialization).
func TestReflectWithSchemaStructuredAnswer(t *testing.T) {
	mem := newTestStore(t)
	client := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func(_ []llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "1", Name: toolDone,
				Args: json.RawMessage(`{"answer":{"status":"complete","count":3},"citations":[]}`)}})
		},
	}}
	eng := New(mem, client)
	ans, err := eng.ReflectWith(context.Background(), "ns", "q",
		Opts{Schema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string"},"count":{"type":"number"}}}`)})
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Status string  `json:"status"`
		Count  float64 `json:"count"`
	}
	if err := json.Unmarshal(ans.Structured, &parsed); err != nil {
		t.Fatalf("structured not parseable: %v (%s)", err, ans.Structured)
	}
	if parsed.Status != "complete" || parsed.Count != 3 {
		t.Fatalf("structured = %+v", parsed)
	}
	if ans.Text == "" {
		t.Fatal("text serialization must accompany structured")
	}

	// A model that stringifies the JSON still yields structured.
	client2 := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func(_ []llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "1", Name: toolDone,
				Args: json.RawMessage(`{"answer":"{\"status\":\"ok\"}"}`)}})
		},
	}}
	ans, err = New(mem, client2).ReflectWith(context.Background(), "ns", "q",
		Opts{Schema: json.RawMessage(`{"type":"object"}`)})
	if err != nil || string(ans.Structured) != `{"status":"ok"}` {
		t.Fatalf("stringified: %s %v", ans.Structured, err)
	}

	// Without a schema, a plain string answer never sets Structured.
	client3 := &scriptedLLM{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func(_ []llm.Turn) (*llm.Result, error) {
			return result([]llm.ToolCall{{ID: "1", Name: toolDone, Args: json.RawMessage(`{"answer":"prose"}`)}})
		},
	}}
	ans, err = New(mem, client3).Reflect(context.Background(), "ns", "q")
	if err != nil || ans.Structured != nil || ans.Text != "prose" {
		t.Fatalf("plain: %+v %v", ans, err)
	}
}
