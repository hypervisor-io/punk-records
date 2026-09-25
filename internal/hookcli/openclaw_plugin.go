package hookcli

import (
	"fmt"
	"strings"
)

// openClawPluginMarker is the required first line of the plugin entry file
// WriteOpenClawPlugin writes, identifying it as punk-managed exactly the way
// openCodePluginMarker gates ConnectOpenCode: an existing file at the target
// path whose first line is NOT this marker is presumed hand-authored (a real
// user plugin, or one installed by a different tool) and is never
// overwritten.
const openClawPluginMarker = "// managed by punk connect openclaw"

// OpenClawPluginID is the plugin id punk registers itself under. It is the
// directory name under the plugins root, the "id" the plugin object
// declares, and the key in config.json's plugins.entries map - all three
// must agree for OpenClaw to associate the loaded plugin with its config
// entry, so they all come from this one constant.
const OpenClawPluginID = "punk-memory"

// nulSeparator is the two-character JavaScript escape for U+0000, built
// programmatically rather than typed as a literal escape sequence: per
// .claude/rules/ai.md, a typed unicode NUL escape has twice landed in this
// repo as a raw control byte in source instead of the six characters it was
// meant to be. Building it from the backslash's own code point makes that
// impossible. It is spliced into the generated JS as the field separator in
// the prompt-id hash below, so "ab"+"c" and "a"+"bc" never hash identically.
var nulSeparator = string(rune(0x5C)) + "u0000"

