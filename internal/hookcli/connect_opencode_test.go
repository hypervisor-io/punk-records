package hookcli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openCodeGoldenContent pins the EXACT byte-for-byte JS a fresh plugin file
// gets for serverURL "http://localhost:9090": the managed marker as the
// first line, the module export shape, and all four hooks (event,
// tool.execute.after, chat.message, experimental.chat.system.transform). A
// mere strings.Contains check would miss a wrong hook name, a dropped
// field, or broken JS syntax around the %s substitution point.
const openCodeGoldenContent = `// managed by punk connect opencode
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
    return (fromEnv || "http://localhost:9090").replace(/\/+$/, "")
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

// TestConnectOpenCodeGoldenContent pins the exact bytes ConnectOpenCode
// writes for a fresh plugin file.
func TestConnectOpenCodeGoldenContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	changed, err := ConnectOpenCode(path, "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != openCodeGoldenContent {
		t.Fatalf("golden mismatch:\n got:  %q\nwant: %q", got, openCodeGoldenContent)
	}
	if !strings.HasPrefix(string(got), openCodePluginMarker) {
		t.Fatalf("marker must be the first line, got: %q", got[:min(len(got), 80)])
	}
}

// TestConnectOpenCodeIdempotent verifies a re-run with identical inputs
// reports changed=false and does not rewrite the file.
func TestConnectOpenCodeIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	if changed, err := ConnectOpenCode(path, "http://localhost:9090"); err != nil || !changed {
		t.Fatal(changed, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectOpenCode(path, "http://localhost:9090")
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

// TestConnectOpenCodeRefusesUnmanagedExisting verifies an existing plugin
// file without the managed marker on its first line is left completely
// untouched and refused with an error naming the path - overwriting a
// user's own hand-authored OpenCode plugin would destroy their content
// silently otherwise.
func TestConnectOpenCodeRefusesUnmanagedExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	original := []byte("// my own hand-authored opencode plugin\nexport const Mine = async () => ({})\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ConnectOpenCode(path, "http://localhost:9090")
	if err == nil {
		t.Fatal("expected error for unmanaged existing plugin file")
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

// TestConnectOpenCodeUpdatesStaleManagedContent verifies a plugin file
// that DOES carry the managed marker (e.g. from a prior punk version, or a
// different serverURL baked into the PUNK_URL fallback) is updated in
// place rather than refused.
func TestConnectOpenCodeUpdatesStaleManagedContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	stale := openCodePluginMarker + "\nexport const Old = async () => { /* stale, mentions http://old:1 */ return {} }\n"
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectOpenCode(path, "http://localhost:9090")
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

// TestConnectOpenCodeSymlinkedPluginStaysSymlink verifies that when the
// plugin path is a symlink (e.g. into a dotfiles repo), connecting updates
// the content the symlink points at in place rather than replacing the
// symlink with a plain file.
func TestConnectOpenCodeSymlinkedPluginStaysSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-punk-memory.js")
	if err := os.WriteFile(real, []byte(openCodePluginMarker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "punk-memory.js")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectOpenCode(link, "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("plugin symlink was replaced by a regular file")
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
	if !strings.Contains(string(raw), "PunkMemoryPlugin") {
		t.Fatalf("symlink target missing plugin content: %s", raw)
	}
}

// TestConnectOpenCodePreservesExistingFileMode verifies connecting against
// an existing plugin file does not widen its permissions.
func TestConnectOpenCodePreservesExistingFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	if _, err := ConnectOpenCode(path, "http://a:1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectOpenCode(path, "http://b:2"); err != nil {
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

// TestConnectOpenCodeCreatesParentDirs verifies ConnectOpenCode creates
// the plugin's parent directory tree (e.g. ~/.config/opencode/plugins/ or
// ./.opencode/plugins/, neither of which typically pre-exists) rather than
// requiring the caller to mkdir first.
func TestConnectOpenCodeCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode", "plugins", "punk-memory.js")
	changed, err := ConnectOpenCode(path, "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected plugin file to exist after parent dir creation: %v", err)
	}
}

// TestConnectOpenCodeEscapesHostileServerURL verifies a serverURL
// containing characters that are meaningful inside a JS string literal
// (a double quote, a backslash) is escaped rather than breaking out of the
// generated string literal - per .claude/rules/ai.md's "always
// unconditionally escape" rule. If node or bun is on PATH, the emitted
// file is additionally syntax-checked so a broken escape would fail loudly
// instead of merely "looking escaped" to a substring check.
func TestConnectOpenCodeEscapesHostileServerURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	hostile := `http://evil","x":"pwned`
	if _, err := ConnectOpenCode(path, hostile); err != nil {
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
	// The fallback expression must still open with exactly one unescaped
	// quote before the hostile payload - i.e. the payload landed INSIDE
	// the string literal, not appended after it as extra JS tokens.
	if !strings.Contains(s, `(fromEnv || "http://evil\"`) {
		t.Fatalf("hostile URL did not stay inside the intended string literal: %s", s)
	}
	runJSSyntaxCheck(t, path, got)
}

// TestOpenCodePluginPassesJSSyntaxCheck is a lightweight sanity check that
// the emitted plugin is syntactically valid JavaScript, using whichever of
// node/bun is available in the test environment; skipped when neither is
// on PATH.
func TestOpenCodePluginPassesJSSyntaxCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	if _, err := ConnectOpenCode(path, "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	runJSSyntaxCheck(t, path, got)
}

