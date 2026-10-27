package hookcli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// hermesPluginMarker is the required first line of both files
// WriteHermesPlugin writes, identifying them as punk-managed exactly the way
// openCodePluginMarker gates ConnectOpenCode: an existing file at either
// target path whose first line is NOT this marker is presumed hand-authored
// (a real user plugin, or one installed by a different tool) and is never
// overwritten.
//
// The marker is a Python/YAML comment so it is inert in both files: the
// plugin entry is Python (__init__.py) and the manifest is YAML
// (plugin.yaml), both parsed by Hermes' own loader.
const hermesPluginMarker = "# managed by punk connect hermes"

// HermesPluginID is the plugin name/directory punk registers itself under.
// Hermes' loader derives a flat directory plugin's registry key from
// plugin.yaml's name (falling back to the directory basename), so the
// directory, the manifest name and the plugins.enabled entry must all agree
// for the plugin to load - they all come from this one constant. See
// hermes-agent's hermes_cli/plugins.py (8301654): _parse_manifest (name/key
// derivation) and _discover_and_load_inner (the plugins.enabled opt-in gate).
const HermesPluginID = "punk-memory"

// hermesPluginEvents lists the Hermes plugin-hook events punk registers.
// These are the same four events punk's shell hooks wire (see
// hermesHookEvents in connect_hermes.go) - the plugin exists so the same
// capture and injection happen in the Desktop backend, where shell hooks
// are never registered. on_session_end and pre_tool_call stay unwired for
// the reasons hookcli's translateHermes documents (on_session_end would
// overwrite post_llm_call's Stop capture; pre_tool_call is a blocking hook
// punk never vetoes).
//
// The names are members of Hermes' VALID_HOOKS set
// (hermes_cli/plugins.py, 8301654), the same names a plugin passes to
// ctx.register_hook.
var hermesPluginEvents = []string{"on_session_start", "pre_llm_call", "post_tool_call", "post_llm_call"}

