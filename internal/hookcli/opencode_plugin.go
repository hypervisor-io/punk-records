package hookcli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// openCodePluginMarker is the required first line of every plugin file
// ConnectOpenCode writes, identifying it as punk-managed the same way
// cursorRulesMarker (connect_cursor.go) gates WriteCursorRules: an existing
// plugin file at the target path whose first line is NOT this marker is
// presumed hand-authored (a real user plugin, or one installed by a
// different tool) and ConnectOpenCode refuses to overwrite it.
const openCodePluginMarker = "// managed by punk connect opencode"

// jsStringLiteral renders s as a double-quoted JavaScript string literal
// safe to splice into generated JS source. json.Marshal of a string
// produces exactly the escaping a JS string literal needs (quotes,
// backslashes, control characters, unicode), with one JSON/JS divergence:
// U+2028 and U+2029 (LINE/PARAGRAPH SEPARATOR) are legal unescaped inside a
// JSON string but historically were not legal unescaped inside a JS string
// literal (fixed in the language spec by ES2019, but plenty of tooling -
// older bundlers, some linters - still chokes on a raw one), so both are
// re-escaped by hand after marshaling. serverURL is operator-supplied (the
// `--url` flag or $PUNK_URL), not attacker data, but per house rule
// (.claude/rules/ai.md: "ALWAYS unconditionally escape strings embedded in
// structured formats") it is escaped unconditionally rather than trusted
// to be quote/backslash/newline-free.
func jsStringLiteral(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// json.Marshal of a string only fails for invalid UTF-8; s reaches
		// here as a Go string built from a CLI flag/env var, so this is
		// unreachable in practice. Fall back to an empty string literal
		// rather than letting a marshal error propagate as a panic-shaped
		// surprise into generated JS source.
		return `""`
	}
	out := string(b)
	out = strings.ReplaceAll(out, " ", `\u2028`)
	out = strings.ReplaceAll(out, " ", `\u2029`)
	return out
}

