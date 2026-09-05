package hookcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillTargetsTable(t *testing.T) {
	env := func(k string) string {
		if k == "CODEX_HOME" {
			return "/ch"
		}
		return ""
	}
	cases := map[string]string{
		"claude-code": "/home/u/.claude/skills/punk-memory/SKILL.md",
		"codex":       "/ch/skills/punk-memory/SKILL.md",
		"opencode":    "/home/u/.agents/skills/punk-memory/SKILL.md",
		"cursor":      "/home/u/.agents/skills/punk-memory/SKILL.md",
		"copilot":     "/home/u/.agents/skills/punk-memory/SKILL.md",
		"antigravity": "/home/u/.gemini/config/skills/punk-memory/SKILL.md",
		"hermes":      "/home/u/.hermes/skills/memory/punk-memory/SKILL.md",
		"openclaw":    "/home/u/.agents/skills/punk-memory/SKILL.md",
		"pi":          "/home/u/.pi/agent/skills/punk-memory/SKILL.md",
	}
	for agent, want := range cases {
		ts, err := SkillTargets(agent, false, "/home/u", env)
		if err != nil || len(ts) != 1 || ts[0].Path != want {
			t.Fatalf("%s global: %+v %v", agent, ts, err)
		}
		if ts[0].Opts.Agent != agent {
			t.Fatalf("%s: opts.Agent = %q", agent, ts[0].Opts.Agent)
		}
	}
	if ts, _ := SkillTargets("claude-code", true, "/home/u", env); ts[0].Path != filepath.Join(".claude", "skills", "punk-memory", "SKILL.md") {
		t.Fatalf("claude project: %s", ts[0].Path)
	}
	if ts, _ := SkillTargets("codex", true, "/home/u", env); ts[0].Path != filepath.Join(".agents", "skills", "punk-memory", "SKILL.md") {
		t.Fatalf("codex project: %s", ts[0].Path)
	}
	if ts, _ := SkillTargets("pi", false, "/home/u", env); ts[0].Opts.ToolPrefix != "punk_" || !ts[0].Opts.Pi {
		t.Fatalf("pi opts: %+v", ts[0].Opts)
	}
	if ts, _ := SkillTargets("claude-code", false, "/home/u", env); ts[0].Opts.ToolPrefix != "mcp__punk__" {
		t.Fatalf("claude prefix: %+v", ts[0].Opts)
	}
	if ts, _ := SkillTargets("hermes", false, "/home/u", env); !ts[0].Opts.Hermes {
		t.Fatal("hermes opts")
	}
	if _, err := SkillTargets("emacs", false, "/home/u", env); err == nil {
		t.Fatal("unknown agent must error")
	}
}

