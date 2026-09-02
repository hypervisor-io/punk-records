package hookcli

import "fmt"

// piExtensionMarker is the required first line of every extension file
// ConnectPi writes, identifying it as punk-managed the same way
// openCodePluginMarker (opencode_plugin.go) and cursorRulesMarker
// (connect_cursor.go) gate their own writers: an existing extension file
// at the target path whose first line is NOT this marker is presumed
// hand-authored and ConnectPi refuses to overwrite it.
const piExtensionMarker = "// managed by punk connect pi"

// piExtensionTemplate is the full source ConnectPi writes for the pi
// coding agent (github.com/earendil-works/pi, package
// @mariozechner/pi-coding-agent, pi.dev), with exactly one substitution
// point: the PUNK_URL fallback default (see punkServerURL below), rendered
// via jsStringLiteral (opencode_plugin.go) so a serverURL containing a
// quote or backslash can never break out of the string literal it's
// spliced into.
//
// Sources (fetched directly, not guessed - re-check if pi's extension API
// changes): https://raw.githubusercontent.com/earendil-works/pi/main/packages/coding-agent/docs/extensions.md
// (also on GitHub at github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/extensions.md).
//
//   - Extension locations: project-local ".pi/extensions/*.ts" (or
//     "*/index.ts"), global "~/.pi/agent/extensions/*.ts" - "Put
//     extensions in ~/.pi/agent/extensions/ (global) or .pi/extensions/
//     (project-local) for auto-discovery." A plain "punk-memory.ts"
//     directly in either directory matches the "*.ts" auto-discovery
//     pattern, so no index.ts subdirectory is needed.
//   - Module shape: an extension exports a default factory function that
//     receives ExtensionAPI - a default export shaped like
//     "export default function (pi) {...}" - and its factory "can be
//     synchronous or asynchronous" (docs' own wording). This template's
//     factory is synchronous: every network call happens inside an event
//     handler, not during registration, so there's nothing to await at
//     startup. Extensions are loaded via jiti, so TypeScript works
//     without compilation - this template deliberately uses NO
//     TypeScript-only syntax (no type annotations, no imports at all, not
//     even "import type"), so despite the .ts extension it is also valid
//     plain JavaScript/ESM. That keeps it fully self-contained: no
//     package.json, no "npm install" in the extension directory, and no
//     dependency on the @earendil-works/pi-coding-agent package actually
//     being resolvable from wherever punk writes this file - unlike
//     OpenCode's plugin (also self-contained, but for a runtime already
//     confirmed to ship global fetch either way), pi's docs never state
//     "the extension host is Node.js" in those words; that's an inference
//     from every code example in the docs calling bare fetch() with no
//     import, plus the docs' own line that "Node.js built-ins (node:fs,
//     node:path, etc.) are also available" - strong enough evidence that
//     this template reads process.env directly with no "typeof process
//     !== undefined" hedge the way opencode_plugin.go's punkServerURL
//     needs for its Bun-or-Node ambiguity.
//   - pi.on(eventName, handler): docs describe this plainly as "Subscribe
//     to events" - registered directly on the pi API object passed into
//     the factory;
//     unlike OpenCode's plugin (which returns a Hooks object), pi
//     extensions call pi.on(...) as a side effect of running the factory.
//   - Session start: "session_start" - docs: "Fired when a session is
//     started, loaded, or reloaded." - fires with event.reason set to one
//     of startup, reload, new, resume, or fork (the docs' code comment
//     lists exactly those five values with no further prose explaining
//     what each one means; this template does not attempt to guess).
//     This template fires SessionStart for every reason rather than
//     filtering - a duplicate SessionStart at the same
//     /agent-sessions/<sid>/start key is a harmless overwrite (memory
//     capture is replay-deduped per key, per README's agent-memory
//     section), and staying inclusive is simpler and strictly safer than
//     guessing which reasons "really" mean a fresh session.
//   - Session id / cwd: not carried on most event objects themselves -
//     every handler below reads "ctx.sessionManager.getSessionId()" and
//     ctx.cwd off ctx (the second handler argument, per
//     ExtensionContext's documented shape), not off the event itself.
//   - User prompt capture: "input" - "event.text - raw input (before
//     skill/template expansion)", "event.source -
//     'interactive' (typed), 'rpc' (API), or 'extension'". Per docs'
//     "Processing order": "Extension commands (/cmd) checked first - if
//     found, handler runs and input event is skipped" - only registered
//     extension commands skip "input"; a "/skill:name" or "/template"
//     slash command still fires "input" first (with its raw, unexpanded
//     text) and only expands afterward if the handler doesn't intercept
//     it, so this template must not assume slash-command text never
//     reaches this handler. "extension"-sourced input is a synthetic
//     pi.sendUserMessage(...) call from another extension, not real user
//     text - excluded here the same way OpenCode's chat.message
//     translation excludes synthetic/ignored parts. Unlike OpenCode's
//     chat.message (whose UserMessage sometimes carries a real messageID),
//     pi's documented "input" event shape carries NO message/turn
//     identifier at all, so the fnv1a fallback below is the only path in
//     practice today; the "real id" branch is kept (checking event.id)
//     only so a future pi release that adds one is picked up automatically
//     without a template change, mirroring the fallback-shape convention
//     (fnv1a hash, NUL-separator join) already used by
//     opencode_plugin.go's chat.message handler and hookcli's own
//     promptIDFallback (normalize.go).
//   - Tool result capture: "tool_result" fires with event.toolName,
//     event.toolCallId, event.input, event.content, event.details,
//     event.isError, and event.usage (the docs' code comment lists just
//     the field names, with no further description of each one). Docs
//     note the handler's return value "can modify" the result before the
//     LLM sees it; this template
//     returns undefined (implicitly, by never returning) so tool output is
//     never altered - it only observes.
//   - Session end: pi has no single event named like Claude Code's "Stop"
//     or OpenCode's "session.idle". "agent_settled" - docs: "Use
//     agent_settled for status integrations that need to know Pi will not
//     continue running automatically" (agent_start/agent_end may still
//     fire again first via auto-retry, auto-compaction, or queued
//     follow-ups) - is the closest analog, but its documented handler
//     example takes an unused "_event" parameter with no field reads
//     shown, so this template treats it as carrying no usable message
//     text of its own. "turn_end" fires with event.turnIndex,
//     event.message, and event.toolResults (again just a bare field list
//     in the docs' code comment) - event.message DOES carry the
//     assistant's response, so this template listens to turn_end purely
//     to cache the latest assistant text in a closure variable, then uses
//     that cache to give agent_settled's Stop capture non-empty content;
//     turn_end itself is never forwarded to the server (it has no Claude
//     Code hook equivalent of its own). Message shape (inferred from the
//     SDK's own code examples elsewhere in the docs, not stated as prose
//     for turn_end specifically): event.message has a role and a content
//     field that is an array of parts, not a plain string -
//     extractAssistantText below joins every {type:"text", text: string}
//     part's text with "\n", mirroring OpenCode's own text-part-joining
//     logic in its chat.message handler.
//   - Context injection: pi has no dedicated "session start" context hook
//     either. "before_agent_start" - "Fired after user submits prompt,
//     before agent loop" - is the closest analog to OpenCode's
//     "experimental.chat.system.transform", and like that hook, its return
//     value can rewrite the system prompt: "return { systemPrompt:
//     event.systemPrompt + '\n\nExtra instructions...' }" (both
//     "message" and "systemPrompt" on the return value are optional).
//     Critically, before_agent_start fires once per submitted prompt (per
//     the "after user submits prompt, before agent loop" language quoted
//     above), not once for the whole session - so, exactly like
//     OpenCode's own per-session injectedSessions Set, injection here is
//     gated to fire at most once per session (on that session's first
//     submitted prompt) rather than re-fetching context on every
//     subsequent prompt.
//
// Hook classification - which handlers await their network call and which
// don't (mirrors opencode_plugin.go's own "Hook classification" doc
// comment and .claude/rules/api.md's "classify each wired event blocking
// vs observational BEFORE wiring" rule):
//   - BLOCKING: before_agent_start. The system prompt pi is about to send
//     to the model is genuinely incomplete until this either succeeds or
//     gives up, bounded by punkFetch's 2-second timeout.
//   - OBSERVATIONAL (fire-and-forget, never awaited): session_start,
//     input, tool_result, agent_settled. Nothing in the running session is
//     waiting on these; awaiting them would stall a tool call or session
//     event by up to 2 seconds whenever the punk-records server is slow or
//     unreachable.
//   - Neither (no network call at all): turn_end only caches state in a
//     closure variable; see its own comment above.
//
// This extension runs inside pi's own Node.js process (see the Node
// inference above) with no external dependencies. Every network call -
// including reading the response body, not just waiting for headers - is
// bounded by a 2-second timeout, and every failure is swallowed
// (console.error at most) - a dead or unreachable punk-records server must
// never break a pi session. pi's docs DO have an "Error Handling" section -
// "Extension errors are logged, agent continues" - so a throwing handler
// would not crash the session even without the try/catch below; the
// try/catch is kept anyway for two things that section doesn't promise:
// (1) it's the mechanism that makes console.error calls consistent instead
// of relying on however pi's own top-level logging formats an uncaught
// error, and (2) no async-handler timeout contract is documented, so an
// event handler that never resolves has no stated host-side backstop -
// every exported handler below is still wrapped in its own try/catch on
// the same fail-open discipline
// opencode_plugin.go and hookcli.RunFrom already apply, rather than
// relying on undocumented host behavior.
const piExtensionTemplate = piExtensionMarker + `
//
// Punk-records memory bridge for pi (https://pi.dev,
// github.com/earendil-works/pi). Forwards session/tool hook events to a
// punk-records server as Claude-shaped hook envelopes (POST
// /v1/agent/hooks) and injects that project's stored memory into the
// model's system prompt on the first turn of each session (GET
// /v1/agent/context).
//
// Sources (accurate as of writing - re-check if pi's extension API
// changes): https://raw.githubusercontent.com/earendil-works/pi/main/packages/coding-agent/docs/extensions.md
//   - Extension locations, module shape (a default factory function
//     receiving the pi API object, loaded via jiti with no compilation
//     step needed), and pi.on(eventName, handler) registration.
//   - Event shapes: session_start (event.reason), input (event.text,
//     event.source), tool_result (event.toolName, event.toolCallId,
//     event.input, event.content), turn_end (event.message, an assistant
//     message whose content is an array of {type:"text", text} parts),
//     agent_settled (notification-only, no documented fields of its own),
//     before_agent_start (fires once per submitted prompt, after the
//     prompt is submitted and before the agent loop starts; return value
//     can rewrite the system prompt via {systemPrompt: ...}).
//   - Session id / cwd come from ctx.sessionManager.getSessionId() /
//     ctx.cwd, not from the event objects themselves. getSessionId() is
//     only shown in one docs code example (a before_provider_headers
//     snippet), not in the sessionManager method list itself - used here
//     anyway since it's the only session-id accessor the docs show
//     anywhere.
//   - Extensions run in Node.js: every code example in the docs calls bare
//     fetch() with no import, and the docs separately note Node.js
//     built-ins (node:fs, node:path, etc.) are also available - inferred
//     from that, not an explicit "runs in Node.js" statement, but not
//     assumed the way OpenCode's Bun-or-Node runtime is either.
//
// Hook classification - which handlers await their network call and which
// don't:
//   - BLOCKING: before_agent_start. The system prompt for this turn is
//     genuinely incomplete until this either succeeds or gives up, bounded
//     by punkFetch's 2-second timeout.
//   - OBSERVATIONAL (fire-and-forget, never awaited): session_start,
//     input, tool_result, agent_settled.
//   - turn_end makes no network call at all - it only caches the latest
//     assistant text so agent_settled's Stop capture has real content
//     instead of an empty body (agent_settled's own event object carries
//     none).
//
// This file has a .ts extension (pi's auto-discovery only looks for
// "*.ts"/"* /index.ts"), but deliberately contains no TypeScript-only
// syntax (no type annotations, no imports) so it is also valid plain
// JavaScript/ESM - self-contained, no package.json, no "npm install"
// needed in the extension directory. Every network call - including
// reading the response body, not just waiting for headers - is bounded by
// a 2-second timeout, and every failure is swallowed (console.error at
// most) - a dead or unreachable punk-records server must never break a pi
// session. pi's docs' Error Handling section says extension errors are
// logged and the agent continues, but every handler below is still
// wrapped in its own try/catch anyway: it keeps error logging consistent
// (console.error with this extension's own prefix) rather than depending
// on however pi's own top-level handling formats it, and it covers the
// case the docs don't promise anything about - a handler that never
// resolves, since no async-handler timeout contract is documented.

export default function (pi) {
  const injectedSessions = new Set()
  let lastAssistantText = ""
  let warnedEmptySessionID = false

  function punkServerURL() {
    const fromEnv = process.env && process.env.PUNK_URL
    return (fromEnv || %s).replace(/\/+$/, "")
  }

  function punkAPIKey() {
    return (process.env && process.env.PUNK_API_KEY) || ""
  }

  // punkFetch performs one request against the punk-records server with a
  // fixed 2-second AbortController timeout that covers the ENTIRE
  // request, including reading and parsing the response body - not just
  // waiting for headers to arrive (see opencode_plugin.go's own punkFetch
  // for the full "why", identical reasoning applies here: clearing the
  // abort timer right after fetch() resolves, before awaiting
  // res.json(), would leave the body read unbounded). Every failure -
  // abort, network error, non-OK status, malformed JSON - is swallowed
  // (console.error at most) and resolves to null rather than rejecting,
  // so every call site can invoke it bare: no surrounding try/catch and,
  // for the observational handlers below, no await needed.
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
      console.error("punk connect pi: request to " + path + " failed:", err && err.message ? err.message : err)
      return null
    } finally {
      clearTimeout(timer)
    }
  }

  function postHook(body) {
    return punkFetch("/v1/agent/hooks", { method: "POST", body: JSON.stringify(body) })
  }

  // fnv1aHex is a 32-bit FNV-1a hash, hex-encoded - the same algorithm as
  // hookcli's own fnv32aHex (internal/hookcli/normalize.go) and
  // opencode_plugin.go's own fnv1aHex, but an independent id space from
  // both: internally deterministic on its own, never compared across
  // languages or extensions. Used the same way both siblings use it:
  // deriving a deterministic fallback id when the real event carries none
  // (see this file's own doc comment on why pi's "input" event never
  // does today), so a capture is never dropped for lack of a stable key.
  // The NUL separator between sessionID and prompt is written as an
  // escape inside the JS string literal below (a backslash followed by
  // the four digits 0000), never as a raw NUL byte in this Go source
  // file, for the same reason opencode_plugin.go's fnv1aHex documents:
  // embedding an actual 0x00 byte here would corrupt this source file,
  // and some tooling that processes generated JS/TS chokes on stray raw
  // control bytes.
  function fnv1aHex(s) {
    let h = 0x811c9dc5
    for (let i = 0; i < s.length; i++) {
      h ^= s.charCodeAt(i)
      h = Math.imul(h, 0x01000193)
    }
    return (h >>> 0).toString(16).padStart(8, "0")
  }

  // sessionIdOf reads the session id off ctx.sessionManager.getSessionId()
  // - the only session-id accessor pi's docs show anywhere (in a
  // before_provider_headers example; it is not part of the sessionManager
  // method list itself, an honest gap since there's no other documented
  // way to get one). Fails open to "" when it's missing or throws, same
  // as every other capture path here, but logs once (not per-call, so a
  // whole session's worth of empty-id events doesn't spam stderr) so a
  // host that never populates it is at least visible in pi's own logs
  // instead of silently producing empty-session_id captures forever.
  function sessionIdOf(ctx) {
    const id = (ctx && ctx.sessionManager && typeof ctx.sessionManager.getSessionId === "function" && ctx.sessionManager.getSessionId()) || ""
    if (!id && !warnedEmptySessionID) {
      warnedEmptySessionID = true
      console.error("punk connect pi: ctx.sessionManager.getSessionId() resolved empty; captures will use an empty session_id until a real one is available (this warning is logged once per extension instance)")
    }
    return id
  }

  function cwdOf(ctx) {
    return (ctx && ctx.cwd) || ""
  }

  // extractAssistantText pulls the plain-text content out of an assistant
  // message for turn_end's caching below: pi's documented message shape
  // has "role" and a "content" array of parts, not a plain string body -
  // every {type:"text", text: string} part's text is joined with "\n",
  // mirroring OpenCode's own text-part-joining logic in its chat.message
  // handler (opencode_plugin.go).
  function extractAssistantText(message) {
    if (!message || message.role !== "assistant") {
      return ""
    }
    const content = message.content
    if (typeof content === "string") {
      return content
    }
    if (Array.isArray(content)) {
      return content
        .filter((p) => p && p.type === "text" && typeof p.text === "string")
        .map((p) => p.text)
        .join("\n")
    }
    return ""
  }

  // OBSERVATIONAL: nothing in the running session is waiting on this
  // capture, so postHook(...) is deliberately NOT awaited
  // (fire-and-forget). punkFetch never rejects (see above), so there is
  // no unhandled-rejection risk from not awaiting it.
  pi.on("session_start", async (event, ctx) => {
    try {
      postHook({
        hook_event_name: "SessionStart",
        session_id: sessionIdOf(ctx),
        cwd: cwdOf(ctx),
        source: "pi",
      })
    } catch (err) {
      console.error("punk connect pi: session_start hook failed:", err && err.message ? err.message : err)
    }
  })

  // OBSERVATIONAL (see session_start's comment above): not awaited.
  // "extension"-sourced input is a synthetic pi.sendUserMessage(...) call
  // from another extension, not real user-authored text, and is excluded
  // - the same real-content-only discipline OpenCode's chat.message
  // translation applies to its synthetic/ignored parts (opencode_plugin.go).
  pi.on("input", async (event, ctx) => {
    try {
      const source = event && event.source
      if (source === "extension") {
        return
      }
      const prompt = (event && event.text) || ""
      const sessionID = sessionIdOf(ctx)
      // pi's "input" event carries no message/turn identifier in its
      // documented shape - the "real id" branch below is dormant today
      // and kept only so a future id field is picked up automatically
      // without a template change; the fallback mirrors OpenCode's own
      // "msg-" fnv1a fallback shape, including the NUL-separator join.
      const promptID = (event && event.id) || "msg-" + fnv1aHex(sessionID + %s + prompt)
      postHook({
        hook_event_name: "UserPromptSubmit",
        session_id: sessionID,
        prompt_id: promptID,
        prompt,
        cwd: cwdOf(ctx),
        source: "pi",
      })
    } catch (err) {
      console.error("punk connect pi: input hook failed:", err && err.message ? err.message : err)
    }
  })

  // OBSERVATIONAL (see session_start's comment above): not awaited. The
  // handler's return value can modify the tool result before the LLM sees
  // it (per pi's docs); this handler never returns anything, so tool
  // output is only observed, never altered.
  pi.on("tool_result", async (event, ctx) => {
    try {
      postHook({
        hook_event_name: "PostToolUse",
        session_id: sessionIdOf(ctx),
        tool_use_id: (event && event.toolCallId) || "",
        tool_name: (event && event.toolName) || "",
        tool_input: event && event.input,
        tool_response: event && event.content,
        cwd: cwdOf(ctx),
        source: "pi",
      })
    } catch (err) {
      console.error("punk connect pi: tool_result hook failed:", err && err.message ? err.message : err)
    }
  })

  // Not forwarded to the server on its own - turn_end has no Claude Code
  // hook equivalent. It only caches the latest assistant response text so
  // agent_settled's Stop capture (below) has real content instead of an
  // empty body, since agent_settled's own event object carries none. No
  // network call here at all, so there is nothing to classify
  // blocking/observational for this handler.
  pi.on("turn_end", (event, ctx) => {
    try {
      const text = extractAssistantText(event && event.message)
      if (text) {
        lastAssistantText = text
      }
    } catch (err) {
      console.error("punk connect pi: turn_end caching failed:", err && err.message ? err.message : err)
    }
  })

  // OBSERVATIONAL (see session_start's comment above): not awaited.
  // agent_settled fires once pi has settled and will not continue
  // automatically - the closest pi analog to Claude Code's Stop /
  // OpenCode's session.idle. Its event object carries no documented
  // fields of its own, so last_assistant_message comes from turn_end's
  // cache above; "status=settled" is the fallback when no assistant turn
  // has completed yet in this session, so this Stop capture's body is
  // never empty (the same lesson OpenCode's own session.idle translation
  // already applies - see opencode_plugin.go's "event" handler comment).
  pi.on("agent_settled", async (event, ctx) => {
    try {
      postHook({
        hook_event_name: "Stop",
        session_id: sessionIdOf(ctx),
        last_assistant_message: lastAssistantText || "status=settled",
        cwd: cwdOf(ctx),
        source: "pi",
      })
    } catch (err) {
      console.error("punk connect pi: agent_settled hook failed:", err && err.message ? err.message : err)
    }
  })

  // BLOCKING: the only handler here that awaits its network call. Fires
  // once per submitted prompt, not once per session (per pi's docs:
  // "Fired after user submits prompt, before agent loop"), so injection
  // is gated behind injectedSessions the same way OpenCode's
  // experimental.chat.system.transform is gated (opencode_plugin.go) -
  // context is fetched and appended to the system prompt once per
  // session, on that session's first submitted prompt, not re-fetched on
  // every subsequent prompt.
  pi.on("before_agent_start", async (event, ctx) => {
    try {
      const sessionID = sessionIdOf(ctx)
      if (!sessionID || injectedSessions.has(sessionID)) {
        return undefined
      }
      // Marked injected BEFORE the fetch, not after a successful response
      // - deliberate tradeoff, same as opencode_plugin.go's own
      // experimental.chat.system.transform: if this request fails
      // (timeout, network error), injection is disabled for the REST of
      // this session rather than retried on the next turn, so one
      // transient failure never causes the 2-second stall to repeat on
      // every subsequent turn.
      injectedSessions.add(sessionID)
      const data = await punkFetch("/v1/agent/context?cwd=" + encodeURIComponent(cwdOf(ctx)))
      if (data && typeof data.context === "string" && data.context.length > 0) {
        const base = event && event.systemPrompt
        if (typeof base !== "string" || base.length === 0) {
          // event.systemPrompt is documented as always populated on
          // before_agent_start, but if a future pi release ever omits or
          // empties it, "" + "\n\n" + data.context would silently BECOME
          // the entire system prompt for this turn instead of being
          // appended to it - fail safe instead: no base prompt means no
          // injection, never a punk-only system prompt.
          return undefined
        }
        return { systemPrompt: base + "\n\n" + data.context }
      }
      return undefined
    } catch (err) {
      console.error("punk connect pi: before_agent_start context injection failed:", err && err.message ? err.message : err)
      return undefined
    }
  })

  const PUNK_NAMESPACE_OVERRIDE = %s; // "" unless punk connect pi --project baked one
  let punkNamespaceCache = ""
  function punkCredentialsKey() {
    const fromEnv = process.env && process.env.PUNK_API_KEY
    if (fromEnv) return fromEnv
    try {
      const fs = require("node:fs")
      const os = require("node:os")
      const path = require("node:path")
      const p = (process.env && process.env.PUNK_CREDENTIALS) || path.join(os.homedir(), ".punk", "credentials.json")
      const c = JSON.parse(fs.readFileSync(p, "utf8"))
      return (c && c.api_key) || ""
    } catch (_) {
      return ""
    }
  }
  async function punkAPICall(path, init) {
    const headers = Object.assign({ "Content-Type": "application/json" }, (init && init.headers) || {})
    const key = punkCredentialsKey()
    if (key) headers["Authorization"] = "Bearer " + key
    const res = await fetch(punkServerURL() + path, Object.assign({}, init, { headers }))
    const text = await res.text()
    if (!res.ok) throw new Error("punk " + res.status + ": " + text.slice(0, 300))
    return text ? JSON.parse(text) : null
  }
  async function punkNamespace(ctx) {
    if (PUNK_NAMESPACE_OVERRIDE) return PUNK_NAMESPACE_OVERRIDE
    if (punkNamespaceCache) return punkNamespaceCache
    const out = await punkAPICall("/v1/agent/namespace?cwd=" + encodeURIComponent(ctx.cwd || process.cwd()))
    punkNamespaceCache = (out && out.namespace) || "agent-default"
    return punkNamespaceCache
  }
  const textResult = (obj) => ({ content: [{ type: "text", text: typeof obj === "string" ? obj : JSON.stringify(obj) }], details: {} })

  pi.registerTool({
    name: "punk_whoami",
    label: "Punk whoami",
    description: "Namespace and server this session's punk memory tools use.",
    promptSnippet: "Show which punk memory namespace this project maps to",
    parameters: { type: "object", properties: {}, additionalProperties: false },
    async execute(_id, _params, _signal, _onUpdate, ctx) {
      return textResult({ namespace: await punkNamespace(ctx), server: punkServerURL() })
    },
  })
  pi.registerTool({
    name: "punk_recall",
    label: "Punk recall",
    description: "Recall the latest live facts under a key prefix from punk memory (deterministic, unranked).",
    promptSnippet: "Read punk memory facts under a known key prefix such as /decisions",
    parameters: { type: "object", properties: { prefix: { type: "string", description: "key prefix, e.g. /decisions" } }, required: ["prefix"], additionalProperties: false },
    async execute(_id, params, _signal, _onUpdate, ctx) {
      const ns = await punkNamespace(ctx)
      return textResult(await punkAPICall("/v1/namespaces/" + encodeURIComponent(ns) + "/memories?prefix=" + encodeURIComponent(params.prefix) + "&max_tokens=1500"))
    },
  })
  pi.registerTool({
    name: "punk_search",
    label: "Punk search",
    description: "Ranked hybrid search over punk memory; compact hits (key, clipped body, score, flags). Put exact identifiers or error strings in anchors.",
    promptSnippet: "Search punk memory when wording or location of prior context is unknown",
    promptGuidelines: ["Use punk_search before re-deriving a decision, convention or incident that an earlier session may have recorded."],
    parameters: { type: "object", properties: { query: { type: "string" }, anchors: { type: "array", items: { type: "string" } } }, required: ["query"], additionalProperties: false },
    async execute(_id, params, _signal, _onUpdate, ctx) {
      const ns = await punkNamespace(ctx)
      let q = "/v1/namespaces/" + encodeURIComponent(ns) + "/memories/search?mode=hybrid&scored=1&format=compact&max_tokens=1500&q=" + encodeURIComponent(params.query)
      for (const a of params.anchors || []) q += "&anchor=" + encodeURIComponent(a)
      return textResult(await punkAPICall(q))
    },
  })
  pi.registerTool({
    name: "punk_remember",
    label: "Punk remember",
    description: "Store a durable fact (decision, fix, convention, gotcha) in punk memory under a hierarchical key; latest wins per key.",
    promptSnippet: "Persist a durable decision or gotcha to punk memory",
    parameters: { type: "object", properties: { key: { type: "string", description: "hierarchical key like /decisions/auth" }, body: { type: "string" }, importance: { type: "number", minimum: 0, maximum: 1 } }, required: ["key", "body"], additionalProperties: false },
    async execute(_id, params, _signal, _onUpdate, ctx) {
      const ns = await punkNamespace(ctx)
      const out = await punkAPICall("/v1/namespaces/" + encodeURIComponent(ns) + "/memories", {
        method: "POST",
        body: JSON.stringify({ key: params.key, body: params.body, importance: params.importance || 0, author: "pi" }),
      })
      return textResult({ stored: out && out.key, id: out && out.id })
    },
  })
}
`