// openCodePluginTemplate is the full JavaScript source ConnectOpenCode
// writes, with exactly one substitution point: the PUNK_URL fallback
// default (see punkServerURL below), rendered via jsStringLiteral so a
// serverURL containing a quote or backslash can never break out of the
// string literal it's spliced into.
//
// Design notes, since this is a JS file most readers will only ever see as
// generated output:
//
//   - Plugin directories (.opencode/plugins/, ~/.config/opencode/plugins/),
//     the module export shape (a function receiving a context object,
//     returning the Hooks object directly - NOT wrapped in a {hooks: ...}
//     key), and the existence of the "event" and "tool.execute.after"
//     hooks are documented at https://opencode.ai/docs/plugins/ ("Use a
//     plugin", "Basic structure", "Events").
//   - The exact payload shapes (not spelled out on that prose page) come
//     from the published npm packages @opencode-ai/sdk and
//     @opencode-ai/plugin, both v1.18.11 (the "latest" tag at the time of
//     writing): sdk's dist/gen/types.gen.d.ts defines
//     EventSessionCreated as {type:"session.created", properties:{info:
//     Session}} (Session.id is the session id) and EventSessionIdle as
//     {type:"session.idle", properties:{sessionID: string}}; plugin's
//     dist/index.d.ts defines "tool.execute.after" as
//     (input:{tool,sessionID,callID,args}, output:{title,output,
//     metadata}).
//   - Session-start context injection: the plugins docs page documents no
//     hook for adding text before an agent's first turn (no "session
//     start" or "chat.message" context hook exists there). The only
//     currently-shipped hook that can add arbitrary text to what becomes
//     the model's system prompt is "experimental.chat.system.transform"
//     (input:{sessionID?, model}, output:{system: string[]}) -
//     @opencode-ai/plugin@1.18.11's dist/index.d.ts, marked experimental
//     and absent from the prose docs page but present in the published
//     package's shipped type surface. This plugin uses it, gated by a
//     per-session Set so the context fetch/injection happens once per
//     session rather than on every LLM turn. If OpenCode ships a
//     dedicated session-start hook later, that should replace this one.
//   - User-prompt capture: "chat.message" (input:{sessionID, agent?,
//     model?, messageID?, variant?}, output:{message: UserMessage,
//     parts: Part[]}) - @opencode-ai/plugin@1.18.11's dist/index.d.ts,
//     fetched directly from unpkg (not guessed). Unlike
//     experimental.chat.system.transform, this hook is NOT marked
//     experimental. UserMessage's "role" field is typed as the literal
//     "user" (@opencode-ai/sdk@1.18.11's dist/gen/types.gen.d.ts), so
//     output.message should always be a user message here, but the
//     handler still checks message.role === "user" defensively rather
//     than trusting the type at runtime. Part's text variant is
//     {type:"text", text: string, synthetic?: boolean, ignored?: boolean,
//     ...}; prompt text is every text part's "text" field joined with
//     "\n", excluding any part with synthetic or ignored true. messageID
//     becomes prompt_id when present; when absent, a deterministic
//     fnv1a-32 hash of sessionID+prompt stands in - the same algorithm as
//     hookcli's own promptIDFallback/fnv32aHex, but an independent id
//     space (JS hashes UTF-16 code units, Go hashes UTF-8 bytes; the two
//     are never compared), used for the same reason: the server drops
//     any UserPromptSubmit whose prompt_id sanitizes to empty.
//   - Every network call goes through punkFetch: a 2-second
//     AbortController timeout that covers the ENTIRE request including
//     reading the response body (fetch() resolves as soon as headers
//     arrive; without also reading the body inside the same abort-armed
//     window, a server that sends headers then stalls the body would
//     hang the caller forever - the abort timer is only cleared in
//     punkFetch's "finally", after the body has been read or the abort
//     has fired). Every failure - timeout, network error, non-OK status,
//     malformed JSON - is swallowed (console.error at most) and
//     punkFetch resolves to null rather than rejecting, so it is always
//     safe to call without a surrounding try/catch and (see the
//     blocking-vs-observational note on the Hooks object below) safe to
//     not await. A non-OK response's body is explicitly drained
//     (response.body.cancel()) since it's never read via .json() in that
//     branch - leaving it unread would leak the underlying socket rather
//     than returning it to any connection pool. Every exported hook body
//     is additionally wrapped in its own try/catch, so a bug in this
//     plugin can never throw into OpenCode and interrupt a user's session
//     (fail-open, mirroring hookcli.RunFrom's hook-forwarder contract -
//     see .claude/rules/api.md).
const openCodePluginTemplate = openCodePluginMarker + `
//
// Punk-records memory bridge for OpenCode (https://opencode.ai). Forwards
// session/tool hook events to a punk-records server as Claude-shaped hook
// envelopes (POST /v1/agent/hooks) and injects that project's stored
// memory into the model's system prompt on the first LLM turn of each
// session (GET /v1/agent/context).
//
// Sources (accurate as of writing - re-check if OpenCode's plugin API
// changes):
//   - Plugin directories, module shape, and the event/tool.execute.after
//     hooks: https://opencode.ai/docs/plugins/ ("Use a plugin",
//     "Basic structure", "Events").
//   - Exact event/hook payload shapes (not spelled out on the docs page):
//     the published @opencode-ai/sdk and @opencode-ai/plugin npm packages,
//     v1.18.11 - sdk's dist/gen/types.gen.d.ts (EventSessionCreated,
//     EventSessionIdle) and plugin's dist/index.d.ts (Hooks interface).
//   - Session-start context injection: the plugins docs page documents no
//     hook for adding text before an agent's first turn. The only
//     currently-shipped hook that can add arbitrary text to what becomes
//     the model's system prompt is "experimental.chat.system.transform"
//     (@opencode-ai/plugin@1.18.11 dist/index.d.ts) - marked experimental
//     and absent from the prose docs page, but present in the published
//     package's type surface. If OpenCode ships a dedicated session-start
//     context hook later, prefer that instead of this one.
//   - User-prompt capture: "chat.message" (@opencode-ai/plugin@1.18.11
//     dist/index.d.ts) - NOT marked experimental. output.message is typed
//     as UserMessage, whose "role" is the literal "user"
//     (@opencode-ai/sdk@1.18.11 dist/gen/types.gen.d.ts).
//
// Hook classification - which hooks await their network call and which
// don't (see punkFetch and the per-hook comments below for the full
// rationale):
//   - BLOCKING: experimental.chat.system.transform. The model's system
//     prompt is genuinely incomplete until this either succeeds or gives
//     up, bounded by punkFetch's 2-second timeout.
//   - OBSERVATIONAL (fire-and-forget, never awaited): event,
//     tool.execute.after, chat.message. Nothing in the running session is
//     waiting on these; awaiting them would stall a tool call or session
//     event by up to 2 seconds whenever the punk-records server is slow
//     or unreachable.
//
// This plugin runs inside the OpenCode process (Bun, or Node per the
// docs' TypeScript-support note) with no external dependencies. Every
// network call - including reading the response body, not just waiting
// for headers - is bounded by a 2-second timeout, and every failure is
// swallowed (console.error at most) - a dead or unreachable punk-records
// server must never break an OpenCode session.

export const PunkMemoryPlugin = async ({ directory }) => {
  const injectedSessions = new Set()

  function punkServerURL() {
    const fromEnv = typeof process !== "undefined" && process.env && process.env.PUNK_URL
    return (fromEnv || %s).replace(/\/+$/, "")
  }

  function punkAPIKey() {
    return (typeof process !== "undefined" && process.env && process.env.PUNK_API_KEY) || ""
  }

  // punkFetch performs one request against the punk-records server with a
  // fixed 2-second AbortController timeout that covers the ENTIRE
  // request, including reading and parsing the response body - not just
  // waiting for headers to arrive. fetch() itself resolves as soon as
  // headers are in; reading the body outside this function (after
  // clearTimeout has already fired in "finally") would let a server that
  // sends headers then stalls the body hang the caller forever. Every
  // failure - abort, network error, non-OK status, malformed JSON - is
  // swallowed (console.error at most) and resolves to null rather than
  // rejecting, so every call site can invoke it bare: no surrounding
  // try/catch and, for the observational hooks below, no await needed.
  async function punkFetch(path, init) {
    const controller = new AbortController()
    const timer = setTimeout(() => controller.abort(), 2000)
    try {
      const headers = Object.assign({ "Content-Type": "application/json" }, init && init.headers)
      const key = punkAPIKey()
      if (key) headers["Authorization"] = "Bearer " + key
      const res = await fetch(punkServerURL() + path, Object.assign({}, init, { headers, signal: controller.signal }))
      if (!res.ok) {
        // Drain rather than ignore: an unread body on a non-OK response
        // leaks the underlying socket instead of freeing it for reuse.
        if (res.body && typeof res.body.cancel === "function") {
          res.body.cancel().catch(() => {})
        }
        return null
      }
      return await res.json()
    } catch (err) {
      console.error("punk connect opencode: request to " + path + " failed:", err && err.message ? err.message : err)
      return null
    } finally {
      clearTimeout(timer)
    }
  }

  function postHook(body) {
    return punkFetch("/v1/agent/hooks", { method: "POST", body: JSON.stringify(body) })
  }

  // fnv1aHex is a 32-bit FNV-1a hash, hex-encoded - the same algorithm as
  // hookcli's own fnv32aHex (internal/hookcli/normalize.go), but an
  // independent id space, not a cross-language twin: this hashes UTF-16
  // code units (JS string iteration) while fnv32aHex hashes UTF-8 bytes,
  // so the same input string does not hash identically on both sides.
  // They are never compared to each other - each only needs to be
  // internally deterministic - so this is used the same way: deriving a
  // deterministic fallback id when the real event carries none, so a
  // capture is never dropped for lack of a stable key. The fallback id
  // below joins sessionID and prompt with a NUL separator, matching
  // promptIDFallback's (normalize.go) own conversationID+prompt join, so
  // "ab"+"c" and "a"+"bc" never hash identically here either - the two
  // hashes are still never compared to each other, only each side's own
  // internal determinism matters. The NUL is written as an escape inside
  // the JS string literal below (a backslash followed by the four digits
  // 0000), never as a raw NUL byte in this Go source file: embedding an
  // actual 0x00 byte here would corrupt this source file, and some
  // tooling that processes generated JS chokes on stray raw control
  // bytes, so the escape form is load-bearing, not stylistic.
  function fnv1aHex(s) {
    let h = 0x811c9dc5
    for (let i = 0; i < s.length; i++) {
      h ^= s.charCodeAt(i)
      h = Math.imul(h, 0x01000193)
    }
    return (h >>> 0).toString(16).padStart(8, "0")
  }

  return {
    // OBSERVATIONAL: nothing in the running session is waiting on this
    // capture, so postHook(...) is deliberately NOT awaited
    // (fire-and-forget). punkFetch never rejects (see above), so there is
    // no unhandled-rejection risk from not awaiting it - awaiting here
    // would otherwise stall every session-start/idle event by up to
    // punkFetch's 2-second timeout whenever the server is slow or down.
    event: async ({ event }) => {
      try {
        if (event.type === "session.created") {
          postHook({
            hook_event_name: "SessionStart",
            session_id: event.properties.info.id,
            cwd: directory || "",
            source: "opencode",
          })
        } else if (event.type === "session.idle") {
          // EventSessionIdle only carries { sessionID } - there is no
          // assistant-message text on this event, but "idle" is itself
          // meaningful terminal-state content, so it is sent as
          // last_assistant_message rather than left out entirely: an
          // empty Stop body would otherwise store a fact with no
          // information at all (the same lesson Cursor's stop/sessionEnd
          // translation already applies - see
          // hookcli/normalize.go's translateCursor).
          postHook({
            hook_event_name: "Stop",
            session_id: event.properties.sessionID,
            last_assistant_message: "status=idle",
            cwd: directory || "",
            source: "opencode",
          })
        }
      } catch (err) {
        console.error("punk connect opencode: event hook failed:", err && err.message ? err.message : err)
      }
    },

    // OBSERVATIONAL (see the "event" hook's comment above): not awaited.
    "tool.execute.after": async (input, output) => {
      try {
        postHook({
          hook_event_name: "PostToolUse",
          session_id: input.sessionID,
          tool_use_id: input.callID,
          tool_name: input.tool,
          tool_input: input.args,
          tool_response: output ? output.output : undefined,
          cwd: directory || "",
          source: "opencode",
        })
      } catch (err) {
        console.error("punk connect opencode: tool.execute.after hook failed:", err && err.message ? err.message : err)
      }
    },

    // OBSERVATIONAL (see the "event" hook's comment above): not awaited.
    // Fires "when a new message is received" (@opencode-ai/plugin@1.18.11
    // dist/index.d.ts). output.message is typed as UserMessage (role
    // literal "user" per @opencode-ai/sdk@1.18.11's
    // dist/gen/types.gen.d.ts), but the role is still checked at runtime
    // rather than trusted from the type alone. Prompt text is every
    // text-type part's "text" field, joined with "\n" (parts can also
    // carry files/tool references this plugin has no use for) - EXCLUDING
    // any part flagged synthetic or ignored (@opencode-ai/sdk@1.18.11's
    // TextPart: both optional booleans), since those are not real
    // user-authored content and must never be captured as if they were.
    // messageID becomes prompt_id when the runtime provides one; otherwise
    // a deterministic hash of sessionID+prompt stands in, mirroring
    // hookcli's promptIDFallback (normalize.go) - including its NUL
    // separator between the two joined fields (see fnv1aHex's own comment
    // above for why that separator is written as an escape sequence, not
    // a raw byte, in this source file) - the server drops any
    // UserPromptSubmit whose prompt_id sanitizes to empty, so a missing id
    // must never mean a silently dropped prompt.
    "chat.message": async (input, output) => {
      try {
        const message = output && output.message
        if (!message || message.role !== "user") {
          return
        }
        const parts = Array.isArray(output.parts) ? output.parts : []
        const prompt = parts
          .filter((p) => p && p.type === "text" && typeof p.text === "string" && !p.synthetic && !p.ignored)
          .map((p) => p.text)
          .join("\n")
        const sessionID = input && input.sessionID
        const promptID = (input && input.messageID) || "msg-" + fnv1aHex((sessionID || "") + "\u0000" + prompt)
        postHook({
          hook_event_name: "UserPromptSubmit",
          session_id: sessionID,
          prompt_id: promptID,
          prompt,
          cwd: directory || "",
          source: "opencode",
        })
      } catch (err) {
        console.error("punk connect opencode: chat.message hook failed:", err && err.message ? err.message : err)
      }
    },

    // BLOCKING: the only hook here that awaits its network call. The
    // model's system prompt for this turn is genuinely incomplete until
    // context injection either succeeds or gives up, so - unlike the
    // observational hooks above - stalling here (bounded by punkFetch's
    // 2-second timeout) is the intended tradeoff, not an oversight.
    "experimental.chat.system.transform": async (input, output) => {
      try {
        const sessionID = input && input.sessionID
        if (!sessionID || injectedSessions.has(sessionID)) {
          return
        }
        // Marked injected BEFORE the fetch, not after a successful
        // response - deliberate tradeoff: if this request fails (timeout,
        // network error), injection is disabled for the REST of this
        // session rather than retried on the next turn, so one transient
        // failure never causes the 2-second stall to repeat on every
        // subsequent turn. injectedSessions is never pruned, so it grows
        // for the life of the OpenCode process (one entry per session) -
        // accepted, since a session count large enough for that to matter
        // is not a realistic usage pattern for a local developer tool.
        injectedSessions.add(sessionID)
        const data = await punkFetch("/v1/agent/context?cwd=" + encodeURIComponent(directory || ""))
        if (data && typeof data.context === "string" && data.context.length > 0) {
          output.system.push(data.context)
        }
      } catch (err) {
        console.error("punk connect opencode: context injection failed:", err && err.message ? err.message : err)
      }
    },
  }
}
`

// openCodePluginContent renders the full plugin source with serverURL
// baked in as the PUNK_URL fallback default - still overridable at
// runtime via the PUNK_URL environment variable, see the rendered
// plugin's own header comment and punkServerURL().
func openCodePluginContent(serverURL string) string {
	return fmt.Sprintf(openCodePluginTemplate, jsStringLiteral(serverURL))
}
