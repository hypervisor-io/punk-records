package hookcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/pretty"
)

func readOpenCodeJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(pretty.Spec(raw), &m); err != nil {
		t.Fatalf("%s no longer parses: %v\n%s", path, err, raw)
	}
	return m
}

func TestEnsureOpenCodeLocationIdleTimeout(t *testing.T) {
	idle := func(m map[string]any) any {
		exp, _ := m["experimental"].(map[string]any)
		return exp[openCodeIdleTimeoutKey]
	}
	cases := []struct {
		name, file, body string
		changed          bool
		want             any // value after the call
		keep             string
	}{
		{"missing file", "opencode.json", "", true, false, ""},
		{"no experimental", "opencode.json", `{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {"punk": {"type": "remote", "url": "http://x/mcp"}}
}
`, true, false, `"mcp"`},
		{"experimental without the key", "opencode.json", `{"experimental": {"other": 1}}`, true, false, `"other"`},
		{"user duration kept", "opencode.json", `{"experimental": {"location_idle_timeout": "30 minutes"}}`, false, "30 minutes", ""},
		{"user true kept", "opencode.json", `{"experimental": {"location_idle_timeout": true}}`, false, true, ""},
		{"non-object experimental left alone", "opencode.json", `{"experimental": "on"}`, false, nil, ""},
		{"null experimental replaced", "opencode.json", `{"experimental": null, "a": 1}`, true, false, `"a"`},
		{"jsonc comments survive", "opencode.jsonc", `{
  // my plugins
  "plugin": ["opencode-mobile@latest"],
}
`, true, false, "// my plugins"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.file)
			if tc.body != "" {
				if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			changed, err := EnsureOpenCodeLocationIdleTimeout(path)
			if err != nil || changed != tc.changed {
				t.Fatalf("changed = %v err = %v, want %v", changed, err, tc.changed)
			}
			m := readOpenCodeJSON(t, path)
			if got := idle(m); got != tc.want {
				t.Fatalf("location_idle_timeout = %#v, want %#v", got, tc.want)
			}
			raw, _ := os.ReadFile(path)
			if tc.keep != "" && !strings.Contains(string(raw), tc.keep) {
				t.Fatalf("lost %q:\n%s", tc.keep, raw)
			}
			if tc.body != "" {
				if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
					t.Fatalf("mode = %v, want the file's own 0600 kept", fi.Mode().Perm())
				}
			}
			// Idempotent: a second run changes nothing.
			if again, err := EnsureOpenCodeLocationIdleTimeout(path); err != nil || again {
				t.Fatalf("second run changed = %v err = %v", again, err)
			}
		})
	}
}
