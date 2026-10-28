package hookcli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteShellEnv(t *testing.T) {
	home := t.TempDir()
	env := filepath.Join(home, ".punk", "env")
	bashrc := filepath.Join(home, ".bashrc")
	if err := os.WriteFile(bashrc, []byte("export FOO=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := WriteShellEnv(env, ShellRCFiles(home), "http://localhost:9090", "prk_it's")
	if err != nil || !res.EnvChanged || len(res.RCChanged) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if fi, _ := os.Stat(env); fi.Mode().Perm() != 0o600 {
		t.Fatalf("env mode %v, want 0600", fi.Mode().Perm())
	}
	rc, _ := os.ReadFile(bashrc)
	if !strings.HasPrefix(string(rc), "export FOO=1\n\n# >>> punk >>>") || strings.Contains(string(rc), "prk_") {
		t.Fatalf("bashrc must keep its content, gain only the source block, never the key:\n%s", rc)
	}
	// The rendered file is valid sh and round-trips a quote in the key.
	if sh, err := exec.LookPath("sh"); err == nil {
		out, err := exec.Command(sh, "-c", `. "$0" && printf '%s|%s' "$PUNK_URL" "$PUNK_API_KEY"`, env).CombinedOutput()
		if err != nil || string(out) != "http://localhost:9090|prk_it's" {
			t.Fatalf("sourced env = %q, %v", out, err)
		}
	}
	// Idempotent; a new key rewrites only the env file.
	if res, err := WriteShellEnv(env, ShellRCFiles(home), "http://localhost:9090", "prk_it's"); err != nil || res.EnvChanged || len(res.RCChanged) != 0 {
		t.Fatalf("rerun changed something: %+v %v", res, err)
	}
	if res, err := WriteShellEnv(env, ShellRCFiles(home), "http://h:9090", ""); err != nil || !res.EnvChanged || len(res.RCChanged) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := os.ReadFile(env); strings.Contains(string(b), "PUNK_API_KEY") {
		t.Fatalf("keyless server must not export a key:\n%s", b)
	}
	// zshrc only when it exists; a hand-written env file is refused.
	if got := ShellRCFiles(home); len(got) != 1 {
		t.Fatalf("rc files %v", got)
	}
	other := filepath.Join(home, "other-env")
	if err := os.WriteFile(other, []byte("export X=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteShellEnv(other, nil, "http://h", ""); err == nil {
		t.Fatal("overwrote an env file punk did not write")
	}
}
