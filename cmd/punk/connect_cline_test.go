package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCmdConnectClinePathsAndMessaging(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, k := range []string{"CLINE_MCP_SETTINGS_PATH", "CLINE_DATA_DIR", "CLINE_DIR", "PUNK_API_KEY"} {
		t.Setenv(k, "")
	}
	_, err := captureStdout(t, func() error { return cmdConnect([]string{"cline", "--url", "http://localhost:9090", "--messaging"}) })
	if err != nil {
		t.Fatal(err)
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".ps1"
	}
	p := filepath.Join(home, "Documents", "Cline", "Hooks", "TaskStart"+suffix)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "--messaging") {
		t.Fatalf("missing messaging: %s", raw)
	}
	mp := filepath.Join(home, ".cline", "data", "settings", "cline_mcp_settings.json")
	if raw, err := os.ReadFile(mp); err != nil || !strings.Contains(string(raw), "streamableHttp") {
		t.Fatalf("MCP missing %v %s", err, raw)
	}
	before := string(raw)
	_, err = captureStdout(t, func() error { return cmdConnect([]string{"cline", "--url", "http://localhost:9090", "--messaging"}) })
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(p); string(raw) != before {
		t.Fatal("rerun changed hook")
	}
	t.Chdir(t.TempDir())
	_, err = captureStdout(t, func() error {
		return cmdConnect([]string{"cline", "--project", "--no-mcp", "--url", "http://localhost:9090"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(".clinerules", "hooks", "TaskStart"+suffix)); err != nil || strings.Contains(string(raw), "--messaging") {
		t.Fatalf("project hook %s %v", raw, err)
	}
}

func TestCmdHookClineOneReply(t *testing.T) {
	t.Setenv("PUNK_MESSAGING", "0")
	stdin, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	if _, err := stdin.WriteString(`{"taskId":"t","hookName":"Notification"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = old }()
	out, err := captureStdout(t, func() error {
		return cmdHook([]string{"--from", "cline", "--messaging", "--url", "http://127.0.0.1:1"})
	})
	if err != nil || out != `{"cancel":false}`+"\n" {
		t.Fatalf("Cline reply=%q %v", out, err)
	}
}
