package mcpserver

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// nsResolver turns an omitted namespace into the one the connecting
// client's workspace maps to, using the same derivation the hooks use
// (Deps.NamespaceFor, wired to api.AgentNamespace). Roots are fetched
// once per session and cached; a client without roots support gets the
// default namespace.
type nsResolver struct {
	namespaceFor func(cwd string) string
	defaultNS    string
	mu           sync.Mutex
	bySession    map[*mcp.ServerSession]rootInfo
}

type rootInfo struct {
	path string // "" when the client advertised no usable root
}

func newNSResolver(namespaceFor func(string) string, defaultNS string) *nsResolver {
	if defaultNS == "" {
		defaultNS = "agent-default"
	}
	return &nsResolver{namespaceFor: namespaceFor, defaultNS: defaultNS, bySession: map[*mcp.ServerSession]rootInfo{}}
}

// rootPath returns the first file:// root of the session as a local path.
func (r *nsResolver) rootPath(ctx context.Context, ss *mcp.ServerSession) string {
	if ss == nil {
		return ""
	}
	r.mu.Lock()
	info, ok := r.bySession[ss]
	r.mu.Unlock()
	if ok {
		return info.path
	}
	path := ""
	if res, err := ss.ListRoots(ctx, nil); err == nil {
		for _, root := range res.Roots {
			if u, perr := url.Parse(root.URI); perr == nil && u.Scheme == "file" && u.Path != "" {
				path = filepath.Clean(u.Path)
				break
			}
		}
	}
	r.mu.Lock()
	r.bySession[ss] = rootInfo{path: path}
	r.mu.Unlock()
	return path
}

// resolve picks the namespace for a tool call: explicit wins, then the
// X-Punk-Namespace header (punk connect --project bakes the checkout's
// namespace into the MCP entry), then the client's workspace root, then
// the server default.
func (r *nsResolver) resolve(ctx context.Context, req *mcp.CallToolRequest, explicit string) (ns, source string) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, "explicit"
	}
	if req != nil && req.Extra != nil && req.Extra.Header != nil {
		if h := strings.TrimSpace(req.Extra.Header.Get("X-Punk-Namespace")); h != "" {
			return h, "header"
		}
	}
	if req != nil && r.namespaceFor != nil {
		if p := r.rootPath(ctx, req.Session); p != "" {
			return r.namespaceFor(p), "roots"
		}
	}
	return r.defaultNS, "default"
}

type whoamiOut struct {
	Namespace string `json:"namespace"`
	Source    string `json:"source"` // explicit | header | roots | default
	Root      string `json:"root,omitempty"`
}

func registerWhoami(s *mcp.Server, r *nsResolver) {
	mcp.AddTool(s, &mcp.Tool{Name: "whoami",
		Description: "Return the namespace this session's tool calls use when namespace is omitted, how it was resolved (roots | default), and the workspace root it came from. Call once at session start."},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, whoamiOut, error) {
			ns, src := r.resolve(ctx, req, "")
			return nil, whoamiOut{Namespace: ns, Source: src, Root: r.rootPath(ctx, req.Session)}, nil
		})
}
