package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/authz"
)

// P4-A: the join tool - one call that registers the session's address in
// the resolved namespace, binds its inbox and answers the who-am-I
// question (address, namespace, active peers with a delivery tier each),
// replacing the whoami + register(inbox:true) + list_region_members
// sequence the punk-join skill used to walk.
//
// Address resolution, in order:
//
//  1. The explicit address argument.
//  2. The identity the connection pins (X-Punk-Agent, else X-Punk-Subject
//     - the headers punk connect bakes into MCP entries and the hook
//     [PUNK ROUTING] block reports): a hook-supplied address is the
//     session's real address and is never replaced.
//  3. No hook-supplied address: a minted "<clientInfo.name>:<random>"
//     address, stable for the MCP session (repeat joins reuse it) and
//     never shared across sessions. The mint lives in server memory only
//     for the session's lifetime; what persists is the ordinary
//     membership and inbox binding Register/SetInboxBinding record.
//
// Delivery tiers per peer reuse the discovery facts list_region_members
// already exposes: "live" for an open inbox stream right now, "catch_up"
// for a recently seen member whose delivery waits for its client's next
// hook, read or await. Only active members are listed (the same
// memberActiveWindow the active_only filter uses).

type joinIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"defaults to workspace namespace (see whoami)"`
	Role      string `json:"role,omitempty" jsonschema:"this session's role in the region; metadata"`
	Address   string `json:"address,omitempty" jsonschema:"explicit punk address; defaults to this session's pinned identity, else a minted <client>:<random> address stable for this MCP session"`
}

type joinPeerOut struct {
	Agent      string `json:"agent"`
	Role       string `json:"role,omitempty"`
	Listening  bool   `json:"listening"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
	// Delivery is the peer's tier: "live" (an inbox stream is open now)
	// or "catch_up" (recently seen; delivery waits for the peer's next
	// hook, read or await).
	Delivery string `json:"delivery"`
}

type joinOut struct {
	Address   string        `json:"address"`
	Namespace string        `json:"namespace"`
	Minted    bool          `json:"minted,omitempty"` // true when join minted this session's address (no hook-supplied identity)
	Peers     []joinPeerOut `json:"peers"`
	// PeersNote explains an empty peers list when the register+bind half
	// succeeded but the peer listing was denied (a write-only grant):
	// the call stays successful, and read-classified data never leaks
	// past the grant.
	PeersNote string `json:"peers_note,omitempty"`
	// Delivery states how messages reach THIS address.
	Delivery string `json:"delivery"`
}

// joinDeliveryNote is the honest per-address delivery statement: the
// server cannot know whether the joining client runs a bridge, so it
// states both halves of the rule.
const joinDeliveryNote = "Delivery to this address is live while its bridge stream listens; otherwise messages wait in the box until the client's next hook, read or await (catch-up)."

// joinMinter mints and remembers the per-MCP-session address for
// sessions with no hook-supplied identity. The cache is keyed by the
// server session and bounded like the roots cache (reset wholesale past
// the cap; a reset session that joins again simply mints a fresh
// address, which is safe because joining is idempotent for the
// membership and rebinds the inbox to the same namespace).
type joinMinter struct {
	mu        sync.Mutex
	bySession map[*mcp.ServerSession]string
}

func newJoinMinter() *joinMinter {
	return &joinMinter{bySession: map[*mcp.ServerSession]string{}}
}

func (j *joinMinter) address(ss *mcp.ServerSession, clientName string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if a, ok := j.bySession[ss]; ok {
		return a
	}
	a := mintJoinAddress(clientName)
	if len(j.bySession) >= maxCachedSessions {
		j.bySession = map[*mcp.ServerSession]string{}
	}
	j.bySession[ss] = a
	return a
}