// runJSSyntaxCheck syntax-checks content with node (preferred) or bun,
// t.Skip-ing when neither is on PATH. node's CommonJS-by-default loader
// rejects top-level "export" outside a module context even under
// --check, so content is copied to a sibling .mjs file (unambiguously ESM
// by extension) rather than checked at its original .js path; bun infers
// ESM from syntax regardless of extension, so it checks the original path
// directly via "bun build".
func runJSSyntaxCheck(t *testing.T, path string, content []byte) {
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
	t.Skip("neither node nor bun on PATH; skipping JS syntax sanity check")
}

// TestConnectOpenCodeEscapesLineAndParagraphSeparators is finding #6's
// pinning test: jsStringLiteral's U+2028/U+2029 re-escape branch
// (opencode_plugin.go) had no test exercising an ACTUAL literal separator
// character reaching it - the hostile-quote test above never touches that
// code path. A serverURL containing a raw U+2028/U+2029 must come out of
// jsStringLiteral escaped (as the six-character  /  sequence),
// never as the raw three-byte UTF-8 separator, since older bundlers/linters
// still choke on an unescaped one inside a JS string literal.
func TestConnectOpenCodeEscapesLineAndParagraphSeparators(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	hostile := "http://evil.example/ mid end"
	if _, err := ConnectOpenCode(path, hostile); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.ContainsRune(s, ' ') || strings.ContainsRune(s, ' ') {
		t.Fatalf("raw U+2028/U+2029 must never reach the emitted JS unescaped: %q", s)
	}
	if !strings.Contains(s, "\\u2028mid\\u2029end") {
		t.Fatalf("expected the escaped \\u2028/\\u2029 sequences in the fallback string literal, got: %s", s)
	}
	runJSSyntaxCheck(t, path, got)
}

// TestOpenCodePunkFetchAbortsStalledResponseBody is finding #1's pinning
// test: before the fix, punkFetch cleared its AbortController's timer in
// "finally" right after fetch() itself resolved (i.e. once response
// headers arrive), leaving the subsequent `await res.json()` body read
// completely unbounded. A server that sends 200 headers and then never
// writes/closes the body would hang the awaited
// "experimental.chat.system.transform" hook forever.
//
// This drives the ACTUAL rendered plugin (not a hand-copied reimplementation
// of punkFetch) through a small node harness: a local httptest.Server
// answers /v1/agent/context with headers-then-silence (flushed 200, then
// blocks on the request context so it releases the connection the moment
// the client aborts), and the harness invokes the plugin's
// "experimental.chat.system.transform" hook and times how long the
// returned promise takes to settle. t.Skip when node is not on PATH - bun's
// AbortController/fetch timing behavior isn't pinned here, only node's.
func TestOpenCodePunkFetchAbortsStalledResponseBody(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping punkFetch abort runtime test")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent/context", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Stall the body indefinitely - never write another byte - until
		// the client disconnects (the plugin's AbortController fires) or
		// the test server shuts down, whichever happens first. Selecting
		// on the request's own context (canceled the moment the client
		// aborts/closes the connection) is what lets httptest.Server.Close
		// return promptly instead of blocking on this handler forever.
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.js")
	if _, err := ConnectOpenCode(path, srv.URL); err != nil {
		t.Fatal(err)
	}
	pluginSrc, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The driver is appended to the actual rendered plugin source in one
	// .mjs file (unambiguously ESM by extension, see runJSSyntaxCheck's own
	// doc comment for why that matters to node), so this exercises the
	// exact bytes ConnectOpenCode writes, not a paraphrase of them.
	driver := `
async function main() {
  const start = Date.now()
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-abort-test-project" })
  const output = { system: [] }
  await hooks["experimental.chat.system.transform"]({ sessionID: "abort-test-session" }, output)
  const elapsedMs = Date.now() - start
  console.log("elapsed_ms=" + elapsedMs)
  if (elapsedMs > 3500) {
    console.error("FAIL: transform hook took " + elapsedMs + "ms to settle, expected the 2s abort to fire well under 4000ms")
    process.exit(1)
  }
  process.exit(0)
}
main().catch((err) => {
  console.error("FAIL: transform hook rejected instead of resolving:", err)
  process.exit(1)
})
`
	harnessPath := path + ".abort-harness.mjs"
	if err := os.WriteFile(harnessPath, append(pluginSrc, []byte(driver)...), 0o644); err != nil {
		t.Fatal(err)
	}

	// A hard outer deadline well above the expected ~2s abort: if the fix
	// regresses back to an unbounded body read, the harness would hang
	// past this deadline and the killed process fails the test with a
	// clear "did not complete" signal rather than blocking `go test`
	// forever.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	start := time.Now()
	out, err := exec.CommandContext(ctx, nodePath, harnessPath).CombinedOutput()
	elapsed := time.Since(start)

	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("node harness did not complete within the 8s hard deadline (abort not honored - unbounded body read): elapsed=%s\n%s", elapsed, out)
	}
	if err != nil {
		t.Fatalf("node harness failed: %v (elapsed=%s)\n%s", err, elapsed, out)
	}
	if elapsed >= 4*time.Second {
		t.Fatalf("transform hook took %s wall-clock to settle from the Go side too, expected well under 4s (abort fired around 2s): %s", elapsed, out)
	}
}
