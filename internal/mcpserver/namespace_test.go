package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sessionWithRoots is session(t) with client roots advertised, the way
// Claude Code and Cursor do for the open workspace.
func sessionWithRoots(t *testing.T, rootURIs ...string) *mcp.ClientSession {
	t.Helper()
	return sessionOpts(t, func(c *mcp.Client) {
		for _, u := range rootURIs {
			c.AddRoots(&mcp.Root{URI: u, Name: "ws"})
		}
	})
}

func TestWhoamiResolvesNamespaceFromRoots(t *testing.T) {
	cs := sessionWithRoots(t, "file:///home/dev/My_Project")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Namespace, Source, Root string
	}
	if err := json.Unmarshal([]byte(text(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Namespace != "agent-my-project" || out.Source != "roots" || out.Root != "/home/dev/My_Project" {
		t.Fatalf("whoami = %+v", out)
	}
}

func TestWhoamiFallsBackToDefault(t *testing.T) {
	cs := session(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := text(t, res); !strings.Contains(got, `"namespace":"agent-default"`) || !strings.Contains(got, `"source":"default"`) {
		t.Fatalf("whoami = %s", got)
	}
}

func TestRememberWithoutNamespaceUsesRoots(t *testing.T) {
	cs := sessionWithRoots(t, "file:///srv/repos/billing")
	ctx := context.Background()
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "remember",
		Arguments: map[string]any{"key": "/k", "body": "v"}}); err != nil {
		t.Fatal(err)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "recall",
		Arguments: map[string]any{"namespace": "agent-billing", "prefix": "/k"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text(t, res), `"body":"v"`) {
		t.Fatalf("fact not stored under resolved namespace: %s", text(t, res))
	}
}

func TestNamespaceFromHeaderBeatsRoots(t *testing.T) {
	srv := newTestServerForHTTP(t) // builds Deps exactly like sessionOpts and returns *mcp.Server
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(h)
	defer ts.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	client.AddRoots(&mcp.Root{URI: "file:///tmp/other"})
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   ts.URL,
		HTTPClient: &http.Client{Transport: headerRT{h: http.Header{"X-Punk-Namespace": {"agent-billing-1a2b3c"}}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := text(t, res); !strings.Contains(got, `"namespace":"agent-billing-1a2b3c"`) || !strings.Contains(got, `"source":"header"`) {
		t.Fatalf("whoami = %s", got)
	}
}

type headerRT struct{ h http.Header }

func (r headerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	c := req.Clone(req.Context())
	for k, v := range r.h {
		c.Header[k] = v
	}
	return http.DefaultTransport.RoundTrip(c)
}
