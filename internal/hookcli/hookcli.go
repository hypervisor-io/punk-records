// Package hookcli is the client side of the agent-memory layer: it runs as
// an agent hook process (a coding agent like Claude Code, or a general
// assistant like Hermes or OpenClaw; Claude Code natively via Run, any other
// agent's native hook payload translated first via RunFrom/Normalize),
// forwarding the (possibly translated) hook payload to a punk-records
// server and, on SessionStart in Claude Code mode only, printing the
// additionalContext JSON that Claude Code injects into the session.
// Non-Claude RunFrom modes have no additionalContext contract and never
// print that envelope; the one exception is Cursor's beforeSubmitPrompt,
// which prints exactly {"continue":true} on its own separate stdout
// contract - see RunFrom's doc comment for why. Every other non-Claude
// event still prints nothing at all. Antigravity CLI and GitHub Copilot CLI
// are each handled by their own entry point - RunFromAntigravity and
// RunFromCopilot respectively - rather than through RunFrom/Normalize
// directly; see each function's own doc comment for why. Failures are
// silent by design (a dead
// memory server must never break the user's coding session): every
// network/decode failure past stdin is swallowed and only noted on
// stderr. The 2s client timeout keeps hook overhead bounded and stays
// well inside hook timeout budgets without citing an exact Claude Code
// number here.
package hookcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpClient is a package var (not a const) so tests can swap its
// Transport to simulate mid-run failures without touching global state
// beyond the test's own defer-restore.
var httpClient = &http.Client{Timeout: 2 * time.Second}

// maxStdinBytes bounds how much of stdin Run reads: Claude Code hook
// payloads are small JSON envelopes, not file contents, so this is
// generous headroom, not a real limit in practice.
const maxStdinBytes = 1 << 20 // 1MB

// maxRespBytes bounds how much of any single HTTP response body Run reads
// or drains - both the /v1/agent/context response it decodes and the
// /v1/agent/hooks response it merely drains and discards. A hostile or
// misconfigured server must not be able to balloon the hook process's
// memory by streaming an unbounded body; the context endpoint's own
// context assembly is token-budgeted well under this, so any real
// response is far smaller. One shared cap is fine: both bodies are small
// JSON envelopes with no reason to need different bounds.
const maxRespBytes = 1 << 20 // 1MB

type hookPayload struct {
	HookEventName string `json:"hook_event_name"`
	CWD           string `json:"cwd"`
	SessionID     string `json:"session_id"`
	Prompt        string `json:"prompt"`
}

// turnQueryClip bounds how many runes of the user's prompt ride the
// mode=turn context request as its q param: the prompt's opening is what
// carries the topical signal for hybrid search, and an unbounded prompt
// (a pasted file, a huge diff) would just bloat the request URL.
const turnQueryClip = 600

// clipRunes truncates s to at most n runes, cutting on a rune boundary.
func clipRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

type contextResp struct {
	Context string `json:"context"`
}

// Run reads one hook payload from stdin, forwards it to baseURL's
// /v1/agent/hooks, and on SessionStart also fetches
// /v1/agent/context?cwd=... and writes the additionalContext envelope
// Claude Code expects to out. apiKey, when non-empty, is sent as a Bearer
// Authorization header on both requests; when empty, no Authorization
// header is sent at all (not an empty-token header). baseURL has any
// trailing slash trimmed here (not just by callers) so a
// caller-supplied "http://host/" never produces a double-slash request
// path - the real chi router doesn't CleanPath by default and 404s on
// that with no useful diagnostic.
//
// Run always returns nil. Every failure mode - malformed stdin, an
// unreachable server, a non-200 or malformed context response - is
// reported to errw and otherwise swallowed: a hook script's exit code and
// stdout drive Claude Code's session behavior directly, so a capture or
// context-fetch failure must never surface as a hook error and must never
// print a garbage or empty injection envelope to stdout. The error return
// is kept in the signature for callers that want to distinguish failure
// classes in the future; today none does.
func Run(stdin io.Reader, baseURL, apiKey string, out, errw io.Writer) error {
	baseURL = strings.TrimRight(baseURL, "/")
	raw, err := io.ReadAll(io.LimitReader(stdin, maxStdinBytes))
	if err != nil {
		fmt.Fprintln(errw, "punk hook: read stdin:", err)
		return nil
	}
	var p hookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		fmt.Fprintln(errw, "punk hook: bad payload:", err)
		return nil
	}

	forwardHook(baseURL, apiKey, raw, errw)

	// Two events inject context, each through its own params: SessionStart
	// gets the session-start block (entities + important/recent facts),
	// and UserPromptSubmit gets the per-turn, prompt-scoped block
	// (empty unless the server's memory.turn_context_tokens
	// is set). Both print the same hookSpecificOutput envelope - Claude
	// Code accepts additionalContext on both events - and both skip
	// printing entirely on an empty or failed fetch.
	var params url.Values
	switch p.HookEventName {
	case "SessionStart":
		params = sessionParams(p.CWD, p.SessionID)
	case "UserPromptSubmit":
		if p.Prompt == "" {
			return nil
		}
		params = turnParams(p.CWD, p.SessionID, p.Prompt)
	default:
		return nil
	}

	ctxBody, ok := fetchContext(baseURL, apiKey, params, errw)
	if !ok || ctxBody == "" {
		return nil
	}

	outJSON, err := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     p.HookEventName,
			"additionalContext": ctxBody,
		},
	})
	if err != nil {
		fmt.Fprintln(errw, "punk hook: encode injection payload:", err)
		return nil
	}
	fmt.Fprintln(out, string(outJSON))
	return nil
}

