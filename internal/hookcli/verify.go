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
	Root         string
	Instructions bool
}

// verifyTransport carries the connection's credentials on every request:
// the bearer token, and for project connections the pinned namespace
// header punk connect --project baked into the MCP entry, so verify
// resolves whoami exactly the way the configured agent will.
type verifyTransport struct{ apiKey, namespace string }

func (t verifyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if t.apiKey != "" {
		clone.Header.Set("Authorization", "Bearer "+t.apiKey)
	}
	if t.namespace != "" {
		clone.Header.Set("X-Punk-Namespace", t.namespace)
	}
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
	return verifyMCP(ctx, endpoint, apiKey, cwd, "")
}

// verifyMCP is VerifyMCP plus pinNS, the connection's pinned namespace
// (sent as X-Punk-Namespace, the header punk connect --project writes
// into the MCP entry). A roots-less probe (cwd "") with a pin answers
// the way Codex's native MCP client sees the connection.
func verifyMCP(ctx context.Context, endpoint, apiKey, cwd, pinNS string) (VerifyReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "punk-connect-verify", Version: "1"}, nil)
	if cwd != "" {
		client.AddRoots(&mcp.Root{URI: fileURI(cwd), Name: filepath.Base(cwd)})
	}
	transport := &mcp.StreamableClientTransport{Endpoint: endpoint}
	if apiKey != "" || pinNS != "" {
		transport.HTTPClient = &http.Client{Transport: verifyTransport{apiKey: apiKey, namespace: pinNS}}
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
		Root      string `json:"root"`
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
	rep.Root = out.Root
	return rep, nil
}

// CaptureDestination is one effective hook-capture destination: the
// namespace captures taken by a punk hook registration land in, why
// (pin = the hook command's --ns flag; path = the server derives it
// from the hook payload's cwd) and where the registration capturing
// there is installed. A destination with Source "path" may leave
// Namespace empty: CheckNamespaceAlignmentCaptures resolves it from
// Dir the way the server resolves it from the hook payload's cwd. A
// layered install (Codex appends hook registrations from every
// applicable config layer) has one destination per registration scope;
// hiding any of them would claim alignment for captures that actually
// land in another namespace.
type CaptureDestination struct {
	Namespace string
	Source    string // "pin" (--ns hook flag) | "path" (server-side derivation from the hook cwd)
	Origin    string // installed file/scope holding the registration ("" for a bare pin input)
}

// NamespaceAlignment is what verify learned about one connection: the
// capture namespaces the hooks use for Dir - one per effective
// registration scope, because layered installs capture into more than
// one - and the namespace MCP whoami resolves under each client
// posture (workspace roots advertised, the way editor MCP clients
// connect, and no roots at all, the way Codex's native MCP client
// connects) using the same server and credentials.
type NamespaceAlignment struct {
	Dir           string
	Captures      []CaptureDestination // every effective capture destination, resolved and merged
	Capture       string               // the namespace all destinations agree on ("" when they disagree)
	CaptureSource string               // pin (--ns hook flag) | path (server-side derivation from the hook cwd); "" for destinations carrying mixed sources
	MCPPin        string               // the MCP entry's X-Punk-Namespace pin ("" = unpinned); kept separate from Capture because the two pins are written to different files and can disagree
	Pinned        bool                 // whether the MCP side of this connection pins a namespace
	Roots         VerifyReport         // whoami with Dir advertised as the only root
	NoRoots       VerifyReport         // whoami without any roots
}

// distinctCaptures lists the capture namespaces of every destination,
// deduplicated, in destination order.
func (a NamespaceAlignment) distinctCaptures() []string {
	var out []string
	for _, c := range a.Captures {
		dup := false
		for _, s := range out {
			if s == c.Namespace {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, c.Namespace)
		}
	}
	return out
}

// Mismatch reports whether any capture destination disagrees with
// either client posture, or whether the installed scopes capture into
// more than one namespace at all - a split capture is a misalignment
// even when one of the destinations happens to match retrieval,
// because the other destination's memories land where the agent never
// reads.
func (a NamespaceAlignment) Mismatch() bool {
	if len(a.distinctCaptures()) > 1 {
		return true
	}
	for _, c := range a.Captures {
		if (a.Roots.Namespace != "" && a.Roots.Namespace != c.Namespace) ||
			(a.NoRoots.Namespace != "" && a.NoRoots.Namespace != c.Namespace) {
			return true
		}
	}
	return false
}

