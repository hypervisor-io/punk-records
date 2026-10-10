package hookcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codeops:trace repo=punk-records work_item=punk-connect-remote-url-review-20261009 spec=docs/client-credentials.md plan=phase-0/task-0-A test=TestConnectOpenCodeMCPPreservesJSONC evidence=docs/superpowers/reports/2026-10-09-client-credentials/builder-a.md
func TestConnectOpenCodeMCPPreservesJSONC(t *testing.T) {
	p := filepath.Join(t.TempDir(), "opencode.jsonc")
	original := `{
  // keep this comment
  "theme": "dark",
  "mcp": {
    "foreign": {"type":"local", "command":["foreign"]},
  },
}
`
	if err := os.WriteFile(p, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectOpenCodeMCP(p, MCPEntryOpts{ServerURL: "https://punk.example.test/base", APIKeyEnv: "PUNK_TEST_KEY"}, false)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{"// keep this comment", `"theme": "dark"`, `"foreign"`, `"punk"`, "Bearer {env:PUNK_TEST_KEY}"} {
		if !strings.Contains(got, want) {
			t.Fatalf("JSONC merge lost %q:\n%s", want, got)
		}
	}
	if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}
	if _, _, _, err := loadOpenCodeSettings(p); err != nil {
		t.Fatalf("merged JSONC is not parseable: %v\n%s", err, got)
	}
	if changed, err := ConnectOpenCodeMCP(p, MCPEntryOpts{ServerURL: "https://punk.example.test/base", APIKeyEnv: "PUNK_TEST_KEY"}, false); err != nil || changed {
		t.Fatalf("JSONC reconnect must be byte-identical: changed=%v err=%v", changed, err)
	}
}

func TestConnectOpenCodeMCPJSONCPreservesSymlinkAndLiteralTokenPrivacy(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "shared.jsonc")
	link := filepath.Join(dir, "opencode.jsonc")
	if err := os.WriteFile(target, []byte("{\n  // shared dotfiles\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectOpenCodeMCP(link, MCPEntryOpts{ServerURL: "https://punk.example.test", APIKey: "synthetic-literal-key"}, false); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config symlink replaced: mode=%v err=%v", info.Mode(), err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("literal-token target mode=%v err=%v", info.Mode().Perm(), err)
	}
	raw, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(raw), "// shared dotfiles") {
		t.Fatalf("symlink target/comment not preserved: %v\n%s", err, raw)
	}
}

func TestConnectOpenCodeMCPRefusesBrokenConfigSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "opencode.jsonc")
	if err := os.Symlink(filepath.Join(dir, "missing-target.jsonc"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectOpenCodeMCP(link, MCPEntryOpts{ServerURL: "https://punk.example.test"}, false); err == nil {
		t.Fatal("broken config symlink must be refused")
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("broken symlink was replaced: mode=%v err=%v", info.Mode(), err)
	}
}

// A .jsonc input must not be compared against itself. Before the guard in
// ResolveOpenCodeConfigPath this returned a false "both exist" ambiguity
// error, because the derived candidate path was the input path.
// codeops:trace repo=punk-records work_item=punk-connect-remote-url-review-20261009 spec=docs/client-credentials.md test=TestResolveOpenCodeConfigPathJSONCInputDoesNotSelfCollide
func TestResolveOpenCodeConfigPathJSONCInputDoesNotSelfCollide(t *testing.T) {
	dir := t.TempDir()
	jsoncPath := filepath.Join(dir, "opencode.jsonc")
	if err := os.WriteFile(jsoncPath, []byte("{\n  // comment\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveOpenCodeConfigPath(jsoncPath)
	if err != nil {
		t.Fatalf("jsonc input must resolve, got err=%v", err)
	}
	if got != jsoncPath {
		t.Fatalf("got %q, want %q", got, jsoncPath)
	}

	// Genuine ambiguity must still be refused from either input form.
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{jsoncPath, filepath.Join(dir, "opencode.json")} {
		if _, err := ResolveOpenCodeConfigPath(in); err == nil {
			t.Fatalf("both files exist; %q must be refused", in)
		}
	}
}