// hermesPluginTemplate is the full Python plugin source WriteHermesPlugin
// writes. Substitution points, in order: the managed marker, the PUNK_URL
// fallback default, the punk binary path, and the messaging flag - the last
// three rendered through pyStringLiteral / a Go bool literal so an
// operator-supplied serverURL or binary path containing a quote, backslash
// or newline can never break out of the Python string literal it is spliced
// into (house rule .claude/rules/ai.md: "ALWAYS unconditionally escape
// strings embedded in structured formats").
//
// Design notes, since this is a Python file most readers will only ever see
// as generated output:
//
//   - Plugin contract, verified against hermes-agent's source (8301654):
//     a user plugin is a directory under $HERMES_HOME/plugins/<name>/
//     (hermes_cli/plugins.py _discover_and_load_inner) holding a
//     plugin.yaml manifest and an __init__.py whose register(ctx) the loader
//     calls (_load_plugin). ctx.register_hook(name, cb) appends the callback
//     to the shared dispatcher (PluginContext.register_hook); the agent core
//     then calls it as cb(**kwargs) via invoke_hook
//     (PluginManager.invoke_hook). A pre_llm_call callback that returns
//     {"context": <text>} has that text appended to the user message, joined
//     with a blank line when several callbacks return one
//     (agent/turn_context.py).
//   - Why a plugin at all: Hermes discovers plugins in EVERY mode - the
//     discover_plugins() call is a module-import side effect of model_tools.py
//     and an explicit call in tui_gateway/server.py (which the Desktop
//     "hermes serve" backend drives). Shell hooks, by contrast, are
//     registered only at CLI startup (hermes_cli/main.py) and gateway startup
//     (gateway/run.py); the Desktop backend never registers them. The plugin
//     therefore covers the one surface the shell hooks miss.
//   - Dedup: a plugin and a shell hook fire through the SAME dispatcher, so
//     in a CLI/Gateway process both would run and the turn would inject the
//     routing block twice and POST every capture twice. The handlers
//     therefore stand down for an event when a punk shell hook is registered
//     for that event IN THIS PROCESS. Detection reads the live dispatcher
//     (get_plugin_manager()._hooks): shell hooks register their callbacks
//     there under names of the form shell_hook[<event>:<cmd>]
//     (agent/shell_hooks.py, _make_callback sets __name__), so a callback
//     named shell_hook[<event>:...] whose name carries a punk invocation
//     marker means the shell hook already covers this event. In the Desktop
//     backend no shell hook is ever registered, so nothing is suppressed. The
//     per-event check (not a single global one) means a partially-allowlisted
//     CLI still gets the plugin for the events the shell hooks did NOT
//     register.
//   - The plugin shells out to the SAME punk binary and the SAME argv the
//     shell hooks invoke - <bin> hook --from hermes --url <url> for capture
//     and <bin> hook inbox --client hermes --mode context --url <url>
//     --messaging for pre_llm_call catch-up (connect_hermes.go's
//     punkHermesHookCommand / punkInboxHookCommand). Nothing about the wire
//     format, the Hermes->envelope translation or the endpoints is
//     reimplemented here; the binary owns all of it, so the plugin cannot
//     drift from the shell path.
//   - The stdin payload is rebuilt to be byte-identical in shape to what
//     Hermes hands a shell hook: agent/shell_hooks.py's _serialize_payload
//     (hook_event_name, tool_name, tool_input, session_id, cwd, extra with
//     the remaining kwargs). That is what the punk binary's
//     Normalize("hermes", ...) expects.
//   - Every subprocess call is bounded at PUNK_TIMEOUT_SECONDS (2s, matching
//     hookcli's own httpClient and the other generated plugins' budgets) and
//     every failure - timeout, missing binary, non-zero exit, malformed
//     stdout - is swallowed to "", so a dead memory server can never break a
//     Hermes turn. Each handler is additionally wrapped in try/except, so a
//     bug here can never throw into Hermes (fail-open, mirroring hookcli's
//     hook-forwarder contract).
//   - PUNK_URL and PUNK_HOOK_BIN are read from the environment at call time
//     so an operator can retarget a relocated server or binary without
//     regenerating the plugin; the baked values are the fallbacks.
const hermesPluginTemplate = `%s
"""punk-records agent memory for Hermes Agent.

Generated by punk connect hermes - do not edit by hand; re-run the command to
regenerate.

Hermes loads this plugin in EVERY mode (CLI, Gateway and the Desktop backend):
plugin discovery runs as a side effect of importing model_tools and again in
tui_gateway/server.py, which the Desktop "hermes serve" backend drives, and a
plugin's register(ctx) hooks are dispatched by the same invoke_hook the shell
hooks use. That is why this plugin exists: Hermes SHELL hooks (the hooks: block
in config.yaml) are registered only at CLI and Gateway startup and never by the
Desktop backend, so punk's shell hooks capture nothing there.

Dedup: where a punk shell hook IS registered for an event in this process (CLI,
Gateway) the plugin stands down for that event - the shell hook already forwards
it, and running both would inject the same context twice and POST the same
capture twice. Detection reads the live dispatcher; see _shell_hook_registered.
In the Desktop backend no shell hook is ever registered, so nothing is
suppressed.

The plugin shells out to the same punk binary and the same argv the shell hooks
invoke, so the payload, translation and endpoints are identical to the shell
path. Every call is bounded and every failure is swallowed: a dead memory server
must never break a Hermes turn (fail-open).
"""

from __future__ import annotations

import json
import os
import subprocess

PUNK_DEFAULT_URL = %s
PUNK_DEFAULT_BIN = %s
PUNK_MESSAGING = %s

PUNK_TIMEOUT_SECONDS = 2.0

# The events punk forwards, matching hookcli.hermesHookEvents: the shell hooks'
# own four events. on_session_end and pre_tool_call are deliberately absent (see
# the module docstring and hookcli's translateHermes).
PUNK_EVENTS = ("on_session_start", "pre_llm_call", "post_tool_call", "post_llm_call")

# Keys hoisted to the top level of the shell-hook payload; everything else is
# passed through under "extra". Mirrors agent/shell_hooks.py's
# _TOP_LEVEL_PAYLOAD_KEYS / _serialize_payload.
_PAYLOAD_TOP_LEVEL_KEYS = ("tool_name", "args", "session_id", "parent_session_id")


def _punk_url():
    url = os.environ.get("PUNK_URL") or PUNK_DEFAULT_URL or ""
    return url.rstrip("/")


def _punk_bin():
    return os.environ.get("PUNK_HOOK_BIN") or PUNK_DEFAULT_BIN


def _plugin_manager():
    """Return Hermes' live plugin manager, or None outside Hermes.

    Split into its own function so tests can substitute a fake manager without
    a real Hermes install on the path.
    """
    try:
        from hermes_cli.plugins import get_plugin_manager
        return get_plugin_manager()
    except Exception:
        return None


def _shell_hook_registered(event):
    """True when a punk shell hook is registered for event in this process.

    Shell hooks register their callbacks on the shared plugin manager under
    names of the form shell_hook[<event>:<command>] (agent/shell_hooks.py
    _make_callback), so a callback whose name starts with shell_hook[<event>:
    and carries a punk invocation marker means the shell hook already forwards
    this event here.

    Fully defensive: any failure resolving or reading the manager means "not
    registered", so the plugin keeps injecting rather than silently going dark
    (the Desktop case, which is the whole reason the plugin exists). The only
    cost of a false negative is a possible double injection in a process whose
    plugin manager is unreadable - never a lost capture.
    """
    try:
        manager = _plugin_manager()
        if manager is None:
            return False
        callbacks = manager._hooks.get(event, [])
    except Exception:
        return False
    prefix = "shell_hook[" + event + ":"
    for callback in callbacks:
        name = getattr(callback, "__name__", "")
        if not isinstance(name, str) or not name.startswith(prefix):
            continue
        if " hook --from hermes" in name or " hook inbox --client hermes" in name:
            return True
    return False


def _payload(event, kwargs):
    """Render the shell-hook stdin payload.

    Byte-identical in shape to agent/shell_hooks.py's _serialize_payload:
    hook_event_name, tool_name, tool_input, session_id, cwd and an "extra"
    object holding every remaining kwarg.
    """
    extra = {k: v for k, v in kwargs.items() if k not in _PAYLOAD_TOP_LEVEL_KEYS}
    try:
        cwd = os.getcwd()
    except OSError:
        cwd = ""
    args = kwargs.get("args")
    return {
        "hook_event_name": event,
        "tool_name": kwargs.get("tool_name"),
        "tool_input": args if isinstance(args, dict) else None,
        "session_id": kwargs.get("session_id") or kwargs.get("parent_session_id") or "",
        "cwd": cwd,
        "extra": extra,
    }


def _serialize(payload):
    return json.dumps(payload, ensure_ascii=False, default=str)


def _run(argv, payload):
    """Run punk with payload on stdin; return stdout, or "" on ANY failure."""
    try:
        proc = subprocess.run(
            argv,
            input=payload,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
            timeout=PUNK_TIMEOUT_SECONDS,
        )
    except Exception:
        return ""
    return proc.stdout or ""


def _capture(event, kwargs):
    """Forward one event exactly as the shell hook does."""
    argv = [_punk_bin(), "hook", "--from", "hermes", "--url", _punk_url()]
    return _run(argv, _serialize(_payload(event, kwargs)))


def _inbox(kwargs):
    """Run punk's pre_llm_call inbox catch-up exactly as the shell hook does."""
    argv = [
        _punk_bin(), "hook", "inbox", "--client", "hermes", "--mode", "context",
        "--url", _punk_url(), "--messaging",
    ]
    return _run(argv, _serialize(_payload("pre_llm_call", kwargs)))


def _context_of(stdout):
    """Extract a {"context": <text>} reply, or "" when there is none."""
    text = (stdout or "").strip()
    if not text:
        return ""
    try:
        data = json.loads(text)
    except Exception:
        return ""
    if isinstance(data, dict):
        ctx = data.get("context")
        if isinstance(ctx, str) and ctx.strip():
            return ctx
    return ""


def _observe(event):
    """Build a capture-only handler for an observation event."""
    def handler(**kwargs):
        try:
            if _shell_hook_registered(event):
                return None
            _capture(event, kwargs)
        except Exception:
            pass
        return None

    handler.__name__ = "punk_hermes_" + event
    return handler


def _pre_llm_call(**kwargs):
    """Inject punk's context before every model call.

    Runs the capture hook (recalled memory) and, when messaging is on, the inbox
    hook (routing identity plus unread catch-up), then joins their context
    replies the way Hermes joins every pre_llm_call result (with a blank line
    between pieces - agent/turn_context.py).
    """
    try:
        if _shell_hook_registered("pre_llm_call"):
            return None
        parts = []
        memory = _context_of(_capture("pre_llm_call", kwargs))
        if memory:
            parts.append(memory)
        if PUNK_MESSAGING:
            catch_up = _context_of(_inbox(kwargs))
            if catch_up:
                parts.append(catch_up)
        if not parts:
            return None
        return {"context": "\n\n".join(parts)}
    except Exception:
        return None


def register(ctx):
    """Register punk's hooks. Called by Hermes during plugin discovery."""
    ctx.register_hook("on_session_start", _observe("on_session_start"))
    ctx.register_hook("pre_llm_call", _pre_llm_call)
    ctx.register_hook("post_tool_call", _observe("post_tool_call"))
    ctx.register_hook("post_llm_call", _observe("post_llm_call"))
`