// Warnings renders the layered-capture line when the installed scopes
// capture into more than one namespace, one line per mismatched
// destination/posture pair, plus one remediation line. Both sides'
// namespaces are named because they are where existing memories
// already live: verify reports those locations and never copies,
// merges or deletes data - a layered split is diagnosed, and removing
// registrations is left to the user. The remediation distinguishes the
// causes that share the mismatched-posture symptom: when whoami
// answered with exactly the MCP pin via the header, the server applied
// that pin and the disagreement is local (layered or conflicting pins,
// or unpinned hooks deriving a different namespace) - only a pin the
// server provably ignored (it resolved something else) means the
// server does not honor X-Punk-Namespace.
func (a NamespaceAlignment) Warnings() []string {
	var out []string
	distinct := a.distinctCaptures()
	if len(distinct) > 1 {
		var parts []string
		for _, c := range a.Captures {
			where := a.Dir
			if c.Origin != "" {
				where = c.Origin
			}
			how := "pinned via --ns"
			if c.Source == "path" {
				how = "derived from the hook's working directory"
			}
			parts = append(parts, fmt.Sprintf("%s (%s, registered in %s)", c.Namespace, how, where))
		}
		out = append(out, fmt.Sprintf("the agent appends hook registrations from every applicable config layer, so captures for this directory land in %d namespaces - %s - while MCP retrieval resolves a single one, and memories captured through one registration are invisible to the others' namespace; remove the punk hook registrations you do not want by hand (verify preserves every installed file, nothing is cleaned up automatically)",
			len(distinct), strings.Join(parts, " and ")))
	}
	honored := false // the server provably applied the MCP entry's pin
	for _, c := range a.Captures {
		for _, p := range []struct {
			who string
			rep VerifyReport
		}{
			{"MCP sessions that advertise workspace roots", a.Roots},
			{"MCP sessions without roots support (Codex's native MCP client)", a.NoRoots},
		} {
			if p.rep.Namespace == "" || p.rep.Namespace == c.Namespace {
				continue
			}
			if a.MCPPin != "" && p.rep.Namespace == a.MCPPin && p.rep.Source == "header" {
				honored = true
			}
			where := "for " + a.Dir
			if c.Origin != "" {
				where = "in " + c.Origin
			}
			out = append(out, fmt.Sprintf("%s resolve namespace %s (source %s), but hooks %s capture into %s (%s); captured memories and MCP retrieval land in different namespaces - existing data in %s and %s is left where it is, nothing is copied or deleted",
				p.who, p.rep.Namespace, p.rep.Source, where, c.Namespace, c.Source, c.Namespace, p.rep.Namespace))
		}
	}
	if len(out) == 0 {
		return nil
	}
	switch {
	case len(distinct) > 1 && a.Pinned:
		return append(out, fmt.Sprintf("to make capture and retrieval agree, keep only the punk hook registrations that pin %s and remove the others from the files named above by hand - or rerun `punk connect codex --project` from the repository root afterwards so both sides of the connection carry %s", a.MCPPin, a.MCPPin))
	case len(distinct) > 1:
		return append(out, "to make capture and retrieval agree, keep ONE punk hook scope for this repository and remove the other punk registrations from the files named above by hand; `punk connect codex --project` from the repository root pins the remote-derived namespace into both the hooks and the MCP entry")
	case honored && a.CaptureSource == "pin":
		return append(out, fmt.Sprintf("the hook command pins %s but the MCP entry pins %s and the server honored the MCP pin - reconnect from the repository root (punk connect codex --project) so both sides of the connection carry the same namespace", a.Capture, a.MCPPin))
	case honored:
		return append(out, fmt.Sprintf("the MCP entry pins %s and the server honored it, but the hooks capture unpinned (the namespace derives from the hook's working directory); pin the hook side to match (punk connect codex --project from the repository root)", a.MCPPin))
	case a.Pinned:
		return append(out, fmt.Sprintf("this connection pins %s but the server resolved another namespace; the server does not honor X-Punk-Namespace - upgrade it", a.MCPPin))
	default:
		return append(out, "to make capture and retrieval agree for this repository, run `punk connect codex --project` from its root: the remote-derived namespace is pinned into both the hooks and the MCP entry (a global connection stays unpinned so every repository keeps resolving its own namespace)")
	}
}

