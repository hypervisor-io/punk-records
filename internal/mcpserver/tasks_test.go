package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func callJSON(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("%s %v: %v %s", name, args, err, text(t, res))
	}
	if out != nil {
		if err := json.Unmarshal([]byte(text(t, res)), out); err != nil {
			t.Fatalf("%s: decode %q: %v", name, text(t, res), err)
		}
	}
}

func TestSetTaskStatusWritesCanonicalFactAndReleasesClaim(t *testing.T) {
	cs, mem := sessionWithStore(t, nil)
	ctx := context.Background()
	callJSON(t, cs, "remember", map[string]any{"namespace": "ns", "key": "/tasks/T1", "body": "do it"}, nil)
	callJSON(t, cs, "claim_work", map[string]any{"namespace": "ns", "key": "/tasks/T1", "holder": "w1", "ttl_seconds": 600}, nil)

	var out struct {
		Key   string `json:"key"`
		State string `json:"state"`
		Body  string `json:"body"`
	}
	callJSON(t, cs, "set_task_status", map[string]any{
		"namespace": "ns", "id": "T1", "state": "in_progress", "phase": "red", "summary": "writing the failing test",
	}, &out)
	if out.Key != "/tasks/T1/status" || out.State != "in_progress" || out.Body != "in_progress: red writing the failing test" {
		t.Fatalf("in_progress = %+v", out)
	}

	callJSON(t, cs, "set_task_status", map[string]any{
		"namespace": "ns", "id": "T1", "state": "done", "sha": "abc1234", "summary": "did it",
		"tests": "go test ./...", "deviation": "renamed foo", "agent": "w1",
	}, &out)
	if out.Body != "done: abc1234 did it; tests: go test ./...\ndeviation: renamed foo" {
		t.Fatalf("done body = %q", out.Body)
	}
	facts, err := mem.Recall(ctx, "ns", "/tasks/T1/status", 0)
	if err != nil || len(facts) != 1 {
		t.Fatalf("recall: %v %d", err, len(facts))
	}
	if facts[0].Attributes["state"] != "done" || facts[0].Attributes["sha"] != "abc1234" || facts[0].Attributes["tests"] != "go test ./..." || facts[0].Writer != "w1" {
		t.Fatalf("attributes = %+v writer = %q", facts[0].Attributes, facts[0].Writer)
	}

	var claims struct {
		Claims []struct{ Key string } `json:"claims"`
	}
	callJSON(t, cs, "list_claims", map[string]any{"namespace": "ns"}, &claims)
	if len(claims.Claims) != 0 {
		t.Fatalf("done must release the caller's claim: %+v", claims.Claims)
	}

	res, _ := cs.CallTool(ctx, &mcp.CallToolParams{Name: "set_task_status", Arguments: map[string]any{"namespace": "ns", "id": "T1", "state": "finished", "summary": "x"}})
	if res == nil || !res.IsError || !strings.Contains(text(t, res), "state must be one of") {
		t.Fatalf("bad state must error: %s", text(t, res))
	}
}

func TestListTasksBoard(t *testing.T) {
	cs := session(t)
	callJSON(t, cs, "remember_many", map[string]any{"namespace": "ns", "facts": []map[string]any{
		{"key": "/tasks/A", "body": "first"},
		{"key": "/tasks/B", "body": "second\ndepends_on: A"},
	}}, nil)
	var board struct {
		Next  string `json:"next"`
		Tasks []struct {
			ID    string `json:"id"`
			State string `json:"state"`
			Ready bool   `json:"ready"`
		} `json:"tasks"`
		Counts struct{ Pending, Done int } `json:"counts"`
	}
	callJSON(t, cs, "list_tasks", map[string]any{"namespace": "ns"}, &board)
	if board.Next != "A" || len(board.Tasks) != 2 || !board.Tasks[0].Ready || board.Tasks[1].Ready || board.Counts.Pending != 2 {
		t.Fatalf("board = %+v", board)
	}
	callJSON(t, cs, "set_task_status", map[string]any{"namespace": "ns", "id": "A", "state": "done", "summary": "ok", "sha": "abc"}, nil)
	callJSON(t, cs, "list_tasks", map[string]any{"namespace": "ns", "state": "pending"}, &board)
	if board.Next != "B" || len(board.Tasks) != 1 || board.Tasks[0].ID != "B" || !board.Tasks[0].Ready || board.Counts.Done != 1 {
		t.Fatalf("filtered board = %+v", board)
	}
}

func TestAwaitTasksWakesOnStatusWrite(t *testing.T) {
	b := bus.New()
	cs, mem := sessionWithStore(t, nil, func(d *Deps) { d.Bus = b })
	ctx := context.Background()
	callJSON(t, cs, "remember", map[string]any{"namespace": "ns", "key": "/tasks/A", "body": "first"}, nil)

	type awaitOut struct {
		Changed bool     `json:"changed"`
		Changes []string `json:"changes"`
		Next    string   `json:"next"`
	}
	var out awaitOut
	callJSON(t, cs, "await_tasks", map[string]any{"namespace": "ns", "timeout_seconds": 1}, &out)
	if out.Changed || len(out.Changes) != 0 || out.Next != "A" {
		t.Fatalf("quiet wait = %+v", out)
	}

	res := make(chan awaitOut, 1)
	go func() {
		var o awaitOut
		callJSON(t, cs, "await_tasks", map[string]any{"namespace": "ns", "timeout_seconds": 10}, &o)
		res <- o
	}()
	time.Sleep(50 * time.Millisecond)
	// The test server has no outbox tailer, so drive the bus the way the tailer would.
	if _, err := mem.Write(ctx, memoryWriteForTest("ns", "/tasks/A/status", "done: x")); err != nil {
		t.Fatal(err)
	}
	b.Publish(bus.Event{Kind: "memory", Key: "ns:/tasks/A/status", Data: map[string]string{"state": "done"}})
	select {
	case o := <-res:
		if !o.Changed || len(o.Changes) != 1 || o.Changes[0] != "/tasks/A/status" || o.Next != "" {
			t.Fatalf("woken = %+v", o)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("await_tasks did not wake")
	}
}

func memoryWriteForTest(ns, key, body string) memory.WriteInput {
	return memory.WriteInput{Namespace: ns, Key: key, Body: body, Writer: "test"}
}
