package mcpserver

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// TestRegisterInboxBinding pins the spec contract for punk_register's
// optional inbox boolean: only an explicit inbox: true creates a
// binding (a plain register never auto-binds), the bind happens after a
// successful register, and a later register with inbox: true replaces
// the previous namespace.
func TestRegisterInboxBinding(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "register-inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	reg := region.New(db, nil)
	srv := New(Deps{Mem: memory.New(db, nil), Region: reg})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	// Plain register: no binding is created.
	callJSON(t, cs, "register", map[string]any{"namespace": "ns-a", "agent": "claude-code:s1"}, nil)
	if ns, ok, err := reg.InboxBinding(ctx, "claude-code:s1"); err != nil || ok || ns != "" {
		t.Fatalf("plain register bound inbox = (%q, %v, %v), want no binding", ns, ok, err)
	}

	// register with inbox: true binds to the registered namespace.
	callJSON(t, cs, "register", map[string]any{"namespace": "ns-a", "agent": "claude-code:s1", "inbox": true}, nil)
	if ns, ok, err := reg.InboxBinding(ctx, "claude-code:s1"); err != nil || !ok || ns != "ns-a" {
		t.Fatalf("inbox register binding = (%q, %v, %v), want ns-a", ns, ok, err)
	}

	// A re-register with inbox: true rebinds: latest explicit bind wins.
	callJSON(t, cs, "register", map[string]any{"namespace": "ns-b", "agent": "claude-code:s1", "inbox": true}, nil)
	if ns, ok, err := reg.InboxBinding(ctx, "claude-code:s1"); err != nil || !ok || ns != "ns-b" {
		t.Fatalf("rebind = (%q, %v, %v), want ns-b", ns, ok, err)
	}

	// A re-register WITHOUT inbox leaves the existing binding alone.
	callJSON(t, cs, "register", map[string]any{"namespace": "ns-c", "agent": "claude-code:s1"}, nil)
	if ns, ok, err := reg.InboxBinding(ctx, "claude-code:s1"); err != nil || !ok || ns != "ns-b" {
		t.Fatalf("plain re-register moved binding = (%q, %v, %v), want ns-b untouched", ns, ok, err)
	}
}
