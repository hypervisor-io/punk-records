package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoIn struct {
	Text string `json:"text" jsonschema:"text to echo"`
}
type echoOut struct {
	Echo string `json:"echo"`
}

func inMemorySession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0.0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "echo text back"},
		func(ctx context.Context, req *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, echoOut, error) {
			if in.Text == "explode" {
				return nil, echoOut{}, fmt.Errorf("boom")
			}
			return nil, echoOut{Echo: "echo:" + in.Text}, nil
		})

	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	session, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestPoolNamespacingAndCall(t *testing.T) {
	p := NewPool(slog.New(slog.DiscardHandler))
	if err := p.AddSession(context.Background(), "fake", inMemorySession(t)); err != nil {
		t.Fatal(err)
	}

	tools := p.Tools()
	if len(tools) != 1 || tools[0].Name != "fake__echo" {
		t.Fatalf("tools = %+v", tools)
	}
	if len(tools[0].Schema) == 0 {
		t.Fatal("schema not propagated")
	}

	out, err := p.Call(context.Background(), "fake__echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "echo:hi") {
		t.Fatalf("out = %q", out)
	}

	if _, err := p.Call(context.Background(), "ghost__tool", nil); err == nil {
		t.Fatal("unknown tool accepted")
	}
	if _, err := p.Call(context.Background(), "fake__echo", json.RawMessage(`{"text":"explode"}`)); err == nil {
		t.Fatal("tool IsError not surfaced")
	}
}