// forwardHook POSTs the raw hook payload to /v1/agent/hooks. Failure -
// including a non-200 status, e.g. a bad/expired API key - is noted on errw
// and otherwise ignored: capture is fire-and-forget from the hook process's
// point of view, so this stays fail-open and never affects Run's return
// value or stdout.
func forwardHook(baseURL, apiKey string, raw []byte, errw io.Writer) {
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/agent/hooks", bytes.NewReader(raw))
	if err != nil {
		fmt.Fprintln(errw, "punk hook: build request:", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	setAuth(req, apiKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Fprintln(errw, "punk hook: capture failed:", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBytes))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(errw, "punk hook: capture: unexpected status %d\n", resp.StatusCode)
	}
}

// sessionParams builds the query for a session-start context fetch: cwd
// always, sid when the payload carried a session id (it feeds the
// server's per-turn dedup bookkeeping; an absent sid just means no
// dedup, never a failure).
func sessionParams(cwd, sid string) url.Values {
	v := url.Values{"cwd": {cwd}}
	if sid != "" {
		v.Set("sid", sid)
	}
	return v
}

// turnParams builds the query for a per-turn (mode=turn) context fetch:
// prompt-scoped hybrid search, deduped against what session sid was
// already shown. An empty prompt yields a q the server answers with an
// empty context, so callers may pass it through without pre-checking.
func turnParams(cwd, sid, prompt string) url.Values {
	v := url.Values{"cwd": {cwd}, "mode": {"turn"}, "q": {clipRunes(prompt, turnQueryClip)}}
	if sid != "" {
		v.Set("sid", sid)
	}
	return v
}

// fetchContext GETs /v1/agent/context with the given query params and
// returns the decoded context string. ok is false on any failure
// (network error, non-200 status, or undecodable body) - the caller must
// treat that identically to "nothing to inject", never as an empty
// string to print.
func fetchContext(baseURL, apiKey string, params url.Values, errw io.Writer) (string, bool) {
	req, err := http.NewRequest(http.MethodGet,
		baseURL+"/v1/agent/context?"+params.Encode(), nil)
	if err != nil {
		fmt.Fprintln(errw, "punk hook: build context request:", err)
		return "", false
	}
	setAuth(req, apiKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Fprintln(errw, "punk hook: context fetch failed:", err)
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(errw, "punk hook: context fetch: unexpected status %d\n", resp.StatusCode)
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBytes)) //nolint:errcheck
		return "", false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		fmt.Fprintln(errw, "punk hook: context response read:", err)
		return "", false
	}
	// json.Unmarshal (unlike json.Decoder.Decode on a stream) requires the
	// entire body to be one valid JSON value with no trailing garbage -
	// same read cost since the body is already bounded, but a stricter
	// parse of it.
	var ctx contextResp
	if err := json.Unmarshal(body, &ctx); err != nil {
		fmt.Fprintln(errw, "punk hook: context response decode:", err)
		return "", false
	}
	return ctx.Context, true
}

