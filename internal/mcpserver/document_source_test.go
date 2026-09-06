package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/memory"
)

func callDocument(t *testing.T, cs *mcp.ClientSession, args map[string]any) documentOut {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "remember_document", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("remember_document %v: %v %s", args, err, text(t, res))
	}
	var out documentOut
	if err := json.Unmarshal([]byte(text(t, res)), &out); err != nil {
		t.Fatalf("decode out %s: %v", text(t, res), err)
	}
	return out
}

func chunkByBody(t *testing.T, mem *memory.Store, prefix string) map[string]memory.Fact {
	t.Helper()
	facts, err := mem.Recall(context.Background(), "ns", prefix+"/chunk-", 100)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]memory.Fact{}
	for _, f := range facts {
		out[f.Body] = f
	}
	return out
}

// TestRememberDocumentLegacyTextUnchanged pins the compatibility half of
// I01: a text-only remember_document call keeps the legacy positional
// chunk keys and counts.
func TestRememberDocumentLegacyTextUnchanged(t *testing.T) {
	cs, mem := sessionWithStore(t, nil)
	out := callDocument(t, cs, map[string]any{
		"namespace": "ns", "prefix": "/docs/legacy", "author": "t",
		"text": "para one about ceph\n\npara two about postgres",
	})
	if out.Written != 2 || out.Unchanged != 0 || out.Removed != 0 {
		t.Fatalf("legacy ingest = %+v, want written 2", out)
	}
	keys, err := mem.ListKeys(context.Background(), "ns", "/docs/legacy")
	if err != nil || len(keys) != 2 ||
		!strings.HasSuffix(keys[0], "/chunk-0001") || !strings.HasSuffix(keys[1], "/chunk-0002") {
		t.Fatalf("legacy keys = %v (err %v), want chunk-0001/chunk-0002", keys, err)
	}
}

// TestRememberDocumentSourceProvenance is the I01 red proof through the
// MCP surface: a source-aware call records provenance on every chunk, and
// inserting a paragraph at the beginning writes exactly one new chunk -
// unaffected later chunks keep their fact IDs and their original-revision
// provenance.
func TestRememberDocumentSourceProvenance(t *testing.T) {
	cs, mem := sessionWithStore(t, nil)
	src := map[string]any{"id": "runbook", "uri": "file:///docs/runbook.md",
		"revision": "r1", "media_type": "text/markdown"}

	out := callDocument(t, cs, map[string]any{
		"namespace": "ns", "prefix": "/docs/rb", "author": "t",
		"text":   "para B about postgres\n\npara C about redis",
		"source": src,
	})
	if out.Written != 2 || out.Unchanged != 0 || out.Removed != 0 {
		t.Fatalf("rev1 = %+v, want written 2", out)
	}
	before := chunkByBody(t, mem, "/docs/rb")
	bFact := before["para B about postgres"]
	prov, ok := bFact.Attributes["source"].(map[string]any)
	if !ok {
		t.Fatalf("chunk has no source provenance: %v", bFact.Attributes)
	}
	if prov["revision"] != "r1" || prov["id"] != "runbook" ||
		prov["uri"] != "file:///docs/runbook.md" || prov["media_type"] != "text/markdown" {
		t.Fatalf("provenance = %v", prov)
	}
	if hash, _ := prov["content_hash"].(string); len(hash) != 64 {
		t.Fatalf("content_hash = %v", prov["content_hash"])
	}
	if !strings.HasPrefix(bFact.SourceRef, "document:runbook@") {
		t.Fatalf("SourceRef = %q", bFact.SourceRef)
	}

	// early insertion at revision r2
	src2 := map[string]any{"id": "runbook", "uri": "file:///docs/runbook.md",
		"revision": "r2", "media_type": "text/markdown"}
	out = callDocument(t, cs, map[string]any{
		"namespace": "ns", "prefix": "/docs/rb", "author": "t",
		"text":   "para A about ceph\n\npara B about postgres\n\npara C about redis",
		"source": src2,
	})
	if out.Written != 1 || out.Unchanged != 2 || out.Removed != 0 {
		t.Fatalf("early insert = %+v, want 1 written / 2 unchanged", out)
	}
	after := chunkByBody(t, mem, "/docs/rb")
	if got := after["para B about postgres"]; got.ID != bFact.ID {
		t.Fatalf("unaffected chunk was rewritten: %s != %s", got.ID, bFact.ID)
	}
	bProv, _ := after["para B about postgres"].Attributes["source"].(map[string]any)
	if bProv["revision"] != "r1" {
		t.Fatalf("unaffected chunk provenance moved to %v, want the r1 it cites", bProv["revision"])
	}
	aProv, _ := after["para A about ceph"].Attributes["source"].(map[string]any)
	if aProv["revision"] != "r2" {
		t.Fatalf("new chunk provenance = %v, want revision r2", aProv)
	}

	// idempotent reingestion of the same revision
	out = callDocument(t, cs, map[string]any{
		"namespace": "ns", "prefix": "/docs/rb", "author": "t",
		"text":   "para A about ceph\n\npara B about postgres\n\npara C about redis",
		"source": src2,
	})
	if out.Written != 0 || out.Unchanged != 3 || out.Removed != 0 {
		t.Fatalf("reingest = %+v, want 0/3/0", out)
	}
}