// openClawPluginTemplate is the full JavaScript source WriteOpenClawPlugin
// writes. Substitution points, in order: the managed marker, the PUNK_URL
// fallback default (rendered via jsStringLiteral so a serverURL containing a
// quote or backslash can never break out of its string literal), the plugin
// id, the plugin id again for the display name, and the NUL separator.
//
// Design notes, since this is a JS file most readers will only ever see as
// generated output:
//
//   - Plugin layout, per the CURRENT documented loader
//     (docs.openclaw.ai/plugins/manifest and /plugins/manifest/package-json,
//     fetched 2026-09-25; the hooks guide restructured into child pages
//     since punk's first research): a directory under the plugins root
//     holding openclaw.plugin.json (REQUIRED for every native plugin - a
//     missing or invalid manifest blocks config validation), package.json
//     (whose "openclaw".extensions array declares the entrypoints - the
//     "openclaw".pluginEntry field punk wrote before M8 is absent from the
//     current documented field list) plus the entry file. The hook API
//     shape (a plugin object exposing id, name and register(api), with
//     register calling api.on(<hook>, handler, opts)) is documented at
//     docs.openclaw.ai/plugins/hooks. The docs' own examples wrap the plugin
//     object in a definePluginEntry() helper imported from the openclaw
//     plugin SDK; this file deliberately exports the plain object instead,
//     because the same page also documents runtime registration by "an
//     object with id, name, configSchema, and register(api) function". That
//     avoids a bare-specifier import that a standalone generated file has no
//     node_modules to resolve.
//   - Hook selection: session_start (observation), before_prompt_build
//     (the only documented hook that can both see the prompt and return
//     prependContext), after_tool_call (observation), agent_end
//     (observation, carries the run's messages). before_tool_call and every
//     other decision hook is deliberately not registered: punk observes,
//     it never blocks a tool call. session_end is not registered either -
//     it maps to the same Stop capture key agent_end already writes, and
//     its documented budget is the tightest of any hook (2 seconds for all
//     handlers combined), so registering it would risk that budget to
//     overwrite a richer fact with a poorer one.
//   - The handler argument shapes come from the same page's per-hook
//     payload table (before_prompt_build: {prompt, messages};
//     after_tool_call: {toolName, params, result, error, durationMs};
//     agent_end: {runId, messages, success, durationMs}; session_start:
//     {sessionKey, sessionId, reason}) and its "context available to all
//     hooks" list (ctx.sessionKey, ctx.sessionId, ctx.runId, ...). The page
//     does not spell out the handler's parameter ORDER, so every accessor
//     below reads defensively from both the event and the context object
//     and falls back rather than assuming which one carries a field - an
//     inference, not a documented contract.
//   - Session identity prefers ctx.sessionId (a UUID) over sessionKey: a
//     session key is a channel-scoped composite that can carry separators
//     the server's own id sanitizer strips, which could fold two distinct
//     sessions onto one namespace.
//   - Every network call goes through punkFetch: a 2-second AbortController
//     timeout covering the ENTIRE request INCLUDING reading the response
//     body (fetch resolves as soon as headers arrive, so a server that
//     sends headers then stalls the body would otherwise hang the host
//     session - the timer is only cleared in finally, after the body has
//     been read or the abort has fired). Every failure - timeout, network
//     error, non-OK status, malformed JSON - resolves to null instead of
//     rejecting, so callers never need a surrounding try/catch and it is
//     always safe to leave a capture call un-awaited. A non-OK response's
//     body is explicitly cancelled since it is never read in that branch.
//   - Awaiting: the three observation hooks fire their capture POST without
//     awaiting it, so punk can never add latency to a user's turn. agent_end
//     is the one deliberate exception to that rule: the docs describe the
//     one-shot CLI as waiting on agent_end (while the Gateway treats it as
//     fire-and-forget), which means in CLI mode the process can exit the
//     moment the handler returns - an un-awaited POST there would routinely
//     lose the session's final Stop capture. Its budget is documented as
//     the most generous of any hook, and punkFetch is bounded at 2 seconds
//     regardless, so awaiting costs nothing the host was not already
//     prepared to wait for.
//   - Every handler body is wrapped in its own try/catch so a bug here can
//     never throw into OpenClaw and interrupt a user's session (fail-open,
//     mirroring hookcli's own hook-forwarder contract).
const openClawPluginTemplate = `%s
//
// punk-records agent memory for OpenClaw. Regenerate with:
//   punk connect openclaw
//
// Capture: session_start, before_prompt_build, after_tool_call, agent_end
// are forwarded to punk's /v1/agent/hooks as Claude-shaped envelopes.
// Injection: before_prompt_build returns prependContext holding this
// project's recalled memory, once per session.

const PUNK_DEFAULT_URL = %s;
const PUNK_PLUGIN_ID = %s;
const PUNK_TIMEOUT_MS = 2000;

function punkURL() {
  const raw =
    (typeof process !== "undefined" && process.env && process.env.PUNK_URL) ||
    PUNK_DEFAULT_URL;
  return String(raw).replace(/\/+$/, "");
}

function punkHeaders() {
  const headers = { "Content-Type": "application/json" };
  const key =
    (typeof process !== "undefined" && process.env && process.env.PUNK_API_KEY) || "";
  if (key) headers["Authorization"] = "Bearer " + key;
  return headers;
}

async function punkFetch(path, init) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), PUNK_TIMEOUT_MS);
  try {
    const res = await fetch(punkURL() + path, {
      ...(init || {}),
      headers: punkHeaders(),
      signal: controller.signal,
    });
    if (!res.ok) {
      try {
        if (res.body) await res.body.cancel();
      } catch (err) {
        // Draining a non-OK body is best-effort cleanup; a failure here
        // must not become the caller's problem.
      }
      return null;
    }
    return await res.json();
  } catch (err) {
    return null;
  } finally {
    clearTimeout(timer);
  }
}

function punkCapture(envelope) {
  return punkFetch("/v1/agent/hooks", {
    method: "POST",
    body: JSON.stringify(envelope),
  });
}

async function punkContext(cwd) {
  const data = await punkFetch(
    "/v1/agent/context?cwd=" + encodeURIComponent(cwd),
    { method: "GET" },
  );
  if (data && typeof data.context === "string" && data.context.length > 0) {
    return data.context;
  }
  return "";
}

// fnv1aHex mirrors hookcli's own fnv32aHex (Go) in algorithm only: this
// hashes UTF-16 code units where Go hashes UTF-8 bytes, so the two id
// spaces are independent and never compared. It exists for the same
// reason Go's does - the server drops any UserPromptSubmit whose prompt_id
// sanitizes to empty, and OpenClaw's before_prompt_build payload carries
// no per-turn id of its own.
function fnv1aHex(input) {
  let hash = 0x811c9dc5;
  for (let i = 0; i < input.length; i++) {
    hash ^= input.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return hash.toString(16).padStart(8, "0");
}

function punkCwd() {
  try {
    return process.cwd();
  } catch (err) {
    return "";
  }
}

function punkSessionID(event, ctx) {
  const e = event || {};
  const c = ctx || {};
  return String(
    c.sessionId || e.sessionId || c.sessionKey || e.sessionKey || "",
  );
}

function punkEnvelope(event, ctx, name) {
  return {
    hook_event_name: name,
    session_id: punkSessionID(event, ctx),
    cwd: punkCwd(),
    source: "openclaw",
  };
}

// punkText flattens the several shapes a prompt or message can arrive in
// (a bare string, an object with .text or .content, or a content array of
// parts) down to plain text, returning "" when there is none to be found.
function punkText(value) {
  if (typeof value === "string") return value;
  if (value && typeof value === "object") {
    if (typeof value.text === "string") return value.text;
    if (typeof value.content === "string") return value.content;
    if (Array.isArray(value.content)) {
      return value.content.map(punkText).filter(Boolean).join("\n");
    }
  }
  return "";
}

function punkLastAssistantText(messages) {
  if (!Array.isArray(messages)) return "";
  for (let i = messages.length - 1; i >= 0; i--) {
    const message = messages[i];
    if (message && message.role === "assistant") {
      const text = punkText(message);
      if (text) return text;
    }
  }
  return "";
}

// punkInjected gates context injection to once per session. An in-process
// Set is the right shape here specifically because an OpenClaw plugin is
// loaded once into a long-lived host (the Gateway) rather than re-executed
// per event the way a subprocess hook is; a one-shot CLI run simply starts
// with an empty Set and injects on its first turn, which is the same
// once-per-session result.
const punkInjected = new Set();

%s

// The plugin object is declared as a named const (not an anonymous export
// literal) so punk's behavioral harness can call register() directly;
// the default export shape OpenClaw loads is unchanged.
const punkOpenClawPlugin = {
  id: PUNK_PLUGIN_ID,
  name: "punk memory",
  register(api) {
    api.on("session_start", async (event, ctx) => {
      try {
        punkCapture(punkEnvelope(event, ctx, "SessionStart"));
        // Messaging bind (M8): starts the fire-and-forget registration
        // retry loop for this session's address. OpenClaw documents no
        // turn-start plugin API (verified 2026-09-25,
        // /research/extension-clients/openclaw), so the bridge is
        // catch-up only: no SSE listener, no wake, no busy state.
        punkBindSession(event, ctx);
      } catch (err) {
        console.error("[punk] session_start:", err);
      }
    });

    api.on("before_prompt_build", async (event, ctx) => {
      try {
        const envelope = punkEnvelope(event, ctx, "UserPromptSubmit");
        const prompt = punkText(event && event.prompt);
        if (prompt) {
          punkCapture({
            ...envelope,
            prompt,
            prompt_id: fnv1aHex(envelope.session_id + "%s" + prompt),
          });
        }
        // Memory recall stays exactly as it was: once per session, marked
        // only after a successful non-empty recall (the one-shot CLI tradeoff
        // documented on punkInjected).
        let prepend = "";
        if (!envelope.session_id || !punkInjected.has(envelope.session_id)) {
          const recalled = await punkContext(envelope.cwd);
          if (recalled) {
            if (envelope.session_id) punkInjected.add(envelope.session_id);
            prepend = recalled;
          }
        }
        // Messaging catch-up (M8): the leased unread set for this
        // session, rendered with the shared M5 envelope and prepended to
        // the same prompt the memory context rides. Delivery happens only
        // through this hook - the ONLY documented seam a plugin has for
        // adding content to an OpenClaw turn.
        const inbox = await punkInboxCatchUp(envelope);
        if (inbox) {
          prepend = prepend ? prepend + "\n\n" + inbox : inbox;
        }
        if (!prepend) return;
        return { prependContext: prepend };
      } catch (err) {
        console.error("[punk] before_prompt_build:", err);
        return;
      }
    });

    api.on("after_tool_call", async (event, ctx) => {
      try {
        const e = event || {};
        const envelope = punkEnvelope(event, ctx, "PostToolUse");
        const toolName = String(e.toolName || "");
        const params = e.params === undefined ? null : e.params;
        // after_tool_call's documented payload carries no call id, and the
        // server drops any PostToolUse whose tool_use_id sanitizes to
        // empty. This event fires once per discrete tool CALL rather than
        // once per target resource, so Date.now() is folded into the hash
        // as the per-call discriminator: without it, running the same
        // command twice in one session would collapse onto a single
        // capture key. Two calls landing in the same millisecond still
        // collapse - the same last-write-wins tradeoff every synthesized
        // id in this project accepts.
        envelope.tool_name = toolName;
        envelope.tool_input = params;
        envelope.tool_response =
          e.error === undefined || e.error === null
            ? e.result === undefined
              ? null
              : e.result
            : { error: e.error, result: e.result === undefined ? null : e.result };
        envelope.tool_use_id =
          "call-" +
          fnv1aHex(
            envelope.session_id +
              "%s" +
              toolName +
              "%s" +
              String(Date.now()) +
              "%s" +
              JSON.stringify(params === null ? "" : params),
          );
        punkCapture(envelope);
      } catch (err) {
        console.error("[punk] after_tool_call:", err);
      }
    });

    api.on("agent_end", async (event, ctx) => {
      try {
        const e = event || {};
        const envelope = punkEnvelope(event, ctx, "Stop");
        const text = punkLastAssistantText(e.messages);
        // Fall back to the run's outcome only when no assistant text can be
        // recovered: an empty body would store a fact carrying nothing at
        // all, the same reasoning every other adapter's Stop fallback
        // documents.
        envelope.last_assistant_message =
          text || "success=" + String(e.success) + " durationMs=" + String(e.durationMs);
        // Awaited on purpose - see this file's header note on agent_end.
        await punkCapture(envelope);
      } catch (err) {
        console.error("[punk] agent_end:", err);
      }
    });

    // MESSAGING TEARDOWN (no capture). Gateway shutdown aborts every
    // registration retry the bridge still has pending; the docs list
    // gateway_stop as the lifecycle flush point. Idempotent, no network.
    api.on("gateway_stop", async (event, ctx) => {
      try {
        punkTeardownAll();
      } catch (err) {
        console.error("[punk] gateway_stop:", err);
      }
    });
  },
};

export default punkOpenClawPlugin;
`

