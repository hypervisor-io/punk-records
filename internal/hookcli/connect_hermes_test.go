package hookcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// hermesConfig is the shape assertions below decode the written file back
// into. Only the keys punk manages are typed; everything else is checked
// against the raw text so an unexpected drop shows up as a missing string
// rather than a silently ignored field.
type hermesConfig struct {
	Hooks map[string][]struct {
		Command string `yaml:"command"`
		Timeout int    `yaml:"timeout"`
		Matcher string `yaml:"matcher"`
	} `yaml:"hooks"`
}

func writeHermesFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readHermesConfig(t *testing.T, path string) (hermesConfig, string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg hermesConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("written config is not valid YAML: %v\n%s", err, raw)
	}
	return cfg, string(raw)
}

// TestConnectHermesCreatesConfig covers the fresh-install path: no file at
// all, so every wired event gets exactly one punk entry and nothing else.
func TestConnectHermesCreatesConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	changed, err := ConnectHermes(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("creating a config must report changed")
	}

	cfg, raw := readHermesConfig(t, path)
	if len(cfg.Hooks) != len(hermesHookEvents) {
		t.Fatalf("wired %d events, want %d: %s", len(cfg.Hooks), len(hermesHookEvents), raw)
	}
	for _, ev := range hermesHookEvents {
		entries := cfg.Hooks[ev]
		if len(entries) != 1 {
			t.Fatalf("%s: %d entries, want 1: %s", ev, len(entries), raw)
		}
		if entries[0].Command != "/usr/local/bin/punk hook --from hermes --url http://localhost:9090" {
			t.Fatalf("%s: command = %q", ev, entries[0].Command)
		}
		if entries[0].Timeout != hermesHookTimeoutSec {
			t.Fatalf("%s: timeout = %d, want %d", ev, entries[0].Timeout, hermesHookTimeoutSec)
		}
		// matcher is tool-events-only and optional; punk wants every tool
		// call captured, so it must never be emitted.
		if entries[0].Matcher != "" {
			t.Fatalf("%s: matcher = %q, want none", ev, entries[0].Matcher)
		}
	}
	// The events deliberately left unwired must not appear at all.
	for _, ev := range []string{"on_session_end", "pre_tool_call"} {
		if _, ok := cfg.Hooks[ev]; ok {
			t.Fatalf("%s must not be wired: %s", ev, raw)
		}
	}
}

// TestConnectHermesPreservesForeignConfig is the guarantee that matters
// most for a live user file: unrelated top-level keys, unrelated hook
// events, other entries on a wired event, the reserved "outbound" webhook
// list, and comments all survive the merge.
func TestConnectHermesPreservesForeignConfig(t *testing.T) {
	path := writeHermesFile(t, `# top of file comment
model: hermes-4
hooks_auto_accept: false
hooks:
  # keep this comment
  pre_tool_call:
    - matcher: "terminal"
      command: "~/.hermes/agent-hooks/block-rm-rf.sh"
      timeout: 5
  post_tool_call:
    - command: "~/.hermes/agent-hooks/auto-format.sh"
  outbound:
    - name: ci-notify
      url: https://ci.example.com/hermes-events
      events: [on_session_end]
`)
	if _, err := ConnectHermes(path, "/opt/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}

	cfg, raw := readHermesConfig(t, path)
	if !strings.Contains(raw, "# top of file comment") || !strings.Contains(raw, "# keep this comment") {
		t.Fatalf("comments were dropped:\n%s", raw)
	}
	if !strings.Contains(raw, "model: hermes-4") || !strings.Contains(raw, "hooks_auto_accept: false") {
		t.Fatalf("unrelated top-level keys were dropped:\n%s", raw)
	}
	if !strings.Contains(raw, "ci-notify") || !strings.Contains(raw, "ci.example.com") {
		t.Fatalf("the outbound webhook list was damaged:\n%s", raw)
	}
	// punk never wires pre_tool_call, so the user's blocking hook there
	// must be left exactly as it was, alone.
	if len(cfg.Hooks["pre_tool_call"]) != 1 || cfg.Hooks["pre_tool_call"][0].Matcher != "terminal" {
		t.Fatalf("pre_tool_call was modified: %+v", cfg.Hooks["pre_tool_call"])
	}
	// post_tool_call IS wired, so the user's own entry must survive with
	// punk's appended after it.
	entries := cfg.Hooks["post_tool_call"]
	if len(entries) != 2 {
		t.Fatalf("post_tool_call: %d entries, want 2 (user's + punk's): %s", len(entries), raw)
	}
	if entries[0].Command != "~/.hermes/agent-hooks/auto-format.sh" {
		t.Fatalf("the user's entry was reordered or replaced: %+v", entries)
	}
	if !strings.Contains(entries[1].Command, "--from hermes") {
		t.Fatalf("punk's entry is not last: %+v", entries)
	}
}

// TestConnectHermesIdempotent pins both halves of the no-op contract: a
// rerun replaces punk's own entry rather than appending a second one, and a
// rerun over punk's own output reports changed=false without rewriting.
func TestConnectHermesIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := ConnectHermes(path, "/opt/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := ConnectHermes(path, "/opt/punk", "http://localhost:9090")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("an unchanged reconnect must report changed=false")
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("an unchanged reconnect rewrote the file:\n%s\n---\n%s", first, second)
	}

	// A changed URL replaces the stale entry in place; it never accumulates.
	if _, err := ConnectHermes(path, "/opt/punk", "http://memory.internal:9090"); err != nil {
		t.Fatal(err)
	}
	cfg, raw := readHermesConfig(t, path)
	for _, ev := range hermesHookEvents {
		if len(cfg.Hooks[ev]) != 1 {
			t.Fatalf("%s: %d entries after re-connect, want 1: %s", ev, len(cfg.Hooks[ev]), raw)
		}
		if !strings.Contains(cfg.Hooks[ev][0].Command, "memory.internal") {
			t.Fatalf("%s: stale command survived: %q", ev, cfg.Hooks[ev][0].Command)
		}
	}
}

