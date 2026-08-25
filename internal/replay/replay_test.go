package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/agent"
	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/store"
	"github.com/hypervisor-io/punk-records/internal/task"
)

func TestMatchTrajectory(t *testing.T) {
	rec := []string{"a", "b", "b", "c"}
	cases := []struct {
		cand []string
		mode TrajectoryMode
		want bool
	}{
		{[]string{"a", "b", "b", "c"}, Strict, true},
		{[]string{"a", "b", "c", "b"}, Strict, false},
		{[]string{"c", "b", "a", "b"}, Unordered, true},
		{[]string{"a", "b", "b", "b"}, Unordered, false},
		{[]string{"a", "c"}, Subset, true},
		{[]string{"a", "z"}, Subset, false},
	}
	for i, c := range cases {
		got, reason := MatchTrajectory(rec, c.cand, c.mode)
		if got != c.want {
			t.Errorf("case %d (%s): got %v (%s)", i, c.mode, got, reason)
		}
	}
	if !PassHatK([]bool{true, true, true}) || PassHatK([]bool{true, false, true}) || PassHatK(nil) {
		t.Fatal("PassHatK wrong")
	}
}

func TestSnapshotToolsOrderAndErrors(t *testing.T) {
	st := NewSnapshotTools([]task.SnapshotRow{
		{Seq: 1, Tool: "x", Args: `{"a":1}`, Result: "one", Status: "ok"},
		{Seq: 2, Tool: "x", Args: `{"a":2}`, Result: "two", Status: "ok"},
		{Seq: 3, Tool: "y", Args: `{}`, Result: "boom", Status: "error"},
	})
	// exact match wins even out of order
	out, err := st.Call(context.Background(), "x", json.RawMessage(`{"a":2}`))
	if err != nil || out != "two" {
		t.Fatalf("exact: %q %v", out, err)
	}
	// rephrased args fall back to oldest unused
	out, err = st.Call(context.Background(), "x", json.RawMessage(`{"a":"rephrased"}`))
	if err != nil || out != "one" {
		t.Fatalf("fallback: %q %v", out, err)
	}
	// frozen errors replay as errors
	if _, err := st.Call(context.Background(), "y", nil); err == nil {
		t.Fatal("frozen error not surfaced")
	}
	// world ends
	if _, err := st.Call(context.Background(), "x", nil); err == nil {
		t.Fatal("exhausted snapshots still answering")
	}
}

// scripted client (same shape as the agent package harness).
type scripted struct {
	t     *testing.T
	queue []func(turns []llm.Turn) (*llm.Result, error)
	calls int
}

func (s *scripted) Chat(_ context.Context, turns []llm.Turn, _ []llm.Tool) (*llm.Result, error) {
	if s.calls >= len(s.queue) {
		s.t.Fatalf("scripted client exhausted after %d calls", s.calls)
	}
	fn := s.queue[s.calls]
	s.calls++
	return fn(turns)
}
func (s *scripted) Model() string { return "scripted" }

const replayAgent = `---
name: database
version: 0.1.0
description: db specialist
tools:
  - incidents__*
triggers:
  - source: acme
---
You are the database agent.
`

func seqFromLastTool(turns []llm.Turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "tool" {
			s := turns[i].Content
			s = s[strings.Index(s, "seq=")+4:]
			return s[:strings.Index(s, "]")]
		}
	}
	return "0"
}

func TestRerunAgainstFrozenWorld(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	specDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(specDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "agents", "database.md"), []byte(replayAgent), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := registry.New(specDir, db, slog.New(slog.DiscardHandler))
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clk = clk.Add(time.Millisecond); return clk }
	ledger := task.NewLedger(db, now)

	// ---- original live run (fake world tool) ----
	liveTools := &liveFake{result: `{"incident":{"id":7,"title":"db connections exhausted"}}`}
	rt := agent.New(ledger, reg, nil, llm.NewManager(false, nil, db, now), liveTools, slog.New(slog.DiscardHandler))
	rt.FixedClient = &scripted{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) {
			return &llm.Result{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "incidents__get_incident", Args: json.RawMessage(`{"id":7}`)}},
				PromptTokens: 5, CompletionTokens: 5}, nil
		},
		func(turns []llm.Turn) (*llm.Result, error) {
			return &llm.Result{Content: fmt.Sprintf(
				`{"finding":{"summary":"pool leak","confidence":"high","evidence":[{"tool_call_seq":%s}]}}`,
				seqFromLastTool(turns)), PromptTokens: 3, CompletionTokens: 3}, nil
		},
	}}
	orig, _, err := ledger.Submit(ctx, task.SubmitInput{
		Source: "acme", ExternalRef: "incident:7",
		Budget: task.Budget{Tokens: 1000, ToolCalls: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, verID, _ := reg.Get("database")
	if err := ledger.AssignAgent(ctx, orig.ID, "database", verID, "test", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if err := rt.RunTask(ctx, orig.ID); err != nil {
		t.Fatal(err)
	}
	snaps, err := ledger.Snapshots(ctx, orig.ID)
	if err != nil || len(snaps) != 1 || snaps[0].Tool != "incidents__get_incident" {
		t.Fatalf("snapshots = %+v err=%v", snaps, err)
	}

	// ---- replay against the frozen world ----
	replayClient := &scripted{t: t, queue: []func([]llm.Turn) (*llm.Result, error){
		func([]llm.Turn) (*llm.Result, error) {
			return &llm.Result{ToolCalls: []llm.ToolCall{{ID: "r1", Name: "incidents__get_incident", Args: json.RawMessage(`{"id":7}`)}},
				PromptTokens: 5, CompletionTokens: 5}, nil
		},
		func(turns []llm.Turn) (*llm.Result, error) {
			// the frozen world must serve the recorded result
			last := turns[len(turns)-1]
			if !strings.Contains(last.Content, "db connections exhausted") {
				return nil, fmt.Errorf("frozen world missing: %q", last.Content)
			}
			return &llm.Result{Content: fmt.Sprintf(
				`{"finding":{"summary":"pool leak (replayed)","confidence":"high","evidence":[{"tool_call_seq":%s}]}}`,
				seqFromLastTool(turns)), PromptTokens: 3, CompletionTokens: 3}, nil
		},
	}}
	res, err := Rerun(ctx, ledger, reg, replayClient, orig.ID, Strict, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pass || res.Status != task.StatusCompleted {
		t.Fatalf("replay result = %+v", res)
	}
	if len(res.Recorded) != 1 || len(res.Candidate) != 1 {
		t.Fatalf("trajectories = %+v", res)
	}
}

type liveFake struct{ result string }

func (l *liveFake) Tools() []llm.Tool {
	return []llm.Tool{{Name: "incidents__get_incident", Description: "d", Schema: json.RawMessage(`{"type":"object"}`)}}
}
func (l *liveFake) Call(context.Context, string, json.RawMessage) (string, error) {
	return l.result, nil
}