// RunFrom is Run's multi-agent entry point: from identifies which agent
// produced stdin's native payload, matched case-insensitively (--from
// Claude, --from CURSOR etc. all resolve the same as their lowercase
// spelling). from="" or from="claude" (or its alias "claude-code", for
// callers that prefer naming every agent explicitly rather than relying on
// the empty-string default) is byte-exact unchanged behavior - it
// delegates straight to Run without ever reading stdin itself, so Run's
// own io.ReadAll is the only read and the raw-body-forwarded-unchanged
// guarantee (see forwardHook) is untouched.
//
// Any other from is normalized first (see Normalize): the translated
// Claude-shaped envelope is forwarded to /v1/agent/hooks exactly like
// Run's claude-mode forward, but RunFrom never calls fetchContext and
// never prints the additionalContext injection envelope. Cursor (and any
// other non-Claude agent) has no additionalContext contract - Claude Code
// alone is the reader that turns hookSpecificOutput.additionalContext on
// stdout into injected context, so printing that envelope in a non-Claude
// hook process would just be garbage on that agent's stdout. Context
// injection for other agents is a separate feature, not part of this
// translation path.
//
// Cursor's own stdout contract IS honored, though, for the one event that
// has one: cursor.com/docs/agent/hooks documents beforeSubmitPrompt as a
// BLOCKING hook - Cursor waits for {"continue": true|false} on stdout
// before letting the prompt proceed, and an exit code alone is not enough
// (a nonzero exit is a Cursor-defined "block", not merely a punk-side hook
// failure). Since punk only observes hook traffic and must never block or
// cancel a user's prompt - not even when the memory server is down, and
// not even when the payload fails to normalize - from == "cursor" and the
// ORIGINAL (pre-translation) event is "beforeSubmitPrompt" always prints
// exactly `{"continue":true}` plus a newline, and never
// `{"continue":false}`. That check is hoisted to run BEFORE Normalize is
// even consulted for its ok/err result (see isCursorBlocking below), not
// gated on a successful translate-and-forward: isCursorBeforeSubmitPrompt
// decodes raw with its own minimal, permissive struct (just
// hook_event_name/cwd as strings), so it still correctly identifies a
// beforeSubmitPrompt event even when Normalize's stricter cursorPayload
// decode fails on some OTHER field's shape (e.g. workspace_roots sent as a
// string instead of an array) - a case that is "malformed" with respect to
// translateCursor's struct but not to Cursor's own blocking-hook contract,
// and is exactly the case where staying silent would leave a user's prompt
// hanging. Every other cursor event (sessionStart, postToolUse,
// afterFileEdit, stop, sessionEnd) keeps stdout completely empty, same as
// before this contract existed.
//
// Two more cursor-mode paths cannot recover hook_event_name at all and
// also print {"continue":true} unconditionally, on the same "never leave
// a blocking hook hanging" reasoning: a stdin read error (nothing was
// even read), and stdin landing exactly at maxStdinBytes with a payload
// that then fails to parse (the cap truncated it mid-value - see Run's
// own maxStdinBytes doc comment). Both are safe to reply to
// unconditionally, even for the five non-blocking cursor events, because
// beforeSubmitPrompt is the only one of the six wired events that reads
// stdout at all; the other five simply never look at it.
//
// Like Run, RunFrom always returns nil: an unrecognized from, a malformed
// native payload, or any network failure is noted on errw and otherwise
// swallowed, fail-open, for the same reason Run is - a dead memory server
// or a bad --from flag must never break the user's coding session.
func RunFrom(from string, stdin io.Reader, baseURL, apiKey string, out, errw io.Writer) error {
	fromKey := strings.ToLower(from)
	if fromKey == "" || fromKey == "claude" || fromKey == "claude-code" {
		return Run(stdin, baseURL, apiKey, out, errw)
	}
	baseURL = strings.TrimRight(baseURL, "/")
	raw, err := io.ReadAll(io.LimitReader(stdin, maxStdinBytes))
	if err != nil {
		fmt.Fprintln(errw, "punk hook: read stdin:", err)
		// A stdin read failure leaves raw empty/partial, so there is no
		// hook_event_name to recover at all - we cannot tell whether this
		// was a beforeSubmitPrompt Cursor is blocked waiting on. Printing
		// {"continue":true} unconditionally for cursor mode here is safe:
		// beforeSubmitPrompt is the only one of the six wired Cursor
		// events that reads stdout at all (see this function's doc
		// comment), so the other five silently ignore a print they never
		// look at.
		if fromKey == "cursor" {
			fmt.Fprintln(out, `{"continue":true}`)
		}
		return nil
	}

	// Hoisted ahead of Normalize's ok/err result on purpose: this must be
	// evaluated - and, if true, acted on - independent of whether
	// translation below succeeds, so a payload that is "malformed" only
	// with respect to translateCursor's stricter struct (see this
	// function's doc comment) still gets its {"continue":true} reply.
	isCursorBlocking := fromKey == "cursor" && isCursorBeforeSubmitPrompt(raw)

	// A payload that hit the maxStdinBytes cap was truncated by Run's
	// LimitReader mid-value, so isCursorBeforeSubmitPrompt's own
	// json.Unmarshal can fail on the cut-off JSON even when the original,
	// untruncated payload really was a beforeSubmitPrompt event - the one
	// case where staying silent would leave Cursor's blocking hook
	// hanging. When the cap was hit AND the payload does not parse at
	// all, we genuinely cannot recover hook_event_name, so treat it the
	// same as the stdin-read-error case above: reply {"continue":true}
	// unconditionally for cursor mode. If the payload still parses
	// cleanly despite landing exactly at the cap (it just happened to fit
	// - the JSON is complete), isCursorBeforeSubmitPrompt's result is
	// trustworthy and this extra branch does not fire.
	if !isCursorBlocking && fromKey == "cursor" && len(raw) == maxStdinBytes {
		var probe hookPayload
		if json.Unmarshal(raw, &probe) != nil {
			isCursorBlocking = true
		}
	}

	translated, ok, err := Normalize(fromKey, raw)
	if err != nil {
		fmt.Fprintln(errw, "punk hook: normalize:", err)
		if isCursorBlocking {
			fmt.Fprintln(out, `{"continue":true}`)
		}
		return nil
	}
	if !ok {
		// Unmapped event for a recognized agent: forward nothing, exit
		// clean. This is normal (most native events have no Claude Code
		// equivalent), not a failure. beforeSubmitPrompt always has a
		// mapping (translateCursor's switch handles it unconditionally),
		// so isCursorBlocking is never true here in practice - but even if
		// it somehow were, skipping the print here would be wrong, so this
		// branch deliberately does not special-case it away.
		return nil
	}
	forwardHook(baseURL, apiKey, translated, errw)

	if isCursorBlocking {
		// This print happens unconditionally, independent of whether
		// forwardHook above succeeded: punk's fail-open guarantee applies
		// to blocking the prompt just as much as it does to capture.
		fmt.Fprintln(out, `{"continue":true}`)
	}
	return nil
}

