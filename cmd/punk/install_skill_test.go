package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

// TestInstallSkillForCodexDedupesAgainstSharedRoot drives the REAL
// installer (installSkillFor, the function cmdConnectCodex calls) through
// its actual option-assignment ordering: a punk skill previously installed
// at the shared ~/.agents/skills root by another agent's connect renders
// with the connect-time ServerURL, so only assigning effective options to
// every target BEFORE ReconcileCodexSkillTargets compares renders
// recognizes the byte-equivalence. With the old ordering (reconcile first,
// options assigned in the write loop) this test's second connect installs
// a duplicate CODEX_HOME copy. HOME and CODEX_HOME are redirected into
// TempDir; no real user file is touched.
func TestInstallSkillForCodexDedupesAgainstSharedRoot(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)

	// Another client (cursor renders the identical neutral text into the
	// shared root) connects first, with the same server URL.
	installSkillFor("cursor", false, "http://127.0.0.1:19393", "")
	sharedSkill := filepath.Join(home, ".agents", "skills", "punk-memory", "SKILL.md")
	if _, err := os.Stat(sharedSkill); err != nil {
		t.Fatalf("shared install missing: %v", err)
	}

	// Codex connect must skip its own copy: identical skill already
	// discoverable via the shared root.
	installSkillFor("codex", false, "http://127.0.0.1:19393", "")
	codexSkill := filepath.Join(codexHome, "skills", "punk-memory", "SKILL.md")
	if _, err := os.Stat(codexSkill); !os.IsNotExist(err) {
		t.Fatalf("codex copy must be skipped when the shared root already provides the identical skill, stat err=%v", err)
	}

	// And the whole connect is idempotent.
	installSkillFor("codex", false, "http://127.0.0.1:19393", "")
	if _, err := os.Stat(codexSkill); !os.IsNotExist(err) {
		t.Fatalf("rerun must remain deduped, stat err=%v", err)
	}
}

// TestInstallSkillForCodexPreservesCustomizedTargetThroughRealInstaller is
// the installer-level preservation regression: a punk-managed but
// customized codex skill file must survive installSkillFor untouched, in
// both shared-root states (absent and equivalent). Through the real
// installer this exercises reconcile-then-write as one flow, not the
// helper alone.
func TestInstallSkillForCodexPreservesCustomizedTargetThroughRealInstaller(t *testing.T) {
	for _, sharedState := range []string{"absent", "equivalent"} {
		t.Run(sharedState, func(t *testing.T) {
			home := t.TempDir()
			codexHome := filepath.Join(home, ".codex")
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", codexHome)

			if sharedState == "equivalent" {
				installSkillFor("cursor", false, "http://127.0.0.1:19393", "")
			}

			codexSkill := filepath.Join(codexHome, "skills", "punk-memory", "SKILL.md")
			targets, err := hookcli.AllSkillTargets("codex", false, home, os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			tg := targets[0]
			tg.Opts.ServerURL = "http://127.0.0.1:19393"
			custom := hookcli.Render(tg) + "\nMy custom codex notes.\n"
			if err := os.MkdirAll(filepath.Dir(codexSkill), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(codexSkill, []byte(custom), 0o644); err != nil {
				t.Fatal(err)
			}

			installSkillFor("codex", false, "http://127.0.0.1:19393", "")
			got, err := os.ReadFile(codexSkill)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != custom {
				t.Fatalf("customized codex skill overwritten through the real installer (shared state %s)", sharedState)
			}
		})
	}
}

// TestInstallSkillForCodexFreshInstallUnchanged: with nothing at either
// root, the codex install lands exactly where it always did.
func TestInstallSkillForCodexFreshInstallUnchanged(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)

	installSkillFor("codex", false, "http://127.0.0.1:19393", "")
	for _, name := range []string{"punk-memory", "punk-plan"} {
		p := filepath.Join(codexHome, "skills", name, "SKILL.md")
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("fresh install missing %s: %v", p, err)
		}
		if !strings.Contains(string(raw), "http://127.0.0.1:19393") {
			t.Fatalf("%s must render the effective server URL", p)
		}
	}
}
