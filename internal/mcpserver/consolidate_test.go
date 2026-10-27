package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
)

func callConsolidate(t *testing.T, cs *mcp.ClientSession, args map[string]any) (consolidateOut, *mcp.CallToolResult) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "consolidate", Arguments: args})
	if err != nil {
		t.Fatalf("consolidate %v: %v", args, err)
	}
	var out consolidateOut
	if !res.IsError {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("consolidate output: %v", err)
		}
	}
	return out, res
}

// TestConsolidateToolDigestAndApply walks /punk-consolidate end to end:
// overview, a facts page, a messages page, the confirm guard, then apply
// - summary and open work stay live, everything else and the read mail
// move to the archive, unread mail stays, and the namespace is marked.
func TestConsolidateToolDigestAndApply(t *testing.T) {
	var mem *memory.Store
	cs, reg := unreadSession(t, "user@host", "", func(d *Deps) { mem = d.Mem })
	ctx := context.Background()
	for k, v := range map[string]string{
		"/tasks/A": "Ship it", "/tasks/A/status": "done: abc shipped",
		"/tasks/B": "Docs", "/decisions/x": "use WAL", "/plan/summary": "the plan",
	} {
		if _, err := mem.Write(ctx, memory.WriteInput{Namespace: "proj", Key: k, Body: v, Writer: "planner"}); err != nil {
			t.Fatal(err)
		}
	}
	seedUnread(t, reg, "proj", "w1", "planner", 2)
	msgs, err := reg.ListMessages(ctx, "proj", region.MessageLogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.AckMessages(ctx, "proj", "planner", []string{msgs[0].ID}); err != nil {
		t.Fatal(err)
	}

	ov, _ := callConsolidate(t, cs, map[string]any{"namespace": "proj"})
	o := ov.Overview
	if o == nil || o.LiveKeys != 5 || strings.Join(o.OpenTasks, ",") != "B" || o.Messages == nil || o.Messages.Total != 2 || o.Messages.Unread != 1 {
		t.Fatalf("overview = %+v", o)
	}
	if !strings.HasPrefix(o.Plan.Archive, "proj-archive-") || o.Plan.KeepKeys != 1 {
		t.Fatalf("plan = %+v", o.Plan)
	}
	fp, _ := callConsolidate(t, cs, map[string]any{"namespace": "proj", "section": "facts", "prefix": "/decisions/"})
	if len(fp.Facts) != 1 || fp.Facts[0].Key != "/decisions/x" || fp.NextCursor != "" {
		t.Fatalf("facts page = %+v", fp)
	}
	mp, _ := callConsolidate(t, cs, map[string]any{"namespace": "proj", "section": "messages"})
	if len(mp.Messages) != 2 || !strings.Contains(mp.Note, "untrusted") {
		t.Fatalf("messages page = %+v", mp)
	}

	if _, res := callConsolidate(t, cs, map[string]any{"namespace": "proj", "action": "apply", "summary": "# S", "confirm": "other"}); !res.IsError {
		t.Fatal("apply without a matching confirm must fail")
	}
	if _, res := callConsolidate(t, cs, map[string]any{"namespace": "", "action": "digest"}); !res.IsError {
		t.Fatal("an empty namespace must fail, never fall back to the session's")
	}

	ap, res := callConsolidate(t, cs, map[string]any{"namespace": "proj", "action": "apply", "summary": "# Summary\nall done", "confirm": "proj"})
	if res.IsError || ap.Result == nil || ap.Result.MovedMessages != 1 {
		t.Fatalf("apply = %+v %v", ap.Result, textItems(res))
	}
	keys, _ := mem.ListKeys(ctx, "proj", "/")
	if got := strings.Join(keys, ","); got != "/consolidated/status,/consolidated/summary,/tasks/B" {
		t.Fatalf("live after apply = %s", got)
	}
	archived, _ := mem.ListKeys(ctx, ap.Result.Archive, "/")
	if got := strings.Join(archived, ","); got != "/archive/source,/decisions/x,/plan/summary,/tasks/A,/tasks/A/status" {
		t.Fatalf("archive = %s", got)
	}
	left, _ := reg.ListMessages(ctx, "proj", region.MessageLogOptions{})
	if len(left) != 1 || left[0].AckedAt != "" {
		t.Fatalf("messages left = %+v, want only the unread one", left)
	}
	marks, err := mem.NamespaceMarks(ctx)
	if err != nil || marks["proj"].ConsolidatedAt.IsZero() || !marks[ap.Result.Archive].Archive {
		t.Fatalf("marks = %+v %v", marks, err)
	}
}