// openClawPluginSource renders the plugin entry file for serverURL. Verb
// order in the template: managed marker, PUNK_URL fallback, plugin id,
// the messaging-bridge splice (after the punkInjected declaration, before
// the register body), then the four NUL separators inside the capture
// handlers.
func openClawPluginSource(serverURL string) string {
	return fmt.Sprintf(openClawPluginTemplate,
		openClawPluginMarker,
		jsStringLiteral(serverURL),
		jsStringLiteral(OpenClawPluginID),
		openClawMessagingBridgeJS(),
		nulSeparator, nulSeparator, nulSeparator, nulSeparator)
}

// openClawPackageJSON renders the plugin's package.json, following the
// CURRENT documented loader (docs.openclaw.ai/plugins/manifest and
// /plugins/manifest/package-json, fetched 2026-09-25): "type":"module" is
// required for the entry file's `export default` to parse as ESM, and
// package.json#openclaw.EXTENSIONS is the field that declares native
// plugin entrypoints. The retired "openclaw".pluginEntry shape punk wrote
// before M8 (and the package.json permissions block, whose gates actually
// live in config.json under plugins.entries.<id>.hooks.* - see
// ConnectOpenClaw, which writes them) are gone: a current OpenClaw
// requires the openclaw.plugin.json manifest (openClawPluginManifest
// below) and does not document pluginEntry at all.
func openClawPackageJSON() string {
	return `{
  "name": "` + OpenClawPluginID + `",
  "version": "1.0.0",
  "private": true,
  "type": "module",
  "main": "./index.js",
  "openclaw": {
    "extensions": ["./index.js"]
  }
}
`
}