// mintJoinAddress renders "<client>:<16 hex>" from the MCP clientInfo
// name and crypto/rand, the same shape the client bridges' own owner
// tokens use. The name is sanitised to the identifier discipline the
// region enforces (no control characters, no surrounding whitespace); an
// absent name falls back to "mcp".
func mintJoinAddress(clientName string) string {
	name := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return '-'
		}
		return r
	}, strings.TrimSpace(clientName))
	if name == "" {
		name = "mcp"
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is not a recoverable condition (region.newMessageID's discipline)
	}
	return name + ":" + hex.EncodeToString(b)
}

// joinClientName reads the MCP clientInfo name from the session's
// initialize params, or "" when absent.
func joinClientName(ss *mcp.ServerSession) string {
	if ss == nil {
		return ""
	}
	if p := ss.InitializeParams(); p != nil && p.ClientInfo != nil {
		return p.ClientInfo.Name
	}
	return ""
}

// registerJoinTool adds the join tool. The minter is per server (shared
// across its sessions, keyed per session), created once here.
func registerJoinTool(s *mcp.Server, d Deps, nsr *nsResolver) {
	minter := newJoinMinter()
	mcp.AddTool(s, &mcp.Tool{Name: "join", OutputSchema: openOutputSchema[joinOut](),
		Description: "Join a brain region in one call: register this session's address, bind its inbox, and report the address, namespace and active peers with each peer's delivery tier (live stream or catch-up). Without a pinned identity a stable <client>:<random> address is minted for this session and reused on repeat calls."},
		func(ctx context.Context, req *mcp.CallToolRequest, in joinIn) (*mcp.CallToolResult, joinOut, error) {
			ns, err := nsr.resolveAuthed(ctx, req, in.Namespace, authz.OpWrite)
			if err != nil {
				return nil, joinOut{}, err
			}
			address := strings.TrimSpace(in.Address)
			minted := false
			switch {
			case address != "":
			default:
				if pinned := nsr.pinnedIdentity(req); pinned != "" {
					address = pinned
				} else {
					address = minter.address(req.Session, joinClientName(req.Session))
					minted = true
				}
			}
			if err := d.Region.Register(ctx, ns, address, in.Role); err != nil {
				return nil, joinOut{}, err
			}
			// Join always binds: unlike plain register (which never
			// auto-binds so cwd hops cannot rewire delivery), joining IS
			// the explicit bind intent - the latest join wins, the same
			// rule register(inbox:true) applies.
			if err := d.Region.SetInboxBinding(ctx, ns, address); err != nil {
				return nil, joinOut{}, err
			}
			out := joinOut{Address: address, Namespace: ns, Minted: minted,
				Peers: []joinPeerOut{}, Delivery: joinDeliveryNote}
			// The peer listing is read-classified: it rides the read
			// grant, and a denial degrades to a note instead of failing
			// the durable register+bind that already succeeded.
			if err := authorizeNS(ctx, ns, authz.OpRead); err != nil {
				out.PeersNote = "peers not listed: " + err.Error()
				return nil, out, nil
			}
			m, err := d.Region.MemberStatuses(ctx, ns)
			if err != nil {
				out.PeersNote = "peers not listed: " + err.Error()
				return nil, out, nil
			}
			for _, mem := range m {
				if !d.Region.Active(mem, memberActiveWindow) {
					continue
				}
				tier := "catch_up"
				if mem.Listening {
					tier = "live"
				}
				out.Peers = append(out.Peers, joinPeerOut{Agent: mem.Agent, Role: mem.Role,
					Listening: mem.Listening, LastSeenAt: mem.LastSeenAt, Delivery: tier})
			}
			// The list_region_members order: listening first, then most
			// recently seen, then name.
			sort.SliceStable(out.Peers, func(i, j int) bool {
				if out.Peers[i].Listening != out.Peers[j].Listening {
					return out.Peers[i].Listening
				}
				if out.Peers[i].LastSeenAt != out.Peers[j].LastSeenAt {
					return out.Peers[i].LastSeenAt > out.Peers[j].LastSeenAt
				}
				return out.Peers[i].Agent < out.Peers[j].Agent
			})
			return nil, out, nil
		})
}