func TestWriteSkillMarkerGated(t *testing.T) {
	p := filepath.Join(t.TempDir(), "skills", "punk-memory", "SKILL.md")
	content := RenderSkill(SkillOpts{Agent: "cursor"})
	changed, err := WriteSkill(p, content)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	if changed, _ := WriteSkill(p, content); changed {
		t.Fatal("idempotent")
	}
	if err := os.WriteFile(p, []byte("---\nname: punk-memory\ndescription: mine\n---\nhand written\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteSkill(p, content); err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("hand-written file must be refused with the path in the error: %v", err)
	}
}

// writeSkillFile is the tests' WriteSkill-shaped helper: create parent
// dirs, write content, no marker gate (fixtures deliberately include
// foreign and customized files WriteSkill itself would refuse).
func writeSkillFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func codexTestEnv(codexHome string) func(string) string {
	return func(k string) string {
		if k == "CODEX_HOME" {
			return codexHome
		}
		return ""
	}
}

// TestReconcileCodexSkillTargetsSkipsByteEquivalentShared is the C03 red
// proof for skills: a punk-managed, byte-identical punk-memory/punk-plan
// already installed at the shared ~/.agents/skills root (e.g. by punk
// connect cursor/copilot/openclaw, which render the identical neutral
// text) makes the CODEX_HOME copy redundant - Codex discovers both roots,
// so installing there is exactly the duplicate-catalog defect. The codex
// targets must be dropped, and a stale byte-identical CODEX_HOME copy from
// an earlier connect must be removed.
func TestReconcileCodexSkillTargetsSkipsByteEquivalentShared(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	env := codexTestEnv(codexHome)

	targets, err := AllSkillTargets("codex", false, home, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %+v", targets)
	}

	// Install the byte-equivalent copies at the shared root first.
	for _, tg := range targets {
		shared := filepath.Join(home, ".agents", "skills", tg.Name, "SKILL.md")
		writeSkillFile(t, shared, Render(tg))
		// And a stale duplicate at the codex root, as an older connect left.
		writeSkillFile(t, tg.Path, Render(tg))
	}

	kept, notes := ReconcileCodexSkillTargets(targets, home, env)
	if len(kept) != 0 {
		t.Fatalf("byte-equivalent shared copies must make every codex target redundant, kept=%+v", kept)
	}
	joined := strings.Join(notes, "\n")
	for _, tg := range targets {
		if _, err := os.Stat(tg.Path); !os.IsNotExist(err) {
			t.Fatalf("stale duplicate %s must be removed, stat err=%v", tg.Path, err)
		}
		if !strings.Contains(joined, tg.Name) {
			t.Fatalf("notes must explain the %s dedupe: %s", tg.Name, joined)
		}
		shared := filepath.Join(home, ".agents", "skills", tg.Name, "SKILL.md")
		if _, err := os.Stat(shared); err != nil {
			t.Fatalf("shared copy must be preserved: %v", err)
		}
	}
}

// TestReconcileCodexSkillTargetsKeepsCustomizedShared pins the other half
// of the red proof: a punk-managed but byte-DIFFERENT shared copy (another
// client's rendering, or a user edit inside the marker) is never deleted
// and never treated as satisfying Codex's install - the codex target is
// kept and the note is an actionable collision diagnostic naming both
// paths.
func TestReconcileCodexSkillTargetsKeepsCustomizedShared(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	env := codexTestEnv(codexHome)

	targets, err := AllSkillTargets("codex", false, home, env)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate an opencode-rendered shared copy: punk-managed, different
	// bytes (punk_ tool prefix).
	for _, tg := range targets {
		custom := Render(tg) + "\nCustomized for another client.\n"
		writeSkillFile(t, filepath.Join(home, ".agents", "skills", tg.Name, "SKILL.md"), custom)
	}

	kept, notes := ReconcileCodexSkillTargets(targets, home, env)
	if len(kept) != len(targets) {
		t.Fatalf("customized shared copy must not satisfy or block the codex install, kept=%d want %d", len(kept), len(targets))
	}
	joined := strings.Join(notes, "\n")
	for _, tg := range targets {
		shared := filepath.Join(home, ".agents", "skills", tg.Name, "SKILL.md")
		raw, err := os.ReadFile(shared)
		if err != nil || !strings.Contains(string(raw), "Customized for another client.") {
			t.Fatalf("customized shared bytes must be untouched: %v", err)
		}
		if !strings.Contains(joined, tg.Name) || !strings.Contains(joined, shared) {
			t.Fatalf("collision diagnostic must name the skill and the shared path: %s", joined)
		}
	}
}

// TestReconcileCodexSkillTargetsForeignShared: a file at the shared root
// that punk did not write (no marker) is foreign content - never removed,
// never treated as ours; the codex install proceeds and the diagnostic
// says the foreign entry is out of punk's control.
func TestReconcileCodexSkillTargetsForeignShared(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	env := codexTestEnv(codexHome)

	targets, err := AllSkillTargets("codex", false, home, env)
	if err != nil {
		t.Fatal(err)
	}
	foreign := "---\nname: punk-memory\ndescription: hand written\n---\nmine\n"
	for _, tg := range targets {
		writeSkillFile(t, filepath.Join(home, ".agents", "skills", tg.Name, "SKILL.md"), foreign)
	}

	kept, notes := ReconcileCodexSkillTargets(targets, home, env)
	if len(kept) != len(targets) {
		t.Fatalf("foreign shared copy must not satisfy the codex install, kept=%d want %d", len(kept), len(targets))
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "not managed by punk") {
		t.Fatalf("diagnostic must call out the foreign file: %s", joined)
	}
	for _, tg := range targets {
		raw, _ := os.ReadFile(filepath.Join(home, ".agents", "skills", tg.Name, "SKILL.md"))
		if string(raw) != foreign {
			t.Fatalf("foreign bytes must be untouched")
		}
	}
}

// TestReconcileCodexSkillTargetsSymlinkedShared: a CODEX_HOME skill that
// is a symlink onto the shared file is already deduped by construction -
// the target is dropped and nothing is removed or rewritten.
func TestReconcileCodexSkillTargetsSymlinkedShared(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	env := codexTestEnv(codexHome)

	targets, err := AllSkillTargets("codex", false, home, env)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range targets {
		shared := filepath.Join(home, ".agents", "skills", tg.Name, "SKILL.md")
		writeSkillFile(t, shared, Render(tg))
		if err := os.MkdirAll(filepath.Dir(tg.Path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(shared, tg.Path); err != nil {
			t.Fatal(err)
		}
	}

	kept, notes := ReconcileCodexSkillTargets(targets, home, env)
	if len(kept) != 0 {
		t.Fatalf("symlinked codex copy is already the shared file, kept=%+v", kept)
	}
	for _, tg := range targets {
		fi, err := os.Lstat(tg.Path)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlink at %s must be preserved: %v", tg.Path, err)
		}
	}
	_ = notes
}

// TestReconcileCodexSkillTargetsNoSharedCopy: with nothing at the shared
// root every target is kept and no notes are produced - a first-time codex
// connect is unchanged.
func TestReconcileCodexSkillTargetsNoSharedCopy(t *testing.T) {
	home := t.TempDir()
	env := codexTestEnv(filepath.Join(home, ".codex"))
	targets, err := AllSkillTargets("codex", false, home, env)
	if err != nil {
		t.Fatal(err)
	}
	kept, notes := ReconcileCodexSkillTargets(targets, home, env)
	if len(kept) != len(targets) || len(notes) != 0 {
		t.Fatalf("kept=%d notes=%v", len(kept), notes)
	}
}

// TestReconcileCodexSkillTargetsPreservesCustomizedTargetThroughInstall is
// the review regression for the overwrite bug: a punk-managed but
// CUSTOMIZED codex target, with a byte-equivalent managed shared copy
// satisfying the install, must survive the COMPLETE install flow - the
// reconcile pass must drop it from kept so the WriteSkill loop never
// touches it. (Modeled on the reviewer's
// TestReviewerC03PreserveCustomTargetThroughInstall.)
func TestReconcileCodexSkillTargetsPreservesCustomizedTargetThroughInstall(t *testing.T) {
	home := t.TempDir()
	env := codexTestEnv(filepath.Join(home, ".codex"))
	targets, err := AllSkillTargets("codex", false, home, env)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range targets {
		writeSkillFile(t, filepath.Join(home, ".agents", "skills", tg.Name, "SKILL.md"), Render(tg))
		custom := Render(tg) + "\nUser customization must survive.\n"
		writeSkillFile(t, tg.Path, custom)

		kept, notes := ReconcileCodexSkillTargets([]SkillTarget{tg}, home, env)
		for _, target := range kept {
			if _, err := WriteSkill(target.Path, Render(target)); err != nil {
				t.Fatal(err)
			}
		}
		got, err := os.ReadFile(tg.Path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != custom {
			t.Fatalf("%s: customized managed target overwritten by the normal install flow", tg.Name)
		}
		if len(kept) != 0 {
			t.Fatalf("%s: customized target must drop out of the install set, kept=%+v", tg.Name, kept)
		}
		if !strings.Contains(strings.Join(notes, "\n"), "customized") {
			t.Fatalf("%s: note must call the file customized: %v", tg.Name, notes)
		}
	}
}

// TestReconcileCodexSkillTargetsPopulatedOptsDedupe is the helper-level
// half of the caller-order regression: with effective options populated on
// the targets BEFORE reconcile (the order installSkillFor now uses), a
// shared copy rendered with those same options is recognized as
// byte-equivalent and every codex target drops. The actual installer
// ordering is covered end-to-end by cmd/punk's
// TestInstallSkillForCodexDedupesAgainstSharedRoot; this test alone would
// also pass under the old caller order, since it sets options itself.
func TestReconcileCodexSkillTargetsPopulatedOptsDedupe(t *testing.T) {
	home := t.TempDir()
	env := codexTestEnv(filepath.Join(home, ".codex"))
	targets, err := AllSkillTargets("codex", false, home, env)
	if err != nil {
		t.Fatal(err)
	}
	for i := range targets {
		targets[i].Opts.ServerURL = "http://localhost:9090"
		targets[i].Opts.Namespace = "agent-proj-1234"
	}
	for _, tg := range targets {
		writeSkillFile(t, filepath.Join(home, ".agents", "skills", tg.Name, "SKILL.md"), Render(tg))
	}
	kept, _ := ReconcileCodexSkillTargets(targets, home, env)
	if len(kept) != 0 {
		t.Fatalf("populated shared copies must satisfy the install, kept=%+v", kept)
	}
}

// TestReconcileCodexSkillTargetsSymlinkDistinctTarget: a codex target
// symlinked to some OTHER file is not deduplicated and not written
// through - it drops out of kept with a diagnostic naming the real target.
func TestReconcileCodexSkillTargetsSymlinkDistinctTarget(t *testing.T) {
	home := t.TempDir()
	env := codexTestEnv(filepath.Join(home, ".codex"))
	targets, err := AllSkillTargets("codex", false, home, env)
	if err != nil {
		t.Fatal(err)
	}
	tg := targets[0]
	writeSkillFile(t, filepath.Join(home, ".agents", "skills", tg.Name, "SKILL.md"), Render(tg))
	other := filepath.Join(home, "elsewhere.md")
	writeSkillFile(t, other, "unrelated content\n")
	if err := os.MkdirAll(filepath.Dir(tg.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, tg.Path); err != nil {
		t.Fatal(err)
	}

	kept, notes := ReconcileCodexSkillTargets([]SkillTarget{tg}, home, env)
	if len(kept) != 0 {
		t.Fatalf("distinct-target symlink must drop out of the install set, kept=%+v", kept)
	}
	joined := strings.Join(notes, "\n")
	if strings.Contains(joined, "already deduplicated") || !strings.Contains(joined, other) {
		t.Fatalf("diagnostic must name the real link target and must not claim dedupe: %s", joined)
	}
	raw, _ := os.ReadFile(other)
	if string(raw) != "unrelated content\n" {
		t.Fatal("link target must be untouched")
	}
}

// TestReconcileCodexSkillTargetsBrokenSymlink: a broken symlink at the
// codex target drops out of kept with a truthful diagnostic - never
// claimed to be deduplicated, never written over.
func TestReconcileCodexSkillTargetsBrokenSymlink(t *testing.T) {
	home := t.TempDir()
	env := codexTestEnv(filepath.Join(home, ".codex"))
	targets, err := AllSkillTargets("codex", false, home, env)
	if err != nil {
		t.Fatal(err)
	}
	tg := targets[0]
	writeSkillFile(t, filepath.Join(home, ".agents", "skills", tg.Name, "SKILL.md"), Render(tg))
	if err := os.MkdirAll(filepath.Dir(tg.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "does-not-exist.md"), tg.Path); err != nil {
		t.Fatal(err)
	}

	kept, notes := ReconcileCodexSkillTargets([]SkillTarget{tg}, home, env)
	if len(kept) != 0 {
		t.Fatalf("broken symlink must drop out of the install set, kept=%+v", kept)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "does not resolve") {
		t.Fatalf("diagnostic must say the link does not resolve: %s", joined)
	}
	fi, err := os.Lstat(tg.Path)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("broken symlink must be preserved: %v", err)
	}
}

// TestReconcileCodexSkillTargetsSymlinkAcrossSharedStates pins the symlink
// counterpart of the across-states preservation contract: whatever the
// shared root holds (equivalent, customized, foreign, absent), a symlinked
// codex target drops out of the install set and is never written through
// or removed.
func TestReconcileCodexSkillTargetsSymlinkAcrossSharedStates(t *testing.T) {
	for _, state := range []string{"equivalent", "customized", "foreign", "absent"} {
		t.Run(state, func(t *testing.T) {
			home := t.TempDir()
			env := codexTestEnv(filepath.Join(home, ".codex"))
			targets, err := AllSkillTargets("codex", false, home, env)
			if err != nil {
				t.Fatal(err)
			}
			tg := targets[0]
			tg.Opts.ServerURL = "http://localhost:19393"
			shared := filepath.Join(home, ".agents", "skills", tg.Name, "SKILL.md")
			switch state {
			case "equivalent":
				writeSkillFile(t, shared, Render(tg))
			case "customized":
				writeSkillFile(t, shared, Render(tg)+"\nShared customization.\n")
			case "foreign":
				writeSkillFile(t, shared, "User-owned shared skill.\n")
			}
			linkTarget := filepath.Join(home, "elsewhere.md")
			writeSkillFile(t, linkTarget, "unrelated content\n")
			if err := os.MkdirAll(filepath.Dir(tg.Path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(linkTarget, tg.Path); err != nil {
				t.Fatal(err)
			}

			kept, _ := ReconcileCodexSkillTargets([]SkillTarget{tg}, home, env)
			for _, target := range kept {
				if _, err := WriteSkill(target.Path, Render(target)); err != nil {
					t.Fatal(err)
				}
			}
			fi, err := os.Lstat(tg.Path)
			if err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("symlink must be preserved: %v", err)
			}
			raw, _ := os.ReadFile(linkTarget)
			if string(raw) != "unrelated content\n" {
				t.Fatal("symlink target must be untouched")
			}
		})
	}
}

// TestReconcileCodexSkillTargetsParentSymlinkMustNotDeleteShared is the
// same-file preservation boundary from review round 3: CODEX_HOME as a
// symlink to ~/.agents makes the codex target and the shared file ONE
// file through a parent-directory alias; reconcile must not delete the
// sole shared copy. (Reviewer's
// TestReviewerC03SkillParentSymlinkMustNotDeleteShared.)
func TestReconcileCodexSkillTargetsParentSymlinkMustNotDeleteShared(t *testing.T) {
	home := t.TempDir()
	sharedRoot := filepath.Join(home, ".agents")
	if err := os.MkdirAll(sharedRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	codexRoot := filepath.Join(home, ".codex")
	if err := os.Symlink(sharedRoot, codexRoot); err != nil {
		t.Fatal(err)
	}
	env := codexTestEnv(codexRoot)
	targets, err := AllSkillTargets("codex", false, home, env)
	if err != nil {
		t.Fatal(err)
	}
	tg := targets[0]
	shared := filepath.Join(sharedRoot, "skills", tg.Name, "SKILL.md")
	writeSkillFile(t, shared, Render(tg))

	kept, notes := ReconcileCodexSkillTargets([]SkillTarget{tg}, home, env)
	if _, err := os.Stat(shared); err != nil {
		t.Fatalf("reconcile deleted the sole shared file through a parent-directory symlink: %v", err)
	}
	if len(kept) != 0 {
		t.Fatalf("same-file target must drop out of the install set, kept=%+v", kept)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "same file") {
		t.Fatalf("note must say the two paths are the same file: %v", notes)
	}
}

// TestReconcileCodexSkillTargetsExactSharedRootPreservesCustom: with
// CODEX_HOME == ~/.agents the target IS the shared file; a customized
// marker-bearing file there must survive the reconcile+install flow.
// (Reviewer's TestReviewerC03ExactSharedRootMustPreserveCustom.)
func TestReconcileCodexSkillTargetsExactSharedRootPreservesCustom(t *testing.T) {
	home := t.TempDir()
	env := codexTestEnv(filepath.Join(home, ".agents"))
	targets, err := AllSkillTargets("codex", false, home, env)
	if err != nil {
		t.Fatal(err)
	}
	tg := targets[0]
	custom := Render(tg) + "\nUser customization.\n"
	writeSkillFile(t, tg.Path, custom)

	kept, _ := ReconcileCodexSkillTargets([]SkillTarget{tg}, home, env)
	for _, target := range kept {
		if _, err := WriteSkill(target.Path, Render(target)); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(tg.Path)
	if string(got) != custom {
		t.Fatal("CODEX_HOME=.agents must preserve the customized shared file, not overwrite it")
	}
}