// resolveCaptures fills in path-derived namespaces (the way the server
// derives them from the hook payload's cwd) and merges destinations
// that capture identically - same namespace and same source - joining
// their origins: one global and one project registration both
// capturing unpinned into the same directory are one destination with
// two homes.
func resolveCaptures(dir string, captures []CaptureDestination) []CaptureDestination {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	type acc struct {
		dest    CaptureDestination
		origins []string
	}
	var accs []acc
	index := map[string]int{}
	for _, c := range captures {
		if c.Source == "path" && c.Namespace == "" {
			c.Namespace = agentNamespaceForPath(abs)
		}
		key := c.Namespace + "\x00" + c.Source
		if i, ok := index[key]; ok {
			if c.Origin != "" {
				dup := false
				for _, o := range accs[i].origins {
					if o == c.Origin {
						dup = true
						break
					}
				}
				if !dup {
					accs[i].origins = append(accs[i].origins, c.Origin)
				}
			}
			continue
		}
		a := acc{dest: c}
		if c.Origin != "" {
			a.origins = []string{c.Origin}
		}
		index[key] = len(accs)
		accs = append(accs, a)
	}
	out := make([]CaptureDestination, 0, len(accs))
	for _, a := range accs {
		a.dest.Origin = strings.Join(a.origins, " and ")
		out = append(out, a.dest)
	}
	return out
}

// singleCapture reports the namespace every destination shares and,
// when they also share one source, that source. uniform is false when
// the destinations disagree about the namespace - there is no single
// honest capture then.
func singleCapture(captures []CaptureDestination) (ns, source string, uniform bool) {
	if len(captures) == 0 {
		return "", "", false
	}
	ns, source = captures[0].Namespace, captures[0].Source
	for _, c := range captures[1:] {
		if c.Namespace != ns {
			return "", "", false
		}
		if c.Source != source {
			source = ""
		}
	}
	return ns, source, true
}

// CheckNamespaceAlignmentCaptures proves that EVERY effective hook
// capture destination for a directory and the namespace MCP sessions
// resolve are the same one. captures carries every destination the
// installed hook scopes contribute (EnumerateCodexHookScopes feeds it;
// a layered install has one per scope) - all of them are compared,
// never just the first, because a capture landing in a namespace
// retrieval never reads is exactly the split-brain this diagnostic
// exists to catch. Destinations with Source "path" and an empty
// Namespace are resolved from dir. mcpNS is the namespace the MCP
// entry pins via X-Punk-Namespace ("" for a global connection).
func CheckNamespaceAlignmentCaptures(ctx context.Context, endpoint, apiKey, dir string, captures []CaptureDestination, mcpNS string) (NamespaceAlignment, error) {
	resolved := resolveCaptures(dir, captures)
	al := NamespaceAlignment{Dir: dir, Captures: resolved, MCPPin: mcpNS, Pinned: mcpNS != ""}
	if ns, src, uniform := singleCapture(resolved); uniform {
		al.Capture, al.CaptureSource = ns, src
	}
	roots, err := verifyMCP(ctx, endpoint, apiKey, dir, mcpNS)
	if err != nil {
		return al, err
	}
	al.Roots = roots
	if dir == "" {
		// No directory to advertise: the roots and no-roots postures are
		// the same session, so probe once.
		al.NoRoots = roots
		return al, nil
	}
	noRoots, err := verifyMCP(ctx, endpoint, apiKey, "", mcpNS)
	if err != nil {
		return al, err
	}
	al.NoRoots = noRoots
	return al, nil
}

// CheckNamespaceAlignment proves hooks and MCP sessions for the same
// directory land in the same namespace. hookNS is the namespace pinned
// into the hook command by punk connect --project ("" when the hooks
// carry no --ns and the server derives the namespace from the hook
// payload's cwd); mcpNS is the namespace the MCP entry pins via
// X-Punk-Namespace ("" for a global connection). The two are separate
// parameters because they are written to different files and can
// diverge - hooks pinned by a project install next to a global MCP
// entry is exactly the split-brain this diagnostic exists to catch.
// It is the single-destination view: a layered install whose scopes
// contribute more than one capture destination goes through
// CheckNamespaceAlignmentCaptures instead.
func CheckNamespaceAlignment(ctx context.Context, endpoint, apiKey, dir, hookNS, mcpNS string) (NamespaceAlignment, error) {
	c := CaptureDestination{Source: "path"}
	if hookNS != "" {
		c.Namespace, c.Source = hookNS, "pin"
	}
	return CheckNamespaceAlignmentCaptures(ctx, endpoint, apiKey, dir, []CaptureDestination{c}, mcpNS)
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