// nulJSStringLiteral is the double-quoted JS string literal for a single
// NUL character: a double quote, a backslash, the four digits 0000, and a
// closing double quote (eight characters total) - used as the separator
// joining sessionID and prompt in the rendered extension's fnv1aHex
// fallback prompt_id derivation, mirroring opencode_plugin.go's own
// NUL-separator join. Built here from the literal backslash rune and the
// string "u0000", rather than typed as one contiguous backslash-u-0000
// escape sequence in this source file: per .claude/rules/ai.md, typing
// that sequence directly as a literal in an editing tool's parameter has
// been observed to be decoded into an actual 0x00 byte landing in the Go
// source itself (which then corrupts this file) rather than staying as
// six literal characters - constructing it at runtime from an explicit
// rune value sidesteps that failure mode entirely.
var nulJSStringLiteral = func() string {
	backslash := rune(0x5C)
	return `"` + string(backslash) + "u0000" + `"`
}()

// piExtensionContent renders the full extension source with serverURL
// baked in as the PUNK_URL fallback default - still overridable at
// runtime via the PUNK_URL environment variable, see the rendered
// extension's own header comment and punkServerURL() - and the NUL-escape
// string literal (see nulJSStringLiteral) spliced into the fallback
// prompt_id derivation.
func piExtensionContent(serverURL string) string {
	return piExtensionContentNS(serverURL, "")
}

// piExtensionContentNS is piExtensionContent with a namespace override
// literal baked in (punk connect pi --project); "" renders an empty
// string and the extension derives the namespace per session instead.
func piExtensionContentNS(serverURL, namespace string) string {
	return fmt.Sprintf(piExtensionTemplate, jsStringLiteral(serverURL), nulJSStringLiteral, jsStringLiteral(namespace))
}