// isCursorBeforeSubmitPrompt reports whether raw's hook_event_name field
// (both Cursor's native and Claude's translated shape use that same JSON
// tag) is the literal string "beforeSubmitPrompt" - Cursor's native name
// for the one blocking hook event, before translateCursor renamed it to
// "UserPromptSubmit" for forwarding. It decodes raw into hookPayload, a
// minimal struct of just hook_event_name/cwd as plain strings, which is
// deliberately more permissive than cursorPayload (Normalize's own decode
// target): a raw payload whose OTHER fields don't match cursorPayload's
// stricter shape (e.g. workspace_roots sent as a string instead of an
// array) still decodes cleanly here, so this reports true for a genuine
// beforeSubmitPrompt event even in cases where Normalize itself would
// return an error - which is exactly why RunFrom's caller hoists this call
// to run independent of Normalize's ok/err result rather than gating it on
// success. A parse failure (raw is not even syntactically valid JSON at
// all) yields false - there is no event name to recover from that, and
// RunFrom already notes the failure on errw via Normalize's own error.
func isCursorBeforeSubmitPrompt(raw []byte) bool {
	var p hookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return false
	}
	return p.HookEventName == "beforeSubmitPrompt"
}

// RunFromAntigravity is punk hook's Antigravity-specific entry point,
// called directly by cmdHook rather than through RunFrom/Normalize's
// generic registry dispatch, for two reasons documented at length on
// translateAntigravity (normalize.go) and the translators map's own doc
// comment there:
//
//  1. None of Antigravity CLI's documented hook payloads carry a field
//     identifying which event fired (antigravity.google/docs/hooks,
//     fetched directly and checked exhaustively). Punk's own hooks.json
//     entries (see ConnectAntigravity, connect_antigravity.go) work around
//     that by invoking a SEPARATE "punk hook --from antigravity --event
//     <Name>" command per wired event, so event is always known to the
//     CALLER (cmdHook, from its own --event flag) rather than recoverable
//     from raw stdin the way Cursor's hook_event_name field lets RunFrom's
//     shared dispatch work.
//  2. Antigravity's stdout contract is per-event and structurally
//     different from every other agent this package supports: Stop
//     requires an explicit non-"continue" decision to avoid forcing the
//     agent's execution loop to re-enter, and PreInvocation's context
//     injection uses an entirely different envelope shape
//     ({"injectSteps":[{"ephemeralMessage":...}]}) than Claude Code's
//     hookSpecificOutput/additionalContext or Cursor's {"continue":true}.
//     A self-contained function keeps this end-to-end readable and leaves
//     RunFrom, Normalize, and every existing --from cursor/claude/""
//     behavior byte-for-byte untouched - per .claude/rules/api.md's "byte-
//     exact passthrough protection for existing modes" rule, no existing
//     call site or test in this package changes at all.
//
// event must be exactly one of the three events ConnectAntigravity wires
// ("PostToolUse", "PreInvocation", "Stop" - PascalCase, matching
// Antigravity's own hooks.json event keys byte-for-byte, since cmdHook's
// --event flag value is threaded straight from the hooks.json command
// string ConnectAntigravity wrote). Any other value (including empty,
// "PreToolUse", or "PostInvocation" - see connect_antigravity.go's doc
// comments for why those two are never wired) makes every branch below a
// no-op except printAntigravityStopReply and printAntigravityPostToolUseReply,
// which themselves only ever fire for event=="Stop" and event=="PostToolUse"
// respectively.
//
// Every failure mode here is fail-open exactly like RunFrom:
// RunFromAntigravity always returns nil, and a dead memory server, a
// malformed payload, or an unreachable API must never surface as an error
// and must never leave Antigravity's execution loop hanging on a Stop hook
// waiting for a decision that never arrives - printAntigravityStopReply and
// printAntigravityPostToolUseReply are both called on every return path,
// independent of how far translation or forwarding got. PreInvocation has
// no such unconditional reply: its injectSteps output is optional, so it
// stays silent on any failure.
func RunFromAntigravity(event string, stdin io.Reader, baseURL, apiKey string, out, errw io.Writer) error {
	baseURL = strings.TrimRight(baseURL, "/")
	raw, err := io.ReadAll(io.LimitReader(stdin, maxStdinBytes))
	if err != nil {
		fmt.Fprintln(errw, "punk hook: read stdin:", err)
		printAntigravityStopReply(event, out)
		printAntigravityPostToolUseReply(event, out)
		return nil
	}

	translated, ok, err := translateAntigravity(event, raw)
	if err != nil {
		fmt.Fprintln(errw, "punk hook: normalize:", err)
		printAntigravityStopReply(event, out)
		printAntigravityPostToolUseReply(event, out)
		return nil
	}
	if !ok {
		// Either an event this translator doesn't map (PreToolUse,
		// PostInvocation, an empty/unrecognized --event) or a
		// PreInvocation whose invocationNum != 0 (translateAntigravity's
		// own session-start/first-invocation gate) - either way, nothing
		// to forward or inject.
		printAntigravityStopReply(event, out)
		printAntigravityPostToolUseReply(event, out)
		return nil
	}
	forwardHook(baseURL, apiKey, translated, errw)

	switch event {
	case "Stop":
		printAntigravityStopReply(event, out)

	case "PreInvocation":
		// Only reached when ok==true, i.e. translateAntigravity's own
		// invocationNum==0 gate already passed: this IS the conversation's
		// first model invocation, so injection fires exactly once per
		// conversation with no in-process state needed (unlike pi/
		// OpenCode's long-lived plugin hosts, punk hook is a fresh
		// subprocess per invocation with nothing to remember between
		// calls - invocationNum arriving fresh from Antigravity itself on
		// every call is what makes a stateless gate possible here).
		var env hookPayload
		if err := json.Unmarshal(translated, &env); err != nil {
			// Unreachable in practice: translateAntigravity just produced
			// these exact bytes via its own json.Marshal a few lines
			// above. Guarded anyway rather than assumed, matching this
			// package's existing discipline of never trusting a decode to
			// succeed silently (see fetchContext's own decode error path).
			fmt.Fprintln(errw, "punk hook: decode translated antigravity envelope:", err)
			return nil
		}
		ctxBody, fetched := fetchContext(baseURL, apiKey, sessionParams(env.CWD, env.SessionID), errw)
		if !fetched || ctxBody == "" {
			return nil
		}
		outJSON, err := json.Marshal(map[string]any{
			"injectSteps": []map[string]string{
				{"ephemeralMessage": ctxBody},
			},
		})
		if err != nil {
			fmt.Fprintln(errw, "punk hook: encode antigravity injection payload:", err)
			return nil
		}
		fmt.Fprintln(out, string(outJSON))

	case "PostToolUse":
		printAntigravityPostToolUseReply(event, out)
	}
	return nil
}

