package reflect

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

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
