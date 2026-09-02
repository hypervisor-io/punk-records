package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/memory"
)

// agentHookIn is the Claude Code hook stdin payload, verbatim
// (code.claude.com/docs/en/hooks). Unknown fields are ignored; unknown
// events answer 200 ignored so future hook types never break capture.
type agentHookIn struct {
	HookEventName        string          `json:"hook_event_name"`
	SessionID            string          `json:"session_id"`
	CWD                  string          `json:"cwd"`
	Source               string          `json:"source"`
	Prompt               string          `json:"prompt"`
	PromptID             string          `json:"prompt_id"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	ToolUseID            string          `json:"tool_use_id"`
	LastAssistantMessage string          `json:"last_assistant_message"`
}

// nsSlugRe is Unicode-aware (\p{L}\p{N}, not a-z0-9): a CJK/Cyrillic/etc.
// project directory earns its own namespace instead of every non-ASCII
// basename collapsing into the single "agent-default" bucket. There is
// no namespace format validator in internal/memory (namespaces are a
// free-form TEXT UNIQUE column, checked directly against the schema) so
// nothing downstream constrains this to ASCII.
var nsSlugRe = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// AgentNamespace derives the capture namespace from the hook's cwd:
// "agent-" + a Unicode-aware slug of the directory basename (letters and
// numbers kept, lowercased; everything else collapses to a hyphen). If
// the basename has no letters or numbers to slug from (cwd "/", ".", or
// a punctuation-only name) the namespace instead falls back to "agent-"
// + an 8-hex fnv32a hash of the full cwd - deterministic, and disjoint
// from the literal "agent-default" bucket. An empty cwd (the hook sent
// none) is the only input that maps to "agent-default".
//
// Tradeoff: two different absolute paths that share a basename (e.g.
// "/home/alice/api" and "/home/bob/api") collapse into the same
// namespace. That's accepted: this namespace is a coarse per-project
// grouping for hook capture, not a strict per-checkout partition.
func AgentNamespace(cwd string) string {
	if cwd == "" {
		return "agent-default"
	}
	base := filepath.Base(cwd)
	slug := strings.Trim(nsSlugRe.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if slug == "" {
		return "agent-" + cwdHash(cwd)
	}
	return "agent-" + slug
}

// cwdHash is an 8-hex-digit fnv32a hash of s, used as AgentNamespace's
// fallback slug when the basename contributes no letters or numbers.
func cwdHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

// idSlugRe is deliberately stricter than nsSlugRe (no case folding needed:
// IDs are opaque tokens, not display text) but shares the same purpose -
// collapsing anything that isn't alphanumeric/hyphen/underscore so a
// hostile session_id/tool_use_id/prompt_id (e.g. containing "/" or "..")
// can never inject extra key segments and write outside the caller's own
// /agent-sessions/<sid>/ subtree.
var idSlugRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// sanitizeID confines an attacker-controlled hook ID to characters that are
// safe inside a memory key path segment.
func sanitizeID(s string) string {
	return strings.Trim(idSlugRe.ReplaceAllString(s, "-"), "-")
}

const hookClip = 2000

// claudeSessionStartSourceReasons is Claude Code's own SessionStart
// "source" enum (code.claude.com/docs/en/hooks: "startup"|"resume"|
// "clear"|"compact"|"fork"). These describe *why* a genuine Claude Code
// session started, not *who* is driving the hook - they must never be
// mistaken for an agent identity a translator (e.g. hookcli's Cursor
// translator) puts in that same "source" JSON field. Without this guard,
// a real Claude Code SessionStart event - which always carries one of
// these values - would derive SourceRef "startup" instead of
// "claude-code", corrupting provenance for every genuine session start.
var claudeSessionStartSourceReasons = map[string]bool{
	"startup": true, "resume": true, "clear": true, "compact": true, "fork": true,
}

// sourceRefFor derives the fact's SourceRef from the hook envelope's
// optional "source" field: sanitized with the same allowlist used for IDs
// (sanitizeID - source is attacker-influenced the same way session_id/
// tool_use_id are), defaulting to "claude-code" when absent, empty after
// sanitizing, or when it's one of Claude Code's own SessionStart reason
// values rather than an actual agent identity (see
// claudeSessionStartSourceReasons). A hostile source ("../evil", control
// characters) is sanitized down to an allowlisted string or the default -
// this is metadata on a well-formed payload, never a 400.
func sourceRefFor(source string) string {
	s := sanitizeID(source)
	if s == "" || claudeSessionStartSourceReasons[s] {
		return "claude-code"
	}
	return s
}

// clipStr truncates s to at most n runes, appending an ellipsis when
// truncated. Cuts on a rune boundary (mirrors memory.clipBody, which is
// unexported to that package) so a multibyte character is never split.
func clipStr(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i] + "..."
		}
		count++
	}
	return s
}

// agentCaptureTTL bounds how long a raw hook capture lives under
// /agent-sessions/. This is ephemeral capture by design, not durable
// knowledge: it exists so the consolidation/context layers have raw
// material to extract from, and nothing else is expected to keep reading
// it past this window. Without an expiry the /agent-sessions/ subtree
// grows unbounded (every prompt, tool call, and transcript, forever);
// SweepRetention only reaps superseded/dead rows, so a live capture
// needs its own ExpiresAt to ever be swept.
const agentCaptureTTL = 30 * 24 * time.Hour

// maxAgentHookBody caps the hook request body Claude Code can send us.
// 5MB is generous for tool payloads (large file reads/diffs included)
// while still bounding worst-case memory use per request.
const maxAgentHookBody = 5 << 20

// handleAgentHook is the capture sink for Claude Code hook events
// (SessionStart, UserPromptSubmit, PostToolUse, Stop): each recognized
// event is written as a fact under /agent-sessions/<sid>/... in the
// namespace derived from the hook's cwd, expiring after agentCaptureTTL.
// Unknown events, and events whose session_id/prompt_id/tool_use_id
// sanitize down to empty (so no stable, non-colliding key segment can be
// formed - see sanitizeID), are acknowledged as ignored (200) rather
// than erroring: a hook script must never see capture failures turn
// into session-breaking errors on the Claude Code side. Content flagged
// by the write-time secret scrubber is a 200 blocked. Malformed JSON or
// a body over maxAgentHookBody is a 400 (we can't even parse the
// envelope). Any other failure to write is a genuine store failure and
// is a 500.
func (s *Server) handleAgentHook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAgentHookBody)
	var in agentHookIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sid := sanitizeID(in.SessionID)
	if sid == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	ns := AgentNamespace(in.CWD)
	if override := r.URL.Query().Get("ns"); override != "" {
		if !validNamespace(override) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad ns"})
			return
		}
		ns = override
	}
	expiresAt := time.Now().Add(agentCaptureTTL)

	var key, body string
	switch in.HookEventName {
	case "SessionStart":
		key = "/agent-sessions/" + sid + "/start"
		body = clipStr("cwd="+in.CWD+" source="+in.Source, hookClip)
	case "UserPromptSubmit":
		pid := sanitizeID(in.PromptID)
		if pid == "" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
			return
		}
		key = "/agent-sessions/" + sid + "/prompt-" + pid
		body = clipStr(in.Prompt, hookClip)
	case "PostToolUse":
		tuid := sanitizeID(in.ToolUseID)
		if tuid == "" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
			return
		}
		key = "/agent-sessions/" + sid + "/tool-" + tuid
		body = in.ToolName + ": " + clipStr(string(in.ToolInput), hookClip) +
			"\n" + clipStr(string(in.ToolResponse), hookClip)
	case "Stop":
		key = "/agent-sessions/" + sid + "/stop"
		body = clipStr(in.LastAssistantMessage, hookClip)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	f, err := s.mem.Write(r.Context(), memory.WriteInput{
		Namespace: ns, Key: key, Body: body,
		Author: "agent-hook", Writer: "agent-hook", SourceRef: sourceRefFor(in.Source),
		ExpiresAt: &expiresAt,
	})
	if err != nil {
		if errors.Is(err, memory.ErrSensitiveContent) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "blocked", "namespace": ns})
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stored", "namespace": ns, "key": f.Key})
}

// agentContextDefaultMaxTokens is the token budget handleAgentContext uses
// when the caller's max_tokens is absent or non-positive: agents calling
// this endpoint at session start typically don't know (or care to tune) a
// token ceiling, so a sane default keeps the endpoint useful without one.
const agentContextDefaultMaxTokens = 2000

// agentContextPoolCap bounds the candidate pool handleAgentContext ranks
// before applying the token budget, mirroring Profile's "rank a bounded
// pool, then truncate" approach rather than sorting the full namespace.
const agentContextPoolCap = 20

// agentContextSearchOverfetch multiplies agentContextPoolCap for the q
// (hybrid-search) path's initial HybridSearchScored request: the
// /agent-sessions/ and /entities/ exclusion is applied after the search
// hit, so requesting only agentContextPoolCap hits up front risks a
// namespace dominated by session captures filtering down to zero durable
// facts even though real matches exist further down the ranked results.
const agentContextSearchOverfetch = 3

// agentContextEntityNameClip and agentContextFactBodyClip bound how many
// runes of an entity name / fact body survive into the assembled context
// block. Every PostToolUse capture (and any fact write in general) is
// attacker-influenced and multi-line by construction; without a hard clip
// a single fact could forge extra "- [key] ..." bullet lines or "##"
// headers into the context text that downstream consumers parse line by
// line.
const (
	agentContextEntityNameClip = 120
	agentContextFactBodyClip   = 500
	// agentContextSummaryClip is larger than the fact clip: a rolling
	// session summary is a narrative paragraph, and clipping it to fact
	// size would defeat its purpose. Still bounded - it counts against
	// the same token budget as everything else.
	agentContextSummaryClip = 1500
)

// contextControlCharsRe matches newlines and other C0/DEL control
// characters: sanitizeContextLine collapses runs of these to a single
// space before clipping, so multi-line content can never inject a line
// break into the assembled context block.
var contextControlCharsRe = regexp.MustCompile(`[\x00-\x1f\x7f]+`)

// sanitizeContextLine makes s safe to embed as (part of) a single line of
// the assembled context block: every run of newlines/control characters
// collapses to one space, then the result is clipped to at most n runes
// (rune-safe, see clipStr). Applied to both entity names and fact bodies
// at assembly time, so forged multi-line content can never produce forged
// bullets or headers downstream.
func sanitizeContextLine(s string, n int) string {
	return clipStr(strings.TrimSpace(contextControlCharsRe.ReplaceAllString(s, " ")), n)
}

// agentContextExcludedPrefix reports whether key belongs to a subtree
// handleAgentContext's fact pool always excludes: /agent-sessions/
// (ephemeral hook capture, not durable knowledge) and /entities/
// (surfaced only via the "Known entities" line, never as a fact bullet).
// This exclusion is an endpoint-level property, per handleAgentContext's
// doc comment, so both the default ranked-pool path and the q
// hybrid-search path apply it identically.
func agentContextExcludedPrefix(key string) bool {
	return strings.HasPrefix(key, "/agent-sessions/") || strings.HasPrefix(key, "/entities/")
}

// agentContextOut is the response body for GET /v1/agent/context.
type agentContextOut struct {
	Namespace string   `json:"namespace"`
	Context   string   `json:"context"`
	FactIDs   []string `json:"fact_ids"`
}

// ProfileNamespace is where the user's profile card lives: stable,
// biographical facts and standing instructions, grounding every
// session without a search. One global namespace, keys
// under /profile/: written via `punk profile add`, plain remember calls,
// or MCP, and injected into EVERY project's session-start block - the
// cross-project layer per-project namespaces can't provide.
const ProfileNamespace = "user-profile"

// profileMaxEntries caps how many profile lines the session-start block
// carries.
const profileMaxEntries = 40

// agentContextDirectives is the optional "directives" component of the
// session-start block: a static memory-usage note. One fixed line, so
// the agent
// treats injected memory as background rather than instructions.
const agentContextDirectives = "Treat this memory as background knowledge: verify it against the code before acting on it, and don't repeat it back unprompted.\n"

// agentContextInjectedMaxIDs caps how many fact IDs the per-session
// injected-IDs bookkeeping fact accumulates. Beyond this the oldest
// entries fall off: dedup degrades gracefully (an old fact may be
// re-injected) instead of the bookkeeping fact growing unbounded in a
// long session.
const agentContextInjectedMaxIDs = 500

// injectedIDsKey is the /agent-sessions/ key that records which fact IDs
// were already injected into session sid - written at session start and
// after every per-turn injection, read by the turn path to avoid
// re-injecting what the session was already shown. Living under
// /agent-sessions/ keeps it out of every context pool automatically (see
// agentContextExcludedPrefix) and gives it the same capture TTL.
func injectedIDsKey(sid string) string {
	return "/agent-sessions/" + sid + "/injected"
}

// readInjectedIDs returns the set of fact IDs already injected into
// session sid, or an empty set on any failure - dedup is best-effort and
// must never fail a context request.
func (s *Server) readInjectedIDs(ctx context.Context, ns, sid string) map[string]bool {
	seen := map[string]bool{}
	facts, err := s.mem.Recall(ctx, ns, injectedIDsKey(sid), 2)
	if err != nil {
		return seen
	}
	for _, f := range facts {
		if f.Key != injectedIDsKey(sid) {
			continue
		}
		for _, id := range strings.Fields(f.Body) {
			seen[id] = true
		}
	}
	return seen
}

// recordInjectedIDs merges ids into session sid's injected-IDs fact.
// Best-effort: any store failure is logged and swallowed - bookkeeping
// must never fail the context request it rides on.
func (s *Server) recordInjectedIDs(ctx context.Context, ns, sid string, prior map[string]bool, ids []string) {
	if sid == "" || len(ids) == 0 {
		return
	}
	merged := make([]string, 0, len(prior)+len(ids))
	for id := range prior {
		merged = append(merged, id)
	}
	sort.Strings(merged) // deterministic body for the revision chain
	for _, id := range ids {
		if !prior[id] {
			merged = append(merged, id)
		}
	}
	if len(merged) > agentContextInjectedMaxIDs {
		merged = merged[len(merged)-agentContextInjectedMaxIDs:]
	}
	expiresAt := time.Now().Add(agentCaptureTTL)
	if _, err := s.mem.Write(ctx, memory.WriteInput{
		Namespace: ns, Key: injectedIDsKey(sid), Body: strings.Join(merged, " "),
		Author: "agent-context", Writer: "agent-context",
		ExpiresAt: &expiresAt,
	}); err != nil && s.log != nil {
		s.log.Warn("record injected ids failed", "ns", ns, "sid", sid, "err", err)
	}
}

// handleAgentContext assembles a ready-to-inject context block for an
// agent starting work on a project: the namespace's known entities plus
// its most important/recent facts (or, with a q param, hybrid-search hits
// for that query), trimmed to a token budget. ns takes precedence when
// both ns and cwd are given; cwd is run through AgentNamespace the same
// way the hook capture endpoint derives it, so a caller that only knows
// its cwd lands in the same namespace its hook events already populated.
// Neither ns nor cwd given is a 400 (malformed client input - nothing to
// look up); a genuine failure talking to the store past that point is a
// 500. /agent-sessions/ (ephemeral hook capture, not durable knowledge)
// and /entities/ (surfaced separately as the "Known entities" line, never
// as a fact bullet) are excluded from both the ranked pool and q's
// hybrid-search hits (see agentContextExcludedPrefix). Entity names and
// fact bodies are sanitized and clipped before assembly (see
// sanitizeContextLine) so forged multi-line content can't inject fake
// bullets/headers and a single oversized fact can't starve every other
// fact out of the token budget. max_tokens covers the entity line plus
// fact bodies, not the "- [key] " bullet markup around each fact. When
// there are zero entities and zero facts, Context is the empty string
// (not just the bare "## Project memory (ns)\n" header) - namespace and
// an empty fact_ids are still returned - so a caller like hookcli can
// treat "nothing to show" as "skip injection" rather than printing a
// useless header-only block every session.
func (s *Server) handleAgentContext(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("ns")
	if ns == "" {
		if cwd := r.URL.Query().Get("cwd"); cwd != "" {
			ns = AgentNamespace(cwd)
		}
	}
	if ns == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ns or cwd required"})
		return
	}
	sid := sanitizeID(r.URL.Query().Get("sid"))
	if r.URL.Query().Get("mode") == "turn" {
		s.handleAgentTurnContext(w, r, ns, sid)
		return
	}
	maxTokens := queryMaxTokens(r)
	if maxTokens <= 0 {
		maxTokens = agentContextDefaultMaxTokens
	}

	// Component set for the session-start block. Layout order is
	// fixed - directives, entities, facts - regardless of list order; the
	// list only selects which components appear.
	components := s.inject
	if len(components) == 0 {
		components = []string{"profile", "summary", "entities", "facts"}
	}
	var wantDirectives, wantProfile, wantSummary, wantEntities, wantFacts bool
	for _, c := range components {
		switch c {
		case "directives":
			wantDirectives = true
		case "profile":
			wantProfile = true
		case "summary":
			wantSummary = true
		case "entities":
			wantEntities = true
		case "facts":
			wantFacts = true
		}
	}

	p, err := s.mem.Profile(r.Context(), ns)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	var facts []memory.Fact
	if q := r.URL.Query().Get("q"); !wantFacts {
		// facts component disabled by memory.inject: skip pool assembly
		// entirely - the block is entities (and directives) only.
	} else if q != "" {
		// Over-fetch (agentContextSearchOverfetch x the pool cap) before
		// filtering: HybridSearchScored's own limit is applied before the
		// /agent-sessions/ and /entities/ exclusion below, so in a
		// namespace dominated by session captures a plain limit-agentContextPoolCap
		// query could fill the whole result with excluded hits and filter
		// down to zero durable facts. Fetching a larger candidate pool
		// first, then filtering, then capping to agentContextPoolCap
		// keeps the q path's effective candidate pool comparable to the
		// default ranked-pool path's.
		scored, err := s.mem.HybridSearchScored(r.Context(), ns, q, agentContextPoolCap*agentContextSearchOverfetch, 0)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		for _, sf := range scored {
			if agentContextExcludedPrefix(sf.Key) {
				continue
			}
			facts = append(facts, sf.Fact)
			if len(facts) == agentContextPoolCap {
				break
			}
		}
	} else {
		all, err := s.mem.Recall(r.Context(), ns, "/", 1000)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		var pool []memory.Fact
		for _, f := range all {
			if agentContextExcludedPrefix(f.Key) {
				continue
			}
			pool = append(pool, f)
		}
		// Important-first (Importance >= 0.5 bucket), then most-recent,
		// with a key-asc tie-break. In practice the tie-break is
		// unreachable: Recall already orders its result by key, and real
		// writes carry nanosecond-resolution CreatedAt, so ties don't
		// occur outside a fixed test clock. It stays as defense in depth
		// against a coarser clock or a future Recall order change, not
		// because it currently changes output.
		sort.SliceStable(pool, func(a, b int) bool {
			ia, ib := pool[a].Importance >= 0.5, pool[b].Importance >= 0.5
			if ia != ib {
				return ia
			}
			if !pool[a].CreatedAt.Equal(pool[b].CreatedAt) {
				return pool[a].CreatedAt.After(pool[b].CreatedAt)
			}
			return pool[a].Key < pool[b].Key
		})
		if len(pool) > agentContextPoolCap {
			pool = pool[:agentContextPoolCap]
		}
		facts = pool
	}

	// Sanitize and clip fact bodies before budgeting: a single oversized
	// or multi-line fact must not (a) consume the whole token budget by
	// itself - TokenBudget stops at the first fact that doesn't fit, so
	// an unclipped 9000-rune fact ahead of a short one would empty the
	// rest of the context - or (b) forge extra bullet/header lines into
	// the assembled text.
	for i := range facts {
		facts[i].Body = sanitizeContextLine(facts[i].Body, agentContextFactBodyClip)
	}

	var b strings.Builder
	b.WriteString("## Project memory (" + ns + ")\n")
	if wantDirectives {
		b.WriteString(agentContextDirectives)
	}
	profileCount := 0
	if wantProfile {
		// The profile card reads from the
		// global ProfileNamespace, not the project namespace: stable
		// user-level facts follow the user across every project. A read
		// failure degrades to no card - the block must not 500 because
		// the profile namespace is unreadable.
		if cardFacts, err := s.mem.Recall(r.Context(), ProfileNamespace, "/profile/", profileMaxEntries); err == nil && len(cardFacts) > 0 {
			b.WriteString("About the user:\n")
			for _, f := range cardFacts {
				b.WriteString("- " + sanitizeContextLine(f.Body, agentContextFactBodyClip) + "\n")
				profileCount++
			}
		}
	}
	summaryCount := 0
	if wantSummary {
		// The most recent session's rolling summary gives
		// the next session narrative continuity the fact bullets can't.
		// Summaries live under /agent-sessions/ - excluded from the fact
		// pool - so this is the one component that reads that subtree.
		if sf, err := s.mem.LatestSessionSummary(r.Context(), ns); err == nil && sf != nil {
			b.WriteString("Last session: " + sanitizeContextLine(sf.Body, agentContextSummaryClip) + "\n")
			summaryCount++
		}
	}
	entityCount := 0
	if wantEntities && len(p.TopEntities) > 0 {
		entityCount = len(p.TopEntities)
		names := make([]string, len(p.TopEntities))
		for i, e := range p.TopEntities {
			names[i] = sanitizeContextLine(e.Name, agentContextEntityNameClip)
		}
		b.WriteString("Known entities: " + strings.Join(names, ", ") + "\n")
	}

	// The header and entities line count against maxTokens too: without
	// subtracting their cost, a namespace with many/large entities could
	// consume the whole budget before a single fact is even considered -
	// the entity set isn't bounded by the caller's max_tokens the way the
	// fact pool is.
	factBudget := maxTokens - memory.EstimateTokens(b.String())
	if factBudget < 0 {
		factBudget = 0
	}
	// TokenBudget treats maxTokens<=0 as "no limit" (returns every fact
	// unfiltered), not "zero facts fit" - call it only when there is
	// budget left, otherwise a fully-consumed budget would perversely let
	// every fact through instead of none.
	if factBudget > 0 {
		facts = memory.TokenBudget(facts, factBudget)
	} else {
		facts = nil
	}

	ids := make([]string, 0, len(facts))
	for _, f := range facts {
		b.WriteString("- [" + f.Key + "] " + f.Body + "\n")
		ids = append(ids, f.ID)
	}

	// With zero entities and zero facts, b is just the "## Project memory
	// (ns)\n" header - a non-empty string that would otherwise still be
	// injected into every session as a useless block. Report the true
	// "nothing to show" state as an empty Context (namespace and the
	// - possibly empty - fact_ids are still returned) so hookcli's
	// existing empty-context guard skips injection.
	context := b.String()
	if profileCount == 0 && summaryCount == 0 && entityCount == 0 && len(facts) == 0 {
		context = ""
	}

	// Session-start bookkeeping for per-turn dedup: when
	// the hook told us its session id, remember which fact IDs this block
	// carried so mode=turn never re-injects them. Merging with any prior
	// record keeps resumed sessions (SessionStart source=resume) additive.
	if sid != "" && len(ids) > 0 && context != "" {
		s.recordInjectedIDs(r.Context(), ns, sid, s.readInjectedIDs(r.Context(), ns, sid), ids)
	}

	writeJSON(w, http.StatusOK, agentContextOut{Namespace: ns, Context: context, FactIDs: ids})
}

// agentTurnContextHeader heads the per-turn injected block. Deliberately
// distinct from the session-start "## Project memory" header so the two
// blocks read as different things in the transcript.
const agentTurnContextHeader = "## Relevant memory\n"

// handleAgentTurnContext is GET /v1/agent/context?mode=turn - the
// per-turn, prompt-scoped injection path: a fresh context slice on
// every prompt, not just at session start. It answers hybrid-search hits for q, minus /agent-sessions/
// and /entities/ (same exclusions as the session block) and minus every
// fact ID already injected into this session (see injectedIDsKey),
// budgeted to memory.turn_context_tokens (caller max_tokens overrides).
// A disabled feature (turnTokens 0), an empty q, or zero surviving facts
// all answer an empty Context so hook clients skip injection; only a
// genuine store failure is a 500.
func (s *Server) handleAgentTurnContext(w http.ResponseWriter, r *http.Request, ns, sid string) {
	q := r.URL.Query().Get("q")
	if s.turnTokens <= 0 || q == "" {
		writeJSON(w, http.StatusOK, agentContextOut{Namespace: ns, Context: "", FactIDs: []string{}})
		return
	}
	maxTokens := queryMaxTokens(r)
	if maxTokens <= 0 {
		maxTokens = s.turnTokens
	}

	seen := map[string]bool{}
	if sid != "" {
		seen = s.readInjectedIDs(r.Context(), ns, sid)
	}

	scored, err := s.mem.HybridSearchScored(r.Context(), ns, q, agentContextPoolCap*agentContextSearchOverfetch, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var facts []memory.Fact
	for _, sf := range scored {
		if agentContextExcludedPrefix(sf.Key) || seen[sf.ID] {
			continue
		}
		facts = append(facts, sf.Fact)
		if len(facts) == agentContextPoolCap {
			break
		}
	}
	for i := range facts {
		facts[i].Body = sanitizeContextLine(facts[i].Body, agentContextFactBodyClip)
	}

	var b strings.Builder
	b.WriteString(agentTurnContextHeader)
	factBudget := maxTokens - memory.EstimateTokens(b.String())
	if factBudget > 0 {
		facts = memory.TokenBudget(facts, factBudget)
	} else {
		facts = nil
	}

	ids := make([]string, 0, len(facts))
	for _, f := range facts {
		b.WriteString("- [" + f.Key + "] " + f.Body + "\n")
		ids = append(ids, f.ID)
	}

	context := b.String()
	if len(facts) == 0 {
		context = ""
	} else if sid != "" {
		s.recordInjectedIDs(r.Context(), ns, sid, seen, ids)
	}
	writeJSON(w, http.StatusOK, agentContextOut{Namespace: ns, Context: context, FactIDs: ids})
}

// validNamespace accepts what AgentNamespace and ProjectNamespace emit:
// lowercase letters/digits (any script) with single dashes, bounded length.
func validNamespace(s string) bool {
	return s != "" && len(s) <= 128 && nsSlugRe.ReplaceAllString(strings.ToLower(s), "-") == strings.ToLower(s)
}
