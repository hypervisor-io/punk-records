package hookcli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const dummyMCPToken = "dummy-token-for-permission-test"

var literalMCPWriters = map[string]func(string, MCPEntryOpts, bool) (bool, error){
	"ConnectAntigravityMCP": ConnectAntigravityMCP,
	"ConnectClaudeCodeMCP":  ConnectClaudeCodeMCP,
	"ConnectClineMCP":       ConnectClineMCP,
	"ConnectCopilotMCP":     ConnectCopilotMCP,
	"ConnectCursorMCP":      ConnectCursorMCP,
	"ConnectHermesMCP":      ConnectHermesMCP,
	"ConnectOpenClawMCP":    ConnectOpenClawMCP,
	"ConnectOpenCodeMCP":    ConnectOpenCodeMCP,
}

func requirePrivateConfig(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got&0o077 != 0 {
		t.Fatalf("secret-bearing config mode = %04o, want no group/other access", got)
	}
}

// codeops:trace repo=punk-records work_item=punk-agent-refresh-20261009 spec=docs/CONFIG.md plan=client-auth test=TestMCPConfigLiteralTokenCreatesPrivateFile
func TestMCPConfigLiteralTokenCreatesPrivateFile(t *testing.T) {
	for name, write := range literalMCPWriters {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.json")
			if _, err := write(p, MCPEntryOpts{ServerURL: "https://punk.example.test", APIKey: dummyMCPToken}, false); err != nil {
				t.Fatal(err)
			}
			requirePrivateConfig(t, p)
		})
	}
}

// TestMCPConfigWriterSecurityClassificationIsExhaustive is a structural
// guard paired with the concrete literal-token behavior matrix above. A new
// exported Connect*MCP writer must be deliberately added to that matrix or
// this test fails instead of silently defaulting to public config writes.
func TestMCPConfigWriterSecurityClassificationIsExhaustive(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(testFile), "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var exported []string
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			name := fn.Name.Name
			if ast.IsExported(name) && strings.HasPrefix(name, "Connect") && strings.HasSuffix(name, "MCP") {
				exported = append(exported, name)
			}
		}
	}
	classified := make([]string, 0, len(literalMCPWriters))
	for name := range literalMCPWriters {
		classified = append(classified, name)
	}
	sort.Strings(exported)
	sort.Strings(classified)
	if strings.Join(exported, "\n") != strings.Join(classified, "\n") {
		t.Fatalf("Connect*MCP security classification drift:\nexported:   %v\nclassified: %v", exported, classified)
	}
}

func TestMCPConfigCodexWriterIsEnvOnlyAndPrivate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if _, err := ConnectCodexConfig(p, MCPEntryOpts{
		ServerURL: "https://punk.example.test",
		APIKey:    dummyMCPToken,
	}, true, false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), dummyMCPToken) {
		t.Fatal("Codex is env-only and must never persist MCPEntryOpts.APIKey")
	}
	if !strings.Contains(string(raw), `bearer_token_env_var = "PUNK_API_KEY"`) {
		t.Fatalf("Codex config must classify authentication as env-only:\n%s", raw)
	}
	requirePrivateConfig(t, p)
}

func TestMCPConfigLiteralTokenTightensExistingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "opencode.json")
	if err := os.WriteFile(p, []byte(`{"theme":"dark"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectOpenCodeMCP(p, MCPEntryOpts{ServerURL: "https://punk.example.test", APIKey: dummyMCPToken}, false); err != nil {
		t.Fatal(err)
	}
	requirePrivateConfig(t, p)
	if readSettings(t, p)["theme"] != "dark" {
		t.Fatal("tightening permissions must preserve unrelated config")
	}

	// A byte-identical reconnect must still repair an unsafe mode.
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectOpenCodeMCP(p, MCPEntryOpts{ServerURL: "https://punk.example.test", APIKey: dummyMCPToken}, false)
	if err != nil || !changed {
		t.Fatalf("mode-only repair: changed=%v err=%v", changed, err)
	}
	requirePrivateConfig(t, p)
}

func TestMCPConfigLiteralTokenPreservesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "shared-opencode.json")
	link := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(target, []byte(`{"theme":"dark"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectOpenCodeMCP(link, MCPEntryOpts{ServerURL: "https://punk.example.test", APIKey: dummyMCPToken}, false); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config symlink was replaced: info=%v err=%v", info, err)
	}
	requirePrivateConfig(t, target)
	if readSettings(t, target)["theme"] != "dark" {
		t.Fatal("symlink-target update must preserve unrelated config")
	}
}

func TestMCPConfigEnvReferenceKeepsSafePublicMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "opencode.json")
	if _, err := ConnectOpenCodeMCP(p, MCPEntryOpts{ServerURL: "https://punk.example.test", APIKeyEnv: "PUNK_TEST_API_KEY"}, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("env-only config mode = %04o, want ordinary 0644 default", got)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), dummyMCPToken) {
		t.Fatal("env-only config must not contain a literal token")
	}
}

func TestConnectOpenClawMCPPrivateAcrossTwoStageFlow(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if _, err := ConnectOpenClaw(p); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectOpenClawMCP(p, MCPEntryOpts{ServerURL: "https://punk.example.test", APIKey: dummyMCPToken}, false); err != nil {
		t.Fatal(err)
	}
	requirePrivateConfig(t, p)
	if readSettings(t, p)["plugins"] == nil {
		t.Fatal("MCP stage must preserve the plugin stage")
	}

	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectOpenClawMCP(p, MCPEntryOpts{ServerURL: "https://punk.example.test", APIKey: dummyMCPToken}, false)
	if err != nil || !changed {
		t.Fatalf("OpenClaw mode-only repair: changed=%v err=%v", changed, err)
	}
	requirePrivateConfig(t, p)
}
