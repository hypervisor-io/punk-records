package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

func TestMessagingSkillCLIAndInstallAreOptIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PUNK_MESSAGING", "")
	for _, enabled := range []bool{false, true} {
		args := []string{"print", "--agent", "claude-code", "--name", "punk-memory", "--url", "http://localhost:9090"}
		if enabled {
			args = append(args, "--messaging")
		}
		out, err := captureStdout(t, func() error { return cmdSkill(args) })
		if err != nil {
			t.Fatal(err)
		}
		want := hookcli.RenderSkill(hookcli.SkillOpts{Agent: "claude-code", ToolPrefix: "mcp__punk__", ServerURL: "http://localhost:9090", Messaging: enabled})
		if out != want {
			t.Fatalf("CLI output differs enabled=%v", enabled)
		}
	}
	for _, enabled := range []bool{false, true} {
		installSkillFor("claude-code", false, "http://localhost:9090", "", enabled)
		for _, skill := range []string{"punk-memory", "punk-plan"} {
			raw, err := os.ReadFile(filepath.Join(home, ".claude", "skills", skill, "SKILL.md"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "send_message") != enabled {
				t.Fatalf("install guidance %s enabled=%v", skill, enabled)
			}
		}
	}
	t.Setenv("PUNK_MESSAGING", "0")
	if messagingGuidanceEnabled(true) {
		t.Fatal("env false did not override guidance flag")
	}
	t.Setenv("PUNK_MESSAGING", "1")
	if !messagingGuidanceEnabled() {
		t.Fatal("env true did not enable guidance")
	}
}