// printAntigravityStopReply prints Antigravity's required Stop decision
// reply - {"decision":"allow"} - whenever event is "Stop", regardless of
// whether translation or forwarding succeeded (mirrors RunFrom's own
// unconditional {"continue":true} reply for Cursor's beforeSubmitPrompt -
// see RunFrom's doc comment for the same "never leave a blocking hook
// hanging" reasoning). "allow" is deliberately never "continue":
// antigravity.google/docs/hooks documents decision:"continue" as forcing
// the agent to re-enter its execution loop instead of stopping - punk only
// observes hook traffic and must never force continuation, so this always
// picks a value from the doc's own "any other value allows the stop"
// branch of that contract. Every other event prints nothing here: Stop is
// the only one of the three wired events with a Required stdout field.
func printAntigravityStopReply(event string, out io.Writer) {
	if event == "Stop" {
		fmt.Fprintln(out, `{"decision":"allow"}`)
	}
}

// printAntigravityPostToolUseReply prints Antigravity's documented
// PostToolUse reply - "{}" - whenever event is "PostToolUse", on every
// return path (a stdin read error, a translation failure, a skipped/
// unmapped event, or the success path), mirroring printAntigravityStopReply's
// own "print unconditionally, regardless of how far translation or
// forwarding got" discipline. antigravity.google/docs/hooks documents
// PostToolUse's stdout contract explicitly as "Returns an empty JSON
// object {}" (not merely "may return"), with no documented exception for
// a failed or skipped hook invocation - unlike PreInvocation, whose
// injectSteps reply is optional and stays silent on any failure (nothing
// in Antigravity's docs requires PreInvocation to reply at all). Every
// other event prints nothing here: PostToolUse and Stop are the only two
// of the three wired events with a Required stdout reply.
func printAntigravityPostToolUseReply(event string, out io.Writer) {
	if event == "PostToolUse" {
		fmt.Fprintln(out, "{}")
	}
}