// TestConnectHermesReplacesRelocatedBinaryEntry covers the fallback branch
// of isPunkManagedFromAgent: after the punk binary moves, the entry written
// under the OLD path must still be recognized as ours and replaced, not
// left behind pointing at a dead path forever.
func TestConnectHermesReplacesRelocatedBinaryEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := ConnectHermes(path, "/old/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectHermes(path, "/new/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	cfg, raw := readHermesConfig(t, path)
	for _, ev := range hermesHookEvents {
		if len(cfg.Hooks[ev]) != 1 {
			t.Fatalf("%s: %d entries, want the relocated one to replace the stale one: %s", ev, len(cfg.Hooks[ev]), raw)
		}
		if !strings.HasPrefix(cfg.Hooks[ev][0].Command, "/new/bin/punk ") {
			t.Fatalf("%s: command = %q", ev, cfg.Hooks[ev][0].Command)
		}
	}
}

// TestConnectHermesRefusesWrongShapes pins that punk names the offending
// key and leaves the file byte-for-byte untouched rather than replacing
// user config with its own shape.
func TestConnectHermesRefusesWrongShapes(t *testing.T) {
	cases := map[string]struct {
		body    string
		wantErr string
	}{
		"root is a list":          {"- one\n- two\n", "config root is not a mapping"},
		"hooks is a string":       {"hooks: disabled\n", "hooks is not a mapping"},
		"event is a mapping":      {"hooks:\n  pre_llm_call:\n    command: x\n", "hooks.pre_llm_call is not a sequence"},
		"event is a scalar":       {"hooks:\n  post_llm_call: off\n", "hooks.post_llm_call is not a sequence"},
		"hooks is an alias":       {"base: &anchor {}\nhooks: *anchor\n", "hooks is a YAML alias"},
		"multiple documents":      {"hooks: {}\n---\nhooks: {}\n", "multiple YAML documents"},
		"not valid yaml at all":   {"hooks:\n\t- bad tab indent\n", "parse config"},
		"event is an alias node":  {"seq: &a []\nhooks:\n  pre_llm_call: *a\n", "hooks.pre_llm_call is a YAML alias"},
		"root is a bare scalar":   {"just a string\n", "config root is not a mapping"},
		"hooks is a list not map": {"hooks:\n  - one\n", "hooks is not a mapping"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeHermesFile(t, tc.body)
			changed, err := ConnectHermes(path, "/opt/punk", "http://localhost:9090")
			if err == nil {
				t.Fatalf("want an error naming the offending key, got changed=%v", changed)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not name the problem (%q)", err, tc.wantErr)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(after) != tc.body {
				t.Fatalf("a refused merge modified the file:\n%s", after)
			}
		})
	}
}

