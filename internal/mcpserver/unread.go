package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/authz"
)

// Task P1-C: the unread trailer. Successful non-message tool results
// append one text line when the caller's resolved address has unread
// mail in the namespace the call resolved, so a session at its prompt
// learns about deliveries without polling read_messages. The trailer is
// advisory: only messaging-enabled sessions pay for the count, the count
// rides the indexed unread query (region.Store.CountUnreadMessages, the
// same path read_messages count_only uses), a short per-address cache
// bounds it, and every failure mode - count error, denied grant,
// unresolvable address - silently drops the trailer instead of failing
// the tool call it rides on.

// messageToolNames are the message tools themselves: their results are
// the mail flow, so a trailer there would be noise (read/await return
// the messages; send/ack change the unread state mid-call).
// list_region_members is discovery, not messaging, and keeps the trailer.
var messageToolNames = map[string]bool{
	"send_message": true, "read_messages": true,
	"ack_messages": true, "await_messages": true,
}

// unreadTrailerTTL bounds how stale the trailer's count may be: one
// recount per namespace+address per TTL, so a burst of tool calls costs
// one indexed query. Delivery stays prompt because the trailer is a
// hint; the messages themselves arrive through the message tools.
const unreadTrailerTTL = 5 * time.Second

// maxUnreadCache bounds the trailer cache; see unread.
const maxUnreadCache = 1024

// unreadCounter counts one address's unread messages in a namespace.
// Wired to region.Store.CountUnreadMessages; a variable so tests can
// fake failures and count the calls.
type unreadCounter func(ctx context.Context, ns, agent string) (int64, error)

type unreadCacheKey struct{ ns, address string }

type unreadCacheEntry struct {
	n  int64
	at time.Time
}

// unreadTrailer renders and caches the trailer line; one per server.
type unreadTrailer struct {
	count unreadCounter
	nsr   *nsResolver
	now   func() time.Time // variable for cache-expiry tests
	mu    sync.Mutex
	cache map[unreadCacheKey]unreadCacheEntry
}

func newUnreadTrailer(count unreadCounter, nsr *nsResolver) *unreadTrailer {
	return &unreadTrailer{
		count: count, nsr: nsr, now: time.Now,
		cache: map[unreadCacheKey]unreadCacheEntry{},
	}
}

// middleware appends the trailer to successful non-message tool results.
// It runs after the tool handler produced the final CallToolResult, so
// it sees exactly what the client would otherwise receive: errored
// results (IsError) and protocol errors pass through untouched, and the
// tool's own text and structured content stay intact - the trailer is
// one appended text item.
func (t *unreadTrailer) middleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil || method != "tools/call" {
				return res, err
			}
			call, ok := req.(*mcp.CallToolRequest)
			if !ok {
				return res, nil
			}
			result, ok := res.(*mcp.CallToolResult)
			if !ok || result == nil || result.IsError {
				return res, nil
			}
			if line := t.line(ctx, call); line != "" {
				result.Content = append(result.Content, &mcp.TextContent{Text: line})
			}
			return result, nil
		}
	}
}

// line renders the trailer for one tool call: exactly one line,
// "N unread for <address> in <ns>: call read_messages", or "" when
// nothing should be appended (message tool, no unread, unresolvable
// address, denied read grant, count error).
func (t *unreadTrailer) line(ctx context.Context, req *mcp.CallToolRequest) string {
	if req == nil || req.Params == nil || messageToolNames[req.Params.Name] {
		return ""
	}
	args := toolCallArgs(req)
	// The address whose unread the trailer reports: the call's own
	// explicit agent/sender argument when it has one, else the session
	// identity (X-Punk-Agent, then the verified subject). Anonymous
	// "mcp" callers get no trailer, mirroring touch's heartbeat policy.
	address := strings.TrimSpace(args.Agent)
	if address == "" {
		address = strings.TrimSpace(args.Sender)
	}
	if address == "" {
		address = t.nsr.identity(req)
	}
	if address == "" || address == "mcp" {
		return ""
	}
	// The namespace the tool call itself resolved: the same resolution
	// every tool runs (explicit argument > header > roots > default).
	ns, _ := t.nsr.resolve(ctx, req, args.Namespace)
	// The trailer discloses read-class information (the same fact
	// read_messages count_only returns), so a caller whose grant on the
	// resolved namespace does not include read gets nothing. The denial
	// is silent: a missing trailer must never explain itself, and the
	// tool call it rides on already passed its own authorization.
	if err := authorizeNS(ctx, ns, authz.OpRead); err != nil {
		return ""
	}
	n, err := t.unread(ctx, ns, address)
	if err != nil || n <= 0 {
		return "" // advisory only: a failed or empty count shows nothing
	}
	return fmt.Sprintf("%d unread for %s in %s: call read_messages", n, address, ns)
}

// unread returns the cached count for ns+address, recounting at most
// once per unreadTrailerTTL. Entries expire lazily on read; the map is
// bounded like nsResolver's roots cache - expired entries are pruned at
// the cap, and past the cap it resets wholesale, which costs one extra
// indexed count per live address and nothing else.
func (t *unreadTrailer) unread(ctx context.Context, ns, address string) (int64, error) {
	key := unreadCacheKey{ns: ns, address: address}
	now := t.now()
	t.mu.Lock()
	if e, ok := t.cache[key]; ok && now.Sub(e.at) < unreadTrailerTTL {
		t.mu.Unlock()
		return e.n, nil
	}
	t.mu.Unlock()
	n, err := t.count(ctx, ns, address)
	if err != nil {
		return 0, err // the caller drops the trailer; errors never surface
	}
	t.mu.Lock()
	if len(t.cache) >= maxUnreadCache {
		for k, e := range t.cache {
			if now.Sub(e.at) >= unreadTrailerTTL {
				delete(t.cache, k)
			}
		}
		if len(t.cache) >= maxUnreadCache {
			t.cache = map[unreadCacheKey]unreadCacheEntry{}
		}
	}
	t.cache[key] = unreadCacheEntry{n: n, at: now}
	t.mu.Unlock()
	return n, nil
}

// trailerArgs are the raw wire arguments the trailer reads: the
// namespace the call targets and the explicit identity arguments.
type trailerArgs struct {
	Namespace string `json:"namespace"`
	Agent     string `json:"agent"`
	Sender    string `json:"sender"`
}

// toolCallArgs decodes the trailer's fields from the raw arguments.
// Anything malformed decodes to zero values, so the trailer degrades to
// the identity/default resolution instead of failing the call.
func toolCallArgs(req *mcp.CallToolRequest) trailerArgs {
	var args trailerArgs
	if req.Params != nil && len(req.Params.Arguments) > 0 {
		_ = json.Unmarshal(req.Params.Arguments, &args)
	}
	return args
}
