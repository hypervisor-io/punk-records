"""Unit tests for the generated Hermes plugin (internal/hookcli/hermes_plugin.go).

Driven by TestHermesPluginPythonUnitTests in hermes_plugin_test.go, which
generates a fresh plugin directory with WriteHermesPlugin and then runs:

    python3 testdata/hermes_plugin_test.py <plugin-dir>

so every assertion exercises the exact bytes the generator writes, not a
hand-copied paraphrase. Stdlib only, stdlib unittest, no Hermes install: the
plugin's one Hermes dependency (the live plugin manager) is replaced through
the module's own _plugin_manager seam, and the punk binary is a fake script.
"""

import importlib.util
import json
import os
import stat
import sys
import tempfile
import unittest
from pathlib import Path

# Set by __main__ from argv[1] before unittest.main() consumes argv.
_PLUGIN_DIR = None


def _load_plugin(plugin_dir):
    entry = Path(plugin_dir) / "__init__.py"
    spec = importlib.util.spec_from_file_location(
        "hermes_punk_plugin_under_test", entry
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class _FakeCtx:
    """Stands in for Hermes' PluginContext (ctx.register_hook)."""

    def __init__(self):
        self.hooks = {}

    def register_hook(self, name, callback):
        self.hooks.setdefault(name, []).append(callback)


class _FakeManager:
    """Stands in for hermes_cli.plugins.PluginManager (._hooks)."""

    def __init__(self, hooks=None):
        self._hooks = hooks if hooks is not None else {}


def _shell_hook_callback(event, command):
    """A callback shaped exactly like agent/shell_hooks.py's _make_callback.

    Its __name__ is shell_hook[<event>:<command>], which is what the plugin's
    dedup probe keys on.
    """
    def callback(**kwargs):
        return None

    callback.__name__ = "shell_hook[" + event + ":" + command + "]"
    return callback


def _write_fake_punk_bin(directory):
    """Write an executable script that answers like `punk hook ...`."""
    path = Path(directory) / "fake-punk"
    path.write_text(
        "#!/usr/bin/env {py}\n"
        "import json, os, sys\n"
        "argv = sys.argv[1:]\n"
        "data = sys.stdin.read()\n"
        "log = os.environ.get('PUNK_TEST_LOG')\n"
        "if log:\n"
        "    with open(log, 'a', encoding='utf-8') as fh:\n"
        "        fh.write(json.dumps({{'argv': argv, 'stdin': data}}) + '\\n')\n"
        "if argv[:1] == ['hook'] and 'inbox' in argv:\n"
        "    sys.stdout.write('{{\"context\": \"ROUTING\"}}\\n')\n"
        "elif argv[:1] == ['hook']:\n"
        "    sys.stdout.write('{{\"context\": \"MEMORY\"}}\\n')\n"
        .format(py=sys.executable),
        encoding="utf-8",
    )
    path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
    return path


class HermesPluginTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.plugin_dir = _PLUGIN_DIR
        cls.tmp = tempfile.mkdtemp(prefix="hermes-plugin-test-")
        cls.fake_bin = _write_fake_punk_bin(cls.tmp)
        cls.log = os.path.join(cls.tmp, "calls.jsonl")

    def setUp(self):
        self.plugin = _load_plugin(self.plugin_dir)
        self.ctx = _FakeCtx()
        self.plugin.register(self.ctx)
        os.environ["PUNK_HOOK_BIN"] = str(self.fake_bin)
        os.environ["PUNK_URL"] = "http://punk.test"
        os.environ["PUNK_TEST_LOG"] = self.log
        Path(self.log).write_text("", encoding="utf-8")
        # Default: no Hermes plugin manager at all (the Desktop-shaped case),
        # so nothing is suppressed unless a test installs a fake one.
        self.plugin._plugin_manager = lambda: None

    def tearDown(self):
        for key in ("PUNK_HOOK_BIN", "PUNK_URL", "PUNK_TEST_LOG"):
            os.environ.pop(key, None)

    def calls(self):
        text = Path(self.log).read_text(encoding="utf-8").strip()
        return [json.loads(line) for line in text.splitlines() if line]

    # -- registration ------------------------------------------------------

    def test_register_wires_the_four_shell_hook_events(self):
        self.assertEqual(
            sorted(self.ctx.hooks.keys()),
            ["on_session_start", "post_llm_call", "post_tool_call", "pre_llm_call"],
        )
        for name, callbacks in self.ctx.hooks.items():
            self.assertEqual(len(callbacks), 1, name)

    def test_module_declares_the_forwarded_events(self):
        self.assertEqual(
            tuple(self.plugin.PUNK_EVENTS),
            ("on_session_start", "pre_llm_call", "post_tool_call", "post_llm_call"),
        )

    # -- context injection -------------------------------------------------

    def test_pre_llm_call_merges_memory_and_inbox(self):
        self.plugin.PUNK_MESSAGING = True
        result = self.ctx.hooks["pre_llm_call"][0](
            session_id="s1", user_message="hello", is_first_turn=True
        )
        self.assertEqual(result, {"context": "MEMORY\n\nROUTING"})
        # Both the capture hook and the inbox hook ran.
        argvs = [c["argv"] for c in self.calls()]
        self.assertIn(["hook", "--from", "hermes", "--url", "http://punk.test"], argvs)
        self.assertIn(
            ["hook", "inbox", "--client", "hermes", "--mode", "context",
             "--url", "http://punk.test", "--messaging"],
            argvs,
        )

    def test_pre_llm_call_without_messaging_skips_inbox(self):
        self.plugin.PUNK_MESSAGING = False
        result = self.ctx.hooks["pre_llm_call"][0](
            session_id="s1", user_message="hello", is_first_turn=False
        )
        self.assertEqual(result, {"context": "MEMORY"})
        for call in self.calls():
            self.assertNotIn("inbox", call["argv"])

    def test_capture_events_are_observation_only(self):
        for event in ("on_session_start", "post_tool_call", "post_llm_call"):
            result = self.ctx.hooks[event][0](session_id="s1", tool_name="terminal")
            self.assertIsNone(result, event)
        # Each capture event POSTed exactly once.
        self.assertEqual(len(self.calls()), 3)

    # -- payload shape -----------------------------------------------------

    def test_payload_matches_the_shell_hook_serialization(self):
        self.ctx.hooks["post_tool_call"][0](
            tool_name="terminal",
            args={"command": "echo hi"},
            session_id="sess-1",
            result="ok",
            duration_ms=12,
        )
        call = self.calls()[0]
        payload = json.loads(call["stdin"])
        self.assertEqual(payload["hook_event_name"], "post_tool_call")
        self.assertEqual(payload["tool_name"], "terminal")
        self.assertEqual(payload["tool_input"], {"command": "echo hi"})
        self.assertEqual(payload["session_id"], "sess-1")
        self.assertIn("cwd", payload)
        # Everything that is not a top-level key lands under extra.
        self.assertEqual(payload["extra"]["result"], "ok")
        self.assertEqual(payload["extra"]["duration_ms"], 12)
        self.assertNotIn("args", payload["extra"])
        self.assertNotIn("tool_name", payload["extra"])

    def test_non_dict_args_are_dropped_from_tool_input(self):
        self.ctx.hooks["post_tool_call"][0](tool_name="terminal", args="not-a-dict")
        payload = json.loads(self.calls()[0]["stdin"])
        self.assertIsNone(payload["tool_input"])

    # -- dedup (no double injection) ---------------------------------------

    def test_pre_llm_call_stands_down_when_a_punk_shell_hook_is_registered(self):
        self.plugin.PUNK_MESSAGING = True
        self.plugin._plugin_manager = lambda: _FakeManager({
            "pre_llm_call": [
                _shell_hook_callback(
                    "pre_llm_call", "/usr/local/bin/punk hook --from hermes --url http://punk.test"
                ),
                _shell_hook_callback(
                    "pre_llm_call",
                    "/usr/local/bin/punk hook inbox --client hermes --mode context --url http://punk.test --messaging",
                ),
            ]
        })
        result = self.ctx.hooks["pre_llm_call"][0](session_id="s1", user_message="hi")
        self.assertIsNone(result)
        self.assertEqual(self.calls(), [], "must not POST or inject when the shell hook covers the event")

    def test_dedup_is_per_event(self):
        # A shell hook on on_session_start must not silence post_tool_call or
        # pre_llm_call: a partially-allowlisted CLI still gets punk's plugin
        # for the events the shell hooks did not register.
        self.plugin.PUNK_MESSAGING = True
        self.plugin._plugin_manager = lambda: _FakeManager({
            "on_session_start": [
                _shell_hook_callback(
                    "on_session_start", "/usr/local/bin/punk hook --from hermes --url http://punk.test"
                )
            ]
        })
        self.assertIsNone(self.ctx.hooks["on_session_start"][0](session_id="s1"))
        self.assertEqual(
            self.ctx.hooks["pre_llm_call"][0](session_id="s1", user_message="hi"),
            {"context": "MEMORY\n\nROUTING"},
        )
        argvs = [c["argv"] for c in self.calls()]
        self.assertEqual(len(argvs), 2, "only the two pre_llm_call calls should run")
        self.assertTrue(all("hook" in a for a in argvs))

    def test_foreign_shell_hook_does_not_suppress(self):
        self.plugin.PUNK_MESSAGING = False
        self.plugin._plugin_manager = lambda: _FakeManager({
            "pre_llm_call": [_shell_hook_callback("pre_llm_call", "/usr/bin/other-tool --not-punk")]
        })
        self.assertEqual(
            self.ctx.hooks["pre_llm_call"][0](session_id="s1", user_message="hi"),
            {"context": "MEMORY"},
        )

    def test_shell_hook_callback_without_marker_does_not_suppress(self):
        self.plugin.PUNK_MESSAGING = False
        # Right shape, wrong program: a relocated punk would still be ours
        # (the " hook --from hermes" marker is what identifies it), but an
        # unrelated shell_hook[...] callback is not.
        self.plugin._plugin_manager = lambda: _FakeManager({
            "pre_llm_call": [_shell_hook_callback("pre_llm_call", "/usr/bin/other-tool run")]
        })
        self.assertEqual(
            self.ctx.hooks["pre_llm_call"][0](session_id="s1", user_message="hi"),
            {"context": "MEMORY"},
        )

    def test_relocated_punk_binary_marker_still_suppresses(self):
        self.plugin.PUNK_MESSAGING = False
        self.plugin._plugin_manager = lambda: _FakeManager({
            "pre_llm_call": [
                _shell_hook_callback(
                    "pre_llm_call", "/opt/tools/punk-v2 hook --from hermes --url http://punk.test"
                )
            ]
        })
        self.assertIsNone(self.ctx.hooks["pre_llm_call"][0](session_id="s1", user_message="hi"))

    # -- fail-open ---------------------------------------------------------

    def test_missing_binary_is_fail_open(self):
        os.environ["PUNK_HOOK_BIN"] = "/nonexistent/punk-binary"
        self.plugin.PUNK_MESSAGING = True
        self.assertIsNone(self.ctx.hooks["pre_llm_call"][0](session_id="s1", user_message="hi"))
        for event in ("on_session_start", "post_tool_call", "post_llm_call"):
            self.assertIsNone(self.ctx.hooks[event][0](session_id="s1"))

    def test_manager_raising_is_fail_open(self):
        def _boom():
            raise RuntimeError("manager exploded")

        self.plugin.PUNK_MESSAGING = False
        self.plugin._plugin_manager = _boom
        self.assertEqual(
            self.ctx.hooks["pre_llm_call"][0](session_id="s1", user_message="hi"),
            {"context": "MEMORY"},
        )

    def test_garbage_stdout_is_treated_as_no_context(self):
        # Point at a binary that prints non-JSON; _context_of must swallow it.
        bogus = Path(self.tmp) / "bogus-punk"
        bogus.write_text(
            "#!/usr/bin/env {py}\nimport sys\nsys.stdout.write('not json at all')\n".format(
                py=sys.executable
            ),
            encoding="utf-8",
        )
        bogus.chmod(0o755)
        os.environ["PUNK_HOOK_BIN"] = str(bogus)
        self.plugin.PUNK_MESSAGING = True
        self.assertIsNone(self.ctx.hooks["pre_llm_call"][0](session_id="s1", user_message="hi"))


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("usage: hermes_plugin_test.py <plugin-dir>", file=sys.stderr)
        sys.exit(2)
    # argv[1] is the plugin dir; capture it, then hand the rest to unittest.
    _PLUGIN_DIR = sys.argv.pop(1)
    unittest.main(verbosity=2)
