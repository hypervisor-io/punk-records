package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		HTTPClient: &http.Client{Transport: headerRT{h: http.Header{"X-Punk-Namespace": {"agent-billing-1a2b3c"}, "X-Punk-Agent": {"alice@laptop"}}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	got := text(t, res)
	if !strings.Contains(got, `"namespace":"agent-billing-1a2b3c"`) || !strings.Contains(got, `"source":"header"`) {
		t.Fatalf("whoami = %s", got)
	}
	if !strings.Contains(got, `"agent":"alice@laptop"`) {
		t.Fatalf("whoami agent = %s", got)
	}
}

func TestClaimWorkDefaultsHolderToIdentity(t *testing.T) {
	srv := newTestServerForHTTP(t)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(h)
	defer ts.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   ts.URL,
		HTTPClient: &http.Client{Transport: headerRT{h: http.Header{"X-Punk-Agent": {"bob@ci"}}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	ctx := context.Background()
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "claim_work",
		Arguments: map[string]any{"namespace": "ns", "key": "/tasks/T1"}}); err != nil {
		t.Fatal(err)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_claims", Arguments: map[string]any{"namespace": "ns"}})
	if err != nil || !strings.Contains(text(t, res), `"holder":"bob@ci"`) {
		t.Fatalf("claims = %s %v", text(t, res), err)
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

func TestClientSupportsRoots(t *testing.T) {
	if clientSupportsRoots(nil) {
		t.Fatal("nil params must not support roots")
	}
	if clientSupportsRoots(&mcp.InitializeParams{}) {
		t.Fatal("nil capabilities must not support roots")
	}
	if clientSupportsRoots(&mcp.InitializeParams{Capabilities: &mcp.ClientCapabilities{}}) {
		t.Fatal("capabilities without roots must not support roots")
	}
	if !clientSupportsRoots(&mcp.InitializeParams{Capabilities: &mcp.ClientCapabilities{RootsV2: &mcp.RootCapabilities{}}}) {
		t.Fatal("roots capability must be detected")
	}
}

// TestRawHTTPClientWithoutRootsIsNotAskedForRoots drives the server the way
// scripts/punk.sh does: plain JSON-RPC over HTTP with no roots capability.
// The tool call must return the result directly instead of emitting a
// roots/list request the client can never answer.
func TestRawHTTPClientWithoutRootsIsNotAskedForRoots(t *testing.T) {
	srv := newTestServerForHTTP(t)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(h)
	defer ts.Close()
	post := func(sid, body string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		res, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("post: %v (a hang here means the server waited on roots/list)", err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}
	res, _ := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}`)
	sid := res.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("no session id from initialize")
	}
	post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_, body := post(sid, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)
	if strings.Contains(body, `"method":"roots/list"`) {
		t.Fatalf("server asked a roots-less client for roots:\n%s", body)
	}
	if !strings.Contains(body, `"source":"default"`) {
		t.Fatalf("whoami over raw HTTP = %s", body)
	}
}