// TestRememberDocumentSourceSections covers section-aware ingest through
// the tool: deleting a section tombstones only its own chunks.
func TestRememberDocumentSourceSections(t *testing.T) {
	cs, mem := sessionWithStore(t, nil)
	out := callDocument(t, cs, map[string]any{
		"namespace": "ns", "prefix": "/docs/manual", "author": "t",
		"source": map[string]any{"id": "manual", "revision": "r1", "sections": []any{
			map[string]any{"name": "install", "page": 1, "text": "step one about ceph"},
			map[string]any{"name": "ops", "page": 2, "text": "ops note about alerts"},
		}},
	})
	if out.Written != 2 {
		t.Fatalf("sections ingest = %+v, want written 2", out)
	}
	before := chunkByBody(t, mem, "/docs/manual")
	install := before["step one about ceph"]
	prov, _ := install.Attributes["source"].(map[string]any)
	if prov["section"] != "install" || prov["page"] != float64(1) {
		t.Fatalf("section provenance = %v", prov)
	}

	out = callDocument(t, cs, map[string]any{
		"namespace": "ns", "prefix": "/docs/manual", "author": "t",
		"source": map[string]any{"id": "manual", "revision": "r2", "sections": []any{
			map[string]any{"name": "ops", "page": 2, "text": "ops note about alerts"},
		}},
	})
	if out.Written != 0 || out.Unchanged != 1 || out.Removed != 1 {
		t.Fatalf("section delete = %+v, want 0/1/1", out)
	}
	keys, err := mem.ListKeys(context.Background(), "ns", "/docs/manual")
	if err != nil || len(keys) != 1 || keys[0] != before["ops note about alerts"].Key {
		t.Fatalf("live keys = %v (err %v), want only the ops chunk", keys, err)
	}

	// sections + text at once is an actionable error
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "remember_document", Arguments: map[string]any{
			"namespace": "ns", "prefix": "/docs/manual",
			"text":   "x",
			"source": map[string]any{"sections": []any{map[string]any{"text": "y"}}},
		}})
	if err == nil && !res.IsError {
		t.Fatal("sections together with text must error")
	}
	// unknown source keys are rejected with the expected shape named
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "remember_document", Arguments: map[string]any{
			"namespace": "ns", "prefix": "/docs/manual", "text": "x",
			"source": map[string]any{"bogus": 1},
		}})
	if err == nil && !res.IsError {
		t.Fatal("unknown source key must error")
	}
	if !strings.Contains(text(t, res), "sections") {
		t.Fatalf("error must name the expected shape: %s", text(t, res))
	}
}