// TestConnectHermesTreatsNullsAsAbsent pins the normalization every writer
// in this package shares: a key present with no value must behave exactly
// like a missing key rather than erroring or being skipped.
func TestConnectHermesTreatsNullsAsAbsent(t *testing.T) {
	for name, body := range map[string]string{
		"whole document is null": "null\n",
		"empty file":             "",
		"hooks is null":          "hooks:\n",
		"event is null":          "hooks:\n  pre_llm_call:\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeHermesFile(t, body)
			if _, err := ConnectHermes(path, "/opt/punk", "http://localhost:9090"); err != nil {
				t.Fatal(err)
			}
			cfg, raw := readHermesConfig(t, path)
			for _, ev := range hermesHookEvents {
				if len(cfg.Hooks[ev]) != 1 {
					t.Fatalf("%s: %d entries: %s", ev, len(cfg.Hooks[ev]), raw)
				}
			}
		})
	}
}

// TestConnectHermesKeepsHostileEntries pins that sequence elements which
// are not mappings at all (or mappings with no usable command) are left
// alone instead of panicking or being silently dropped.
func TestConnectHermesKeepsHostileEntries(t *testing.T) {
	path := writeHermesFile(t, `hooks:
  pre_llm_call:
    - "a bare string entry"
    - 42
    - [nested, list]
    - command: 17
`)
	if _, err := ConnectHermes(path, "/opt/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a bare string entry", "42", "nested", "command: 17"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("hostile entry %q was dropped:\n%s", want, raw)
		}
	}
	if !strings.Contains(string(raw), "--from hermes") {
		t.Fatalf("punk's own entry was not appended:\n%s", raw)
	}
}

// TestConnectHermesQuotesHostilePaths pins the escaping rule for values
// spliced into a structured document: a path or URL carrying YAML
// metacharacters must change the document's CONTENT, never its STRUCTURE,
// and must come back out of a parse byte-identical.
func TestConnectHermesQuotesHostilePaths(t *testing.T) {
	hostile := []struct {
		name     string
		punkPath string
		url      string
	}{
		{"colon space in path", "/opt/my: punk/punk", "http://localhost:9090"},
		{"quote in url", `http://x"y`, `http://x"y`},
		{"newline in url", "/opt/punk", "http://localhost:9090\nrogue_key: pwned"},
		{"anchor sigil", "&anchor/punk", "*alias"},
		{"comment sigil", "/opt/punk #not-a-comment", "http://localhost:9090 # nope"},
	}
	for _, tc := range hostile {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if _, err := ConnectHermes(path, tc.punkPath, tc.url); err != nil {
				t.Fatal(err)
			}
			cfg, raw := readHermesConfig(t, path)
			if _, injected := map[string]bool{}["rogue_key"]; injected {
				t.Fatal("unreachable")
			}
			if strings.Contains(raw, "\nrogue_key:") {
				t.Fatalf("a newline in a value escaped its scalar and created a new key:\n%s", raw)
			}
			for _, ev := range hermesHookEvents {
				entries := cfg.Hooks[ev]
				if len(entries) != 1 {
					t.Fatalf("%s: %d entries: %s", ev, len(entries), raw)
				}
				want := punkHermesHookCommand(tc.punkPath, tc.url)
				if entries[0].Command != want {
					t.Fatalf("%s: command round-tripped as %q, want %q", ev, entries[0].Command, want)
				}
			}
		})
	}
}

// TestConnectHermesPreservesFileMode pins that connecting never widens the
// permissions of a config a user locked down (Hermes' config.yaml can carry
// provider keys and webhook secrets alongside the hooks block).
func TestConnectHermesPreservesFileMode(t *testing.T) {
	path := writeHermesFile(t, "model: hermes-4\n")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectHermes(path, "/opt/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want the original 0600", info.Mode().Perm())
	}
}

func TestConnectHermesMCP(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("model: sonnet\nmcp_servers:\n  github:\n    command: gh-mcp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := MCPEntryOpts{ServerURL: "https://punk.example.com", APIKey: "prk_h"}
	if changed, err := ConnectHermesMCP(p, o, false); err != nil || !changed {
		t.Fatal(changed, err)
	}
	raw, _ := os.ReadFile(p)
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "sonnet" {
		t.Fatal("unrelated keys must survive")
	}
	servers := m["mcp_servers"].(map[string]any)
	punk := servers["punk"].(map[string]any)
	if punk["url"] != "https://punk.example.com/mcp?toolset=agent" || servers["github"] == nil {
		t.Fatalf("servers = %v", servers)
	}
	if punk["headers"].(map[string]any)["Authorization"] != "Bearer prk_h" {
		t.Fatalf("headers = %v", punk["headers"])
	}
	if changed, _ := ConnectHermesMCP(p, o, false); changed {
		t.Fatal("idempotent")
	}
}