// openClawPluginManifest renders the openclaw.plugin.json manifest every
// native OpenClaw plugin MUST ship in its plugin root ("A missing or
// invalid manifest blocks config validation and is treated as a plugin
// error" - docs.openclaw.ai/plugins/manifest, fetched 2026-09-25). The
// first line is the managed marker as a JSON5 comment: native manifests
// are parsed with JSON5, which accepts comments, so the marker doubles as
// punk's ownership proof for the never-overwrite rule in
// WriteOpenClawPlugin. id must equal the plugins.entries key and the
// package name (OpenClawPluginID is all three); configSchema is required
// even for a plugin that accepts no config, and punk's config-side
// settings all live in the operator's config.json, not here.
func openClawPluginManifest() string {
	return openClawPluginMarker + "\n" + `{
  "id": "` + OpenClawPluginID + `",
  "name": "punk memory",
  "description": "punk-records agent memory and agent-message inbox catch-up for OpenClaw",
  "version": "1.0.0",
  "configSchema": {
    "type": "object",
    "additionalProperties": false,
    "properties": {}
  }
}
`
}

// hasOpenClawMarker reports whether src's first line is the managed marker.
// Split on "\n" and trimmed of a trailing "\r" so a file checked out with
// CRLF line endings is still recognized as ours rather than being refused
// as hand-authored.
func hasOpenClawMarker(src string) bool {
	first := src
	if i := strings.IndexByte(src, '\n'); i >= 0 {
		first = src[:i]
	}
	return strings.TrimRight(first, "\r") == openClawPluginMarker
}

