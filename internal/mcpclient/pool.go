// Package mcpclient connects to external MCP tool servers and exposes
// their tools to the agent runtime, namespaced <server>__<tool> so
// servers can never collide. One failed server never breaks the pool.
package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/config"
	"github.com/hypervisor-io/punk-records/internal/llm"
)

// Caller is what the runtime needs; fakes implement it in tests.
type Caller interface {
	Tools() []llm.Tool
	Call(ctx context.Context, name string, args json.RawMessage) (string, error)
}

type entry struct {
	session *mcp.ClientSession
	tool    string
}

// Pool aggregates tool sessions.
type Pool struct {
	log *slog.Logger

	mu    sync.RWMutex
	tools []llm.Tool
	index map[string]entry
}

func NewPool(log *slog.Logger) *Pool {
	return &Pool{log: log, index: map[string]entry{}}
}

// Connect dials every configured server; failures are logged and skipped.
func (p *Pool) Connect(ctx context.Context, servers []config.MCPServer) {
	for _, s := range servers {
		if err := p.connectOne(ctx, s); err != nil {
			p.log.Warn("mcp server skipped", "name", s.Name, "err", err)
		}
	}
}

func (p *Pool) connectOne(ctx context.Context, s config.MCPServer) error {
	if s.Name == "" {
		return fmt.Errorf("mcp server with empty name")
	}
	var transport mcp.Transport
	switch {
	case s.Command != "":
		transport = &mcp.CommandTransport{Command: exec.Command(s.Command, s.Args...)}
	case s.URL != "":
		hc := http.DefaultClient
		if s.Token != "" {
			tok := os.Getenv(s.Token)
			if tok == "" {
				return fmt.Errorf("token env %s empty", s.Token)
			}
			hc = &http.Client{Transport: bearerTransport{tok: tok}}
		}
		transport = &mcp.StreamableClientTransport{Endpoint: s.URL, HTTPClient: hc}
	default:
		return fmt.Errorf("server %s: need command or url", s.Name)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "punk-records", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return err
	}
	return p.AddSession(ctx, s.Name, session)
}

type bearerTransport struct{ tok string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

// AddSession registers a live session's tools under the server namespace.
// Exported so tests can wire in-memory servers.
func (p *Pool) AddSession(ctx context.Context, name string, session *mcp.ClientSession) error {
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		_ = session.Close()
		return fmt.Errorf("list tools: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range res.Tools {
		full := name + "__" + t.Name
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			schema = []byte(`{"type":"object"}`)
		}
		p.index[full] = entry{session: session, tool: t.Name}
		p.tools = append(p.tools, llm.Tool{Name: full, Description: t.Description, Schema: schema})
	}
	sort.Slice(p.tools, func(i, j int) bool { return p.tools[i].Name < p.tools[j].Name })
	p.log.Info("mcp server connected", "name", name, "tools", len(res.Tools))
	return nil
}

// Tools lists every namespaced tool.
func (p *Pool) Tools() []llm.Tool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]llm.Tool, len(p.tools))
	copy(out, p.tools)
	return out
}

// Call invokes one namespaced tool and flattens the text content.
// Tool-level errors (IsError) come back as errors, never silent text.
func (p *Pool) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	p.mu.RLock()
	e, ok := p.index[name]
	p.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("mcp: unknown tool %q", name)
	}
	var argMap map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argMap); err != nil {
			return "", fmt.Errorf("mcp: tool %s args: %w", name, err)
		}
	}
	res, err := e.session.CallTool(ctx, &mcp.CallToolParams{Name: e.tool, Arguments: argMap})
	if err != nil {
		return "", fmt.Errorf("mcp: call %s: %w", name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	if res.IsError {
		return "", fmt.Errorf("mcp: tool %s failed: %s", name, sb.String())
	}
	return sb.String(), nil
}