// hermesPluginSource renders the plugin entry file for serverURL, punkPath
// and messaging. Verb order in the template: managed marker, PUNK_URL
// fallback, punk binary path, messaging flag.
func hermesPluginSource(serverURL, punkPath string, messaging bool) string {
	return fmt.Sprintf(hermesPluginTemplate,
		hermesPluginMarker,
		pyStringLiteral(serverURL),
		pyStringLiteral(punkPath),
		pyBoolLiteral(messaging))
}

// hermesPluginManifest renders the plugin.yaml manifest Hermes requires next
// to the entry file. Fields follow hermes-agent's PluginManifest /
// _parse_manifest (8301654): name is the registry key (and must match the
// directory and the plugins.enabled entry), kind "standalone" routes it
// through the ordinary opt-in loader, and provides_hooks is the documented
// declaration of which VALID_HOOKS the plugin registers.
func hermesPluginManifest() string {
	var b strings.Builder
	b.WriteString(hermesPluginMarker + "\n")
	b.WriteString("name: " + HermesPluginID + "\n")
	b.WriteString("version: 1.0.0\n")
	b.WriteString("description: punk-records agent memory for Hermes Agent (Desktop, CLI and Gateway)\n")
	b.WriteString("author: punk-records\n")
	b.WriteString("kind: standalone\n")
	b.WriteString("provides_hooks:\n")
	for _, ev := range hermesPluginEvents {
		b.WriteString("  - " + ev + "\n")
	}
	return b.String()
}