// openClawBridgeCoreJS is the OpenClaw-specific half of the
// agent-messaging bridge: everything except the shared envelope renderer
// (inboxRendererJS). A plain raw-string constant - no fmt verbs, no
// backticks, no literal percent characters - spliced into
// openClawPluginTemplate. Verified scope (2026-09-25,
// /research/extension-clients/openclaw): OpenClaw documents NO plugin API
// that can start a turn - enqueueNextTurnInjection only queues context
// for the next prompt build, heartbeat_prompt_contribution fires only on
// heartbeat turns, before_agent_run only blocks, and webhooks are
// operator-configured Gateway HTTP endpoints - so this bridge is
// CATCH-UP ONLY: bind, register (confirmed, retried), and inject the
// leased unread set through before_prompt_build's prependContext, the one
// documented seam for adding content to a turn. No SSE, no wake, no busy
// state, no continuation. It re-applies the reviewed OpenCode bridge's
// disciplines: confirmed registration before any delivery, bounded
// cancellable retry sleeps, delivered-but-unacked ids re-acked without
// re-rendering, and the sender allowlist.
const openClawBridgeCoreJS = `
  // ---- punk agent-messaging bridge (opt-in: PUNK_MESSAGING=1) ----
  // Catch-up only: OpenClaw has no turn-start plugin API (verified). The
  // client machinery - leased fetch passes (with the allowlist
  // starvation fix), owner ACKs with partial-success handling, releases
  // and the allowlist - is the shared inboxBridgeCoreJS spliced in
  // BEFORE this block; what stays here is OpenClaw-specific: binding,
  // the confirmed-registration retry loop, gateway_stop teardown, and
  // the before_prompt_build catch-up itself.
  const punkMessagingEnabled =
    typeof process !== "undefined" && process.env && process.env.PUNK_MESSAGING === "1";
  const punkSessions = new Map();

  function punkSessionState(sessionID) {
    let st = punkSessions.get(sessionID);
    if (st) return st;
    st = punkInboxState(sessionID, "openclaw");
    st.registered = false;
    st.registering = false;
    st.ns = "";
    punkSessions.set(sessionID, st);
    return st;
  }

  // Namespace: PUNK_NAMESPACE env, else the server's cwd lookup. Only
  // successes cache; there is no "agent-default" fallback because a wrong
  // namespace would silently orphan the session's inbox.
  let punkMsgNamespaceCache = "";
  async function punkMessagingResolveNamespace() {
    if (punkMsgNamespaceCache) return punkMsgNamespaceCache;
    const env = typeof process !== "undefined" && process.env && process.env.PUNK_NAMESPACE;
    if (env) {
      punkMsgNamespaceCache = env;
      return env;
    }
    const data = await punkFetch("/v1/agent/namespace?cwd=" + encodeURIComponent(punkCwd()));
    if (data && typeof data.namespace === "string" && data.namespace) {
      punkMsgNamespaceCache = data.namespace;
    }
    return punkMsgNamespaceCache;
  }

  // Bind: start the registration flow for this session's address. Inert
  // when messaging is off or the session id is unusable.
  function punkBindSession(event, ctx) {
    if (!punkMessagingEnabled) return;
    const sid = punkSessionID(event, ctx);
    if (!sid) return;
    const st = punkSessionState(sid);
    if (st.registered || st.registering) return;
    st.registering = true;
    punkRegisterSession(st);
  }

  // Registration is confirmed, not assumed: the member POST retries on
  // bounded exponential backoff until the server answers
  // {status:"registered"}; no inbox read happens before that. Cancelled
  // instantly by teardown. Never rejects.
  async function punkRegisterSession(st) {
    try {
      let attempt = 0;
      while (punkInboxStAlive(st) && !st.registered) {
        const ns = await punkMessagingResolveNamespace();
        if (!punkInboxStAlive(st) || st.registered) return;
        if (ns) {
          const res = await punkFetch("/v1/namespaces/" + encodeURIComponent(ns) + "/members", {
            method: "POST",
            body: JSON.stringify({ agent: st.agent, role: "satellite" }),
          });
          if (!punkInboxStAlive(st) || st.registered) return;
          if (res && res.status === "registered") {
            st.registered = true;
            st.registering = false;
            st.ns = ns;
            return;
          }
          console.error("[punk] messaging registration not confirmed for " + st.agent + ", retrying");
        } else {
          console.error("[punk] messaging namespace resolution failed for " + st.agent + ", retrying");
        }
        await punkInboxCancellableSleep(st, Math.min(punkInboxEnvInt("PUNK_MESSAGING_BACKOFF_MS", 500) * Math.pow(2, attempt), 30000));
        attempt++;
      }
    } catch (err) {
      console.error("[punk] messaging registration loop failed:", err && err.message ? err.message : err);
    } finally {
      if (!st.registered) st.registering = false;
    }
  }

  // Teardown for gateway_stop: abort every pending retry. Idempotent.
  function punkTeardownAll() {
    for (const st of punkSessions.values()) {
      try {
        st.abortController.abort();
      } catch (err) {
        // abort() on an already-aborted controller is a no-op.
      }
    }
    punkSessions.clear();
  }

  // Catch-up: one leased fetch pass for the session in this prompt
  // build, rendered with the shared M5 envelope and prepended to the
  // same prompt the memory context rides. Delivery happens ONLY through
  // this return value - the one documented seam a plugin has for adding
  // content to an OpenClaw turn. Delivered ids are only MARKED here and
  // a bounded recurrent re-ack pass (paced by lease expiry, cancellable,
  // self-terminating once pending empties) confirms the ACK afterwards:
  // HOST HANDOFF under at-least-once semantics, NOT proof of
  // consumption - the return value is the only delivery signal and
  // OpenClaw exposes no completion callback, so the post-expiry ACK
  // records that the text was handed to the session's turn, not that the
  // host provably consumed it, and a dropped return value can cause a
  // redelivery. A failed or partial ACK ({acked:n} with n < len, e.g.
  // the lease expired) keeps the ids pending inside that recurrent pass
  // - a pending ACK is never thrown away. The auxiliary ACK and release
  // calls here are deliberately NOT awaited: before_prompt_build blocks
  // the start of a user turn, and every extra awaited request could add
  // another full punkFetch timeout to it. Returns "" whenever there is
  // nothing to inject (fail-open). Never rejects.
  async function punkInboxCatchUp(envelope) {
    if (!punkMessagingEnabled) return "";
    const sid = envelope.session_id;
    if (!sid) return "";
    const st = punkSessions.get(sid);
    if (!st || !st.registered) return "";
    try {
      const ns = st.ns;
      const pass = await punkInboxFetchPass(ns, st);
      if (!pass || !punkInboxStAlive(st)) return "";
      if (pass.reack.length) {
        punkInboxAckIds(ns, st, pass.reack).then((ok) => {
          if (!ok && punkInboxStAlive(st)) {
            // Failed or partial (expired-lease) ACK: the recurrent pass
            // reacquires after the lease window; the pending marks are kept.
            punkInboxScheduleReackRetry(ns, st);
          }
        });
      }
      if (pass.denied.length) punkInboxReleaseIds(ns, st, pass.denied);
      if (pass.deniedCount > 0) {
        console.error("[punk] held back " + pass.deniedCount + " message(s) from senders outside PUNK_MESSAGING_FROM");
      }
      if (!pass.deliver.length) return "";
      const rend = punkRenderInbox(ns, st.agent, pass.deliver);
      if (!rend.text) {
        punkInboxReleaseIds(ns, st, pass.deliver.map((m) => m.id));
        return "";
      }
      for (const m of rend.used) st.delivered.add(m.id);
      const usedIds = rend.used.map((m) => m.id);
      const leftover = [];
      for (const m of pass.deliver) {
        if (usedIds.indexOf(m.id) < 0) leftover.push(m.id);
      }
      if (leftover.length) punkInboxReleaseIds(ns, st, leftover);
      // Pending marks get the recurrent expiry pass (host handoff; see
      // the comment above).
      punkInboxScheduleReackRetry(ns, st);
      return rend.text;
    } catch (err) {
      console.error("[punk] inbox catch-up failed:", err && err.message ? err.message : err);
      return "";
    }
  }

`

// openClawMessagingBridgeJS renders the complete messaging-bridge splice:
// the shared envelope renderer (byte-identical to hookcli.RenderInbox,
// see inbox_renderjs.go) followed by the OpenClaw-specific machinery.
func openClawMessagingBridgeJS() string {
	return inboxBridgeJS() + openClawBridgeCoreJS
}
