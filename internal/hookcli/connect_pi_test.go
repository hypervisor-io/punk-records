package hookcli

// The full node-driven round trip (rendering the extension via ConnectPi,
// then driving it under node against a REAL server contract) lives in
// internal/api/pi_node_roundtrip_test.go
// (TestPiExtensionNodeRoundTripsThroughRealServer), mirroring
// internal/api/opencode_node_roundtrip_test.go: only internal/api can pair
// the rendered extension with Server.Router() (internal/hookcli cannot
// import internal/api - see that test's own doc comment for the full
// reasoning). What stays here is package-local: byte-exact golden
// content, file-write semantics (idempotency, unmanaged-file refusal,
// symlink/mode preservation, parent dir creation), hostile-input escaping,
// and syntax-only sanity checks - none of which need a live server.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// piGoldenContent pins the EXACT byte-for-byte extension source a fresh
// extension file gets for serverURL "http://localhost:9090": the managed
// marker as the first line, the module export shape, and all six wired
// events (session_start, input, tool_result, turn_end, agent_settled,
// before_agent_start). A mere strings.Contains check would miss a wrong
// event name, a dropped field, or broken JS/TS syntax around the
// substitution points.
const piGoldenContent = `// managed by punk connect pi
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
    return (fromEnv || "http://localhost:9090").replace(/\/+$/, "")
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
      const promptID = (event && event.id) || "msg-" + fnv1aHex(sessionID + "\u0000" + prompt)
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

  const PUNK_NAMESPACE_OVERRIDE = ""; // "" unless punk connect pi --project baked one
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

// TestConnectPiGoldenContent pins the exact bytes ConnectPi writes for a
// fresh extension file.
func TestConnectPiGoldenContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.ts")
	changed, err := ConnectPi(path, "http://localhost:9090", PiOpts{})
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != piGoldenContent {
		t.Fatalf("golden mismatch:\n got:  %q\nwant: %q", got, piGoldenContent)
	}
	if !strings.HasPrefix(string(got), piExtensionMarker) {
		t.Fatalf("marker must be the first line, got: %q", got[:min(len(got), 80)])
	}
}

// TestConnectPiIdempotent verifies a re-run with identical inputs reports
// changed=false and does not rewrite the file.
func TestConnectPiIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.ts")
	if changed, err := ConnectPi(path, "http://localhost:9090", PiOpts{}); err != nil || !changed {
		t.Fatal(changed, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectPi(path, "http://localhost:9090", PiOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("idempotent re-run must report changed=false")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("idempotent re-run must not rewrite the file")
	}
}

// TestConnectPiRefusesUnmanagedExisting verifies an existing extension
// file without the managed marker on its first line is left completely
// untouched and refused with an error naming the path - overwriting a
// user's own hand-authored pi extension would destroy their content
// silently otherwise.
func TestConnectPiRefusesUnmanagedExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.ts")
	original := []byte("// my own hand-authored pi extension\nexport default function (pi) {}\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ConnectPi(path, "http://localhost:9090", PiOpts{})
	if err == nil {
		t.Fatal("expected error for unmanaged existing extension file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("expected error naming the path, got: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("unmanaged file must be left untouched: got %q, want %q", after, original)
	}
}

// TestConnectPiUpdatesStaleManagedContent verifies an extension file that
// DOES carry the managed marker (e.g. from a prior punk version, or a
// different serverURL baked into the PUNK_URL fallback) is updated in
// place rather than refused.
func TestConnectPiUpdatesStaleManagedContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.ts")
	stale := piExtensionMarker + "\nexport default function (pi) { /* stale, mentions http://old:1 */ }\n"
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectPi(path, "http://localhost:9090", PiOpts{})
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "http://old:1") {
		t.Fatalf("stale content must be replaced: %s", after)
	}
	if !strings.Contains(string(after), "http://localhost:9090") {
		t.Fatalf("expected new server URL baked in: %s", after)
	}
}

// TestConnectPiSymlinkedExtensionStaysSymlink verifies that when the
// extension path is a symlink (e.g. into a dotfiles repo), connecting
// updates the content the symlink points at in place rather than
// replacing the symlink with a plain file.
func TestConnectPiSymlinkedExtensionStaysSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-punk-memory.ts")
	if err := os.WriteFile(real, []byte(piExtensionMarker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "punk-memory.ts")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectPi(link, "http://localhost:9090", PiOpts{}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("extension symlink was replaced by a regular file")
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if target != real {
		t.Fatalf("symlink now points elsewhere: %s", target)
	}
	raw, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "export default function") {
		t.Fatalf("symlink target missing extension content: %s", raw)
	}
}

// TestConnectPiPreservesExistingFileMode verifies connecting against an
// existing extension file does not widen its permissions.
func TestConnectPiPreservesExistingFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.ts")
	if _, err := ConnectPi(path, "http://a:1", PiOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectPi(path, "http://b:2", PiOpts{}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 0600 preserved, got %o", fi.Mode().Perm())
	}
}

// TestConnectPiCreatesParentDirs verifies ConnectPi creates the
// extension's parent directory tree (e.g. ~/.pi/agent/extensions/ or
// ./.pi/extensions/, neither of which typically pre-exists) rather than
// requiring the caller to mkdir first.
func TestConnectPiCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pi", "extensions", "punk-memory.ts")
	changed, err := ConnectPi(path, "http://localhost:9090", PiOpts{})
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected extension file to exist after parent dir creation: %v", err)
	}
}

// TestConnectPiEscapesHostileServerURL verifies a serverURL containing
// characters that are meaningful inside a JS/TS string literal (a double
// quote, a backslash) is escaped rather than breaking out of the
// generated string literal - per .claude/rules/ai.md's "always
// unconditionally escape" rule. If node or bun is on PATH, the emitted
// file is additionally syntax-checked so a broken escape would fail
// loudly instead of merely "looking escaped" to a substring check.
func TestConnectPiEscapesHostileServerURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.ts")
	hostile := `http://evil","x":"pwned`
	if _, err := ConnectPi(path, hostile, PiOpts{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, `\"x\":\"pwned`) {
		t.Fatalf("expected hostile quotes to be escaped, got: %s", s)
	}
	if !strings.Contains(s, `(fromEnv || "http://evil\"`) {
		t.Fatalf("hostile URL did not stay inside the intended string literal: %s", s)
	}
	runPiSyntaxCheck(t, path, got)
}

