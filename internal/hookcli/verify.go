package hookcli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// VerifyReport is what a real MCP session against punk looked like.
type VerifyReport struct {
	Tools        []string
	Namespace    string
	Source       string
	Instructions bool
}

// bearerTransport sets Authorization: Bearer <key> on every request.
type bearerTransport struct{ key string }

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.key)
	return http.DefaultTransport.RoundTrip(clone)
}

// VerifyMCP connects to endpoint as an MCP client, lists tools and calls
// whoami. An agent whose first punk call fails tends to abandon the tool
// for the rest of the session, so connect ends by proving the round trip.
// When apiKey is non-empty every request carries it as a bearer token.
// When cwd is non-empty it is advertised as the client's only root, so
// whoami resolves the namespace the way a real editor session does
// instead of falling back to the server default.
func VerifyMCP(ctx context.Context, endpoint, apiKey, cwd string) (VerifyReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "punk-connect-verify", Version: "1"}, nil)
	if cwd != "" {
		client.AddRoots(&mcp.Root{URI: fileURI(cwd), Name: filepath.Base(cwd)})
	}
	transport := &mcp.StreamableClientTransport{Endpoint: endpoint}
	if apiKey != "" {
		transport.HTTPClient = &http.Client{Transport: bearerTransport{key: apiKey}}
	}
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("connect %s: %w", endpoint, err)
	}
	defer func() { _ = cs.Close() }()
	rep := VerifyReport{Instructions: cs.InitializeResult().Instructions != ""}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		return rep, fmt.Errorf("tools/list: %w", err)
	}
	for _, t := range tools.Tools {
		rep.Tools = append(rep.Tools, t.Name)
	}
	sort.Strings(rep.Tools)
	if len(rep.Tools) == 0 {
		return rep, fmt.Errorf("server at %s exposes no tools", endpoint)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil {
		return rep, fmt.Errorf("whoami: %w", err)
	}
	var out struct {
		Namespace string `json:"namespace"`
		Source    string `json:"source"`
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			_ = json.Unmarshal([]byte(tc.Text), &out)
		}
	}
	if out.Namespace == "" {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			_ = json.Unmarshal(raw, &out)
		}
	}
	rep.Namespace = out.Namespace
	rep.Source = out.Source
	return rep, nil
}

// VerifyHTTP is VerifyMCP for agents whose punk tools call the HTTP API
// directly (the pi extension): it authenticates against
// /v1/agent/namespace and returns the namespace cwd maps to, so a wrong
// URL or key fails here rather than on the agent's first tool call.
func VerifyHTTP(ctx context.Context, serverURL, apiKey, cwd string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(serverURL, "/")+"/v1/agent/namespace?cwd="+url.QueryEscape(cwd), nil)
	if err != nil {
		return "", err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("connect %s: %w", serverURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s/v1/agent/namespace: status %d", serverURL, resp.StatusCode)
	}
	var out struct {
		Namespace string `json:"namespace"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Namespace == "" {
		return "", fmt.Errorf("%s returned no namespace", serverURL)
	}
	return out.Namespace, nil
}

// fileURI renders an absolute local path as a file:// URI. A Windows
// drive path ("C:\\work") has no leading slash, so one is added to keep
// the three-slash form ("file:///C:/work") that MCP clients send.
func fileURI(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}