// WriteHermesPlugin writes punk's Hermes plugin into pluginDir: the entry
// file (__init__.py) and the plugin.yaml manifest Hermes' loader requires
// (hermes_cli/plugins.py: "Each directory plugin must contain a plugin.yaml
// manifest and an __init__.py with a register(ctx) function"). changed
// reports whether anything on disk actually differs afterwards, so a no-op
// reconnect can say so instead of touching mtimes.
//
// Both files are refused rather than overwritten when they exist and are not
// punk's: each must carry hermesPluginMarker as its first line. A file
// without it is a hand-authored plugin (or another tool's), and silently
// replacing it would destroy user work. The write goes through
// writePreservingSymlinkAndMode (symlink resolved, existing mode preserved,
// atomic temp file + rename), shared with every other file this package
// writes.
func WriteHermesPlugin(pluginDir, serverURL, punkPath string, messaging bool) (changed bool, err error) {
	entryPath := filepath.Join(pluginDir, "__init__.py")
	manifestPath := filepath.Join(pluginDir, "plugin.yaml")

	existingEntry, err := readIfExists(entryPath)
	if err != nil {
		return false, err
	}
	if existingEntry != nil && !hasHermesPluginMarker(string(existingEntry)) {
		return false, fmt.Errorf("%s exists and is not managed by punk (missing %q marker on its first line); refusing to overwrite it",
			entryPath, hermesPluginMarker)
	}

	existingManifest, err := readIfExists(manifestPath)
	if err != nil {
		return false, err
	}
	if existingManifest != nil && !hasHermesPluginMarker(string(existingManifest)) {
		return false, fmt.Errorf("%s exists and is not managed by punk (missing %q marker on its first line); refusing to overwrite it",
			manifestPath, hermesPluginMarker)
	}

	entry := []byte(hermesPluginSource(serverURL, punkPath, messaging))
	manifest := []byte(hermesPluginManifest())

	if existingEntry == nil || string(existingEntry) != string(entry) {
		if err := writePreservingSymlinkAndMode(entryPath, entry, 0o644); err != nil {
			return false, err
		}
		changed = true
	}
	if existingManifest == nil || string(existingManifest) != string(manifest) {
		if err := writePreservingSymlinkAndMode(manifestPath, manifest, 0o644); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// hasHermesPluginMarker reports whether src's first line is the managed
// marker (allowing leading blank lines, which a YAML manifest may
// legitimately carry before a comment).
func hasHermesPluginMarker(src string) bool {
	return strings.HasPrefix(src, hermesPluginMarker) ||
		strings.HasPrefix(strings.TrimLeft(src, "\n\r"), hermesPluginMarker)
}

// pyStringLiteral renders s as a double-quoted Python string literal safe to
// splice into generated source. Go's encoding/json escapes exactly the
// characters a Python string literal needs (quotes, backslashes, control
// characters, and non-ASCII via \uXXXX, which Python accepts), so
// json.Marshal of a string is valid Python too - with the single JSON/Python
// caveat that a bare U+2028/U+2029 is legal in both but is re-escaped by hand
// anyway, matching jsStringLiteral. serverURL and punkPath are
// operator-supplied (the --url flag / os.Executable), not attacker data, but
// per house rule they are escaped unconditionally rather than trusted to be
// quote/backslash/newline-free.
func pyStringLiteral(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// json.Marshal of a string only fails for invalid UTF-8, which a
		// CLI-flag/exec-path string is not; fall back to an empty literal
		// rather than emitting malformed Python.
		return `""`
	}
	out := string(b)
	out = strings.ReplaceAll(out, "\u2028", `\u2028`)
	out = strings.ReplaceAll(out, "\u2029", `\u2029`)
	return out
}

// pyBoolLiteral renders b as the Python literal True or False.
func pyBoolLiteral(b bool) string {
	if b {
		return "True"
	}
	return "False"
}