// TestPiExtensionPassesSyntaxCheck is a lightweight sanity check that the
// emitted extension is syntactically valid, using whichever of node/bun is
// available in the test environment; skipped when neither is on PATH. The
// extension has a .ts extension (required for pi's own auto-discovery
// pattern) but contains no TypeScript-only syntax (see pi_extension.go's
// doc comment), so it validates as plain JavaScript too.
func TestPiExtensionPassesSyntaxCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.ts")
	if _, err := ConnectPi(path, "http://localhost:9090", PiOpts{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	runPiSyntaxCheck(t, path, got)
}

// runPiSyntaxCheck syntax-checks content with node (preferred) or bun,
// t.Skip-ing when neither is on PATH. Mirrors runJSSyntaxCheck
// (connect_opencode_test.go): node's CommonJS-by-default loader rejects
// top-level "export" outside a module context even under --check, so
// content is copied to a sibling .mjs file (unambiguously ESM by
// extension, and plain JS since this template uses no TS-only syntax)
// rather than checked at its original .ts path; bun infers ESM from
// syntax regardless of extension, so it checks the original path
// directly via "bun build".
func runPiSyntaxCheck(t *testing.T, path string, content []byte) {
	t.Helper()
	if nodePath, err := exec.LookPath("node"); err == nil {
		mjs := path + ".syntax-check.mjs"
		if err := os.WriteFile(mjs, content, 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(nodePath, "--check", mjs).CombinedOutput()
		if err != nil {
			t.Fatalf("node --check %s failed: %v\n%s", mjs, err, out)
		}
		return
	}
	if bunPath, err := exec.LookPath("bun"); err == nil {
		outDir := path + ".syntax-check-out"
		out, err := exec.Command(bunPath, "build", path, "--outdir", outDir).CombinedOutput()
		if err != nil {
			t.Fatalf("bun build %s failed: %v\n%s", path, err, out)
		}
		return
	}
	t.Skip("neither node nor bun on PATH; skipping extension syntax sanity check")
}

// TestConnectPiEscapesLineAndParagraphSeparators mirrors
// TestConnectOpenCodeEscapesLineAndParagraphSeparators
// (connect_opencode_test.go): jsStringLiteral's U+2028/U+2029 re-escape
// branch (opencode_plugin.go, shared by piExtensionContent) had no test
// exercising an ACTUAL literal separator character reaching it there
// either. A serverURL containing a raw U+2028/U+2029 must come out of
// jsStringLiteral escaped, never as the raw three-byte UTF-8 separator.
// The separator runes are written here as ordinary Go "\u2028"/"\u2029"
// escapes (six ASCII characters each in this source file, interpreted by
// the Go compiler into the real rune) rather than as literal multi-byte
// characters typed directly into the file, for the same
// tooling-corruption reason documented on nulJSStringLiteral
// (pi_extension.go) and .claude/rules/ai.md.
func TestConnectPiEscapesLineAndParagraphSeparators(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.ts")
	hostile := "http://evil.example/\u2028mid\u2029end"
	if _, err := ConnectPi(path, hostile, PiOpts{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.ContainsRune(s, '\u2028') || strings.ContainsRune(s, '\u2029') {
		t.Fatalf("raw U+2028/U+2029 must never reach the emitted extension unescaped: %q", s)
	}
	if !strings.Contains(s, `\u2028mid\u2029end`) {
		t.Fatalf("expected the escaped \\u2028/\\u2029 sequences in the fallback string literal, got: %s", s)
	}
	runPiSyntaxCheck(t, path, got)
}

func TestPiExtensionRegistersTools(t *testing.T) {
	p := filepath.Join(t.TempDir(), "punk-memory.ts")
	if _, err := ConnectPi(p, "http://localhost:9090", PiOpts{}); err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile(p)
	for _, want := range []string{`name: "punk_whoami"`, `name: "punk_recall"`, `name: "punk_search"`, `name: "punk_remember"`, "/v1/agent/namespace", "/memories/search", "format=compact"} {
		if !strings.Contains(string(src), want) {
			t.Fatalf("extension missing %q", want)
		}
	}
	if strings.Contains(string(src), `PUNK_NAMESPACE_OVERRIDE = "agent-`) {
		t.Fatal("no override without --project namespace")
	}
	if _, err := ConnectPi(p, "http://localhost:9090", PiOpts{Namespace: "agent-x-abcdef"}); err != nil {
		t.Fatal(err)
	}
	src, _ = os.ReadFile(p)
	if !strings.Contains(string(src), `const PUNK_NAMESPACE_OVERRIDE = "agent-x-abcdef"`) {
		t.Fatal("namespace override not baked")
	}
}
