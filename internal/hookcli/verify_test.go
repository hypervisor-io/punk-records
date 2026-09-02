package hookcli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestVerifyMCPAgainstInMemoryServer(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "punk-records", Version: "test"}, &mcp.ServerOptions{Instructions: "use it"})
	type out struct {
		Namespace string `json:"namespace"`
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "whoami", Description: "who"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, out, error) {
			return nil, out{Namespace: "agent-test"}, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(h)
	defer ts.Close()

	rep, err := VerifyMCP(context.Background(), ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Namespace != "agent-test" || !rep.Instructions || len(rep.Tools) != 1 || rep.Tools[0] != "whoami" {
		t.Fatalf("report = %+v", rep)
	}
	if _, err := VerifyMCP(context.Background(), "http://127.0.0.1:1", ""); err == nil {
		t.Fatal("unreachable endpoint must error")
	}
}