// RunFromCopilot is punk hook's GitHub Copilot CLI-specific entry point,
// called directly by cmdHook rather than through RunFrom's generic
// dispatch. Unlike Cursor (which goes through RunFrom/Normalize directly),
// Copilot needs its own entry point for exactly one reason: SessionStart
// context injection uses a DIFFERENT stdout envelope than Claude Code's.
// The reference (docs.github.com/en/copilot/reference/hooks-reference) has
// no dedicated output-schema block for sessionStart. The flat
// {"additionalContext": ...} shape this function prints is an inference
// from the reference's postToolUse and notification output examples, both
// documented as flat objects with an optional additionalContext string -
// not Claude Code's nested {"hookSpecificOutput": {"hookEventName":...,
// "additionalContext":...}} envelope Run (above) prints, and not
// Antigravity's {"injectSteps":[...]} shape either. The CLI tutorial
// (docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/
// use-hooks) is silent on sessionStart output processing. Printing the
// field is fail-safe either way: if Copilot ignores it, nothing happens.
// RunFrom
// itself never calls fetchContext for any non-Claude "from" (see its own
// doc comment) specifically because Claude Code's nested envelope would be
// garbage on another agent's stdout - that reasoning does not apply here
// since this function prints Copilot's own native shape instead, the same
// way RunFromAntigravity earned its own entry point to print Antigravity's
// native injectSteps shape. Keeping this self-contained leaves RunFrom,
// Normalize, and every existing --from cursor/claude/""/antigravity
// behavior byte-for-byte untouched, per this package's established
// per-agent-entry-point discipline (see RunFromAntigravity's own doc
// comment for the same rule stated at length).
//
// translateCopilot (normalize.go) IS registered in the shared translators
// map and self-identifies its event from hook_event_name inside raw
// (unlike Antigravity's event-blind payloads), so - unlike
// RunFromAntigravity - this function calls Normalize("copilot", raw)
// directly rather than needing a caller-supplied --event flag of its own.
//
// None of Copilot's wired events (SessionStart, UserPromptSubmit,
// PostToolUse, Stop, SessionEnd - see translateCopilot's own doc comment)
// REQUIRE a stdout reply, though the docs' Hook events table documents
// them differently: UserPromptSubmit and SessionEnd are both "Output
// processed: No"; postToolUse's own output section says "Return {} or
// empty output to keep the original ... result"; sessionStart's
// additionalContext is explicitly "Optional"; and agentStop (this
// package's "Stop") is listed "Yes - can block and force continuation",
// i.e. it CAN act on a reply, but only blocks when an explicit decision
// is returned - empty output is not itself a block. So unlike
// RunFromAntigravity's printAntigravityStopReply/
// printAntigravityPostToolUseReply (which print a REQUIRED reply on every
// return path, success or failure), this function only ever prints
// anything for the one optional case - SessionStart with a non-empty
// fetched context - and every other path, including every failure path,
// stays completely silent, matching Copilot's own "no output = default
// behavior" fallback.
//
// RunFromCopilot always returns nil: a dead memory server, a malformed
// payload, or any network failure is noted on errw and otherwise
// swallowed, fail-open, for the same reason every other entry point in
// this package is - punk only observes hook traffic and must never affect
// or block a user's Copilot CLI session.
func RunFromCopilot(stdin io.Reader, baseURL, apiKey string, out, errw io.Writer) error {
	baseURL = strings.TrimRight(baseURL, "/")
	raw, err := io.ReadAll(io.LimitReader(stdin, maxStdinBytes))
	if err != nil {
		fmt.Fprintln(errw, "punk hook: read stdin:", err)
		return nil
	}

	translated, ok, err := Normalize("copilot", raw)
	if err != nil {
		fmt.Fprintln(errw, "punk hook: normalize:", err)
		return nil
	}
	if !ok {
		// Unmapped event: forward nothing, exit clean - normal, expected
		// input, not a failure (see Normalize's own doc comment).
		return nil
	}
	forwardHook(baseURL, apiKey, translated, errw)

	var env hookPayload
	if err := json.Unmarshal(translated, &env); err != nil {
		// Unreachable in practice: translateCopilot just produced these
		// exact bytes via its own json.Marshal a few lines above. Guarded
		// anyway rather than assumed, matching this package's existing
		// discipline of never trusting a decode to succeed silently (see
		// fetchContext's own decode error path).
		fmt.Fprintln(errw, "punk hook: decode translated copilot envelope:", err)
		return nil
	}
	if env.HookEventName != "SessionStart" {
		return nil
	}

	ctxBody, fetched := fetchContext(baseURL, apiKey, sessionParams(env.CWD, env.SessionID), errw)
	if !fetched || ctxBody == "" {
		return nil
	}
	outJSON, err := json.Marshal(map[string]any{"additionalContext": ctxBody})
	if err != nil {
		fmt.Fprintln(errw, "punk hook: encode copilot injection payload:", err)
		return nil
	}
	fmt.Fprintln(out, string(outJSON))
	return nil
}

// hermesInjectionProbe recovers the two fields RunFromHermes' injection
// gate needs that the TRANSLATED envelope does not carry: the ORIGINAL
// (pre-translation) event name, and pre_llm_call's own is_first_turn kwarg.
// translateHermes rewrites hook_event_name to "UserPromptSubmit" and drops
// everything in "extra" it has no envelope field for, so both have to be
// read back off the raw native payload rather than off the translation.
// Every field is decoded permissively (a plain string / a bool inside one
// nested object), so a payload whose OTHER fields don't match
// hermesPayload's stricter shape still answers the injection question
// correctly.
type hermesInjectionProbe struct {
	HookEventName string `json:"hook_event_name"`
	Extra         struct {
		IsFirstTurn bool   `json:"is_first_turn"`
		UserMessage string `json:"user_message"`
	} `json:"extra"`
}

// RunFromHermes is punk hook's Hermes Agent-specific entry point, called
// directly by cmdHook rather than through RunFrom's generic dispatch, for
// the same single reason RunFromCopilot has its own: context injection uses
// a stdout envelope no other agent shares. hermes-agent.nousresearch.com/
// docs/user-guide/features/hooks documents pre_llm_call's shell-hook reply
// as {"context": "..."} - a flat object with one "context" string that
// Hermes appends to the user message - which is neither Claude Code's
// nested {"hookSpecificOutput":{...}} envelope (Run, above), nor
// Antigravity's {"injectSteps":[...]}, nor Copilot's flat
// {"additionalContext":...}. RunFrom itself never calls fetchContext for
// any non-Claude "from" (see its own doc comment) precisely because Claude
// Code's envelope would be garbage on another agent's stdout; that
// reasoning does not apply here since this function prints Hermes' own
// native shape instead. Keeping it self-contained leaves RunFrom,
// Normalize, and every existing --from behavior byte-for-byte untouched.
//
// translateHermes IS registered in the shared translators map and
// self-identifies its event from hook_event_name inside raw (unlike
// Antigravity's event-blind payloads), so - like RunFromCopilot and unlike
// RunFromAntigravity - this function calls Normalize("hermes", raw)
// directly and needs no caller-supplied --event flag.
//
// Injection fires on pre_llm_call ONLY when the payload's own
// extra.is_first_turn is true. pre_llm_call runs before EVERY model call in
// a session, so injecting unconditionally would re-send the whole recalled
// context on every turn - burning the model's context budget on a repeat of
// what it was already told. is_first_turn is Hermes' own per-call kwarg, so
// it makes a stateless once-per-session gate possible in a process that has
// nothing to remember between invocations (punk hook is a fresh subprocess
// per hook call, unlike the OpenCode/pi/OpenClaw plugin hosts that can hold
// an in-process Set) - the same technique RunFromAntigravity uses with
// invocationNum==0. Capture is NOT gated: every pre_llm_call still forwards
// its UserPromptSubmit envelope, so no user prompt is ever lost.
//
// None of the four wired Hermes events REQUIRE a stdout reply: pre_llm_call
// treats an empty or missing reply as "no context to add", and
// on_session_start/post_tool_call/post_llm_call are documented as
// observation hooks whose return value is ignored. So, unlike
// RunFromAntigravity (whose Stop/PostToolUse replies are required and are
// therefore printed on every return path including failures), this function
// prints only for the one optional case - a first-turn pre_llm_call with a
// non-empty fetched context - and stays completely silent on every other
// path, including every failure path.
//
// RunFromHermes always returns nil: a dead memory server, a malformed
// payload, or any network failure is noted on errw and otherwise swallowed,
// fail-open, for the same reason every other entry point in this package is.
func RunFromHermes(stdin io.Reader, baseURL, apiKey string, out, errw io.Writer) error {
	baseURL = strings.TrimRight(baseURL, "/")
	raw, err := io.ReadAll(io.LimitReader(stdin, maxStdinBytes))
	if err != nil {
		fmt.Fprintln(errw, "punk hook: read stdin:", err)
		return nil
	}

	translated, ok, err := Normalize("hermes", raw)
	if err != nil {
		fmt.Fprintln(errw, "punk hook: normalize:", err)
		return nil
	}
	if !ok {
		// Unmapped event: forward nothing, exit clean - normal, expected
		// input, not a failure (see Normalize's own doc comment).
		return nil
	}
	forwardHook(baseURL, apiKey, translated, errw)

	var probe hermesInjectionProbe
	if err := json.Unmarshal(raw, &probe); err != nil {
		// Unreachable in practice: Normalize just decoded these same bytes
		// with a stricter struct. Guarded anyway rather than assumed,
		// matching this package's discipline of never trusting a decode to
		// succeed silently (see fetchContext's own decode error path).
		fmt.Fprintln(errw, "punk hook: decode hermes injection probe:", err)
		return nil
	}
	if probe.HookEventName != "pre_llm_call" {
		return nil
	}

	var env hookPayload
	if err := json.Unmarshal(translated, &env); err != nil {
		fmt.Fprintln(errw, "punk hook: decode translated hermes envelope:", err)
		return nil
	}
	// First turn gets the session-start block (the once-per-session gate
	// documented above). Every later turn gets the per-turn, prompt-scoped
	// block instead - empty unless the server's
	// memory.turn_context_tokens is set and the prompt matches something
	// not already injected, so quiet turns stay reply-free exactly like
	// before this path existed.
	var params url.Values
	if probe.Extra.IsFirstTurn {
		params = sessionParams(env.CWD, env.SessionID)
	} else {
		if probe.Extra.UserMessage == "" {
			return nil
		}
		params = turnParams(env.CWD, env.SessionID, probe.Extra.UserMessage)
	}
	ctxBody, fetched := fetchContext(baseURL, apiKey, params, errw)
	if !fetched || ctxBody == "" {
		return nil
	}
	outJSON, err := json.Marshal(map[string]any{"context": ctxBody})
	if err != nil {
		fmt.Fprintln(errw, "punk hook: encode hermes injection payload:", err)
		return nil
	}
	fmt.Fprintln(out, string(outJSON))
	return nil
}

// setAuth sets the Authorization header on req when apiKey is non-empty.
// An empty apiKey leaves the header entirely absent - never "Bearer " with
// no token, which some servers treat differently from no header at all.
func setAuth(req *http.Request, apiKey string) {
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}
