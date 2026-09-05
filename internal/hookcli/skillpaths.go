package hookcli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SkillTarget is one SKILL.md punk writes for an agent. Name is the
// skill it carries (SkillName or PlanSkillName); Render picks the text.
type SkillTarget struct {
	Name string
	Path string
	Opts SkillOpts
}

func skillFile(dir string) string { return filepath.Join(dir, SkillName, "SKILL.md") }

// PlanSkillTargets mirrors SkillTargets for the punk-plan skill: the same
// per-agent skill directory, a sibling folder named punk-plan.
func PlanSkillTargets(agent string, project bool, home string, env func(string) string) ([]SkillTarget, error) {
	base, err := SkillTargets(agent, project, home, env)
	if err != nil {
		return nil, err
	}
	out := make([]SkillTarget, 0, len(base))
	for _, tg := range base {
		dir := filepath.Dir(filepath.Dir(tg.Path))
		out = append(out, SkillTarget{Name: PlanSkillName, Path: filepath.Join(dir, PlanSkillName, "SKILL.md"), Opts: tg.Opts})
	}
	return out, nil
}

// AllSkillTargets is every skill punk installs for an agent: punk-memory
// then punk-plan. punk connect and punk skill install walk this list.
func AllSkillTargets(agent string, project bool, home string, env func(string) string) ([]SkillTarget, error) {
	mem, err := SkillTargets(agent, project, home, env)
	if err != nil {
		return nil, err
	}
	plan, err := PlanSkillTargets(agent, project, home, env)
	if err != nil {
		return nil, err
	}
	return append(mem, plan...), nil
}

// SkillTargets lists where an agent loads skills from, global or project,
// with the rendering options for that agent. Paths come from each
// vendor's documentation; agents that read the shared ~/.agents/skills
// location get that single file.
func SkillTargets(agent string, project bool, home string, env func(string) string) ([]SkillTarget, error) {
	if env == nil {
		env = os.Getenv
	}
	agents := filepath.Join(home, ".agents", "skills")
	projAgents := filepath.Join(".agents", "skills")
	one := func(p string, o SkillOpts) []SkillTarget {
		o.Agent = agent
		return []SkillTarget{{Name: SkillName, Path: skillFile(p), Opts: o}}
	}
	switch agent {
	case "claude-code":
		o := SkillOpts{ToolPrefix: "mcp__punk__"}
		if project {
			return one(filepath.Join(".claude", "skills"), o), nil
		}
		return one(filepath.Join(home, ".claude", "skills"), o), nil
	case "codex":
		if project {
			return one(projAgents, SkillOpts{}), nil
		}
		ch := env("CODEX_HOME")
		if ch == "" {
			ch = filepath.Join(home, ".codex")
		}
		return one(filepath.Join(ch, "skills"), SkillOpts{}), nil
	case "opencode":
		if project {
			return one(projAgents, SkillOpts{ToolPrefix: "punk_"}), nil
		}
		return one(agents, SkillOpts{ToolPrefix: "punk_"}), nil
	case "cursor", "copilot", "openclaw":
		if project {
			return one(projAgents, SkillOpts{}), nil
		}
		return one(agents, SkillOpts{}), nil
	case "antigravity":
		if project {
			return one(projAgents, SkillOpts{}), nil
		}
		return one(filepath.Join(home, ".gemini", "config", "skills"), SkillOpts{}), nil
	case "hermes":
		// Hermes documents no project location; the global category tree is the only target.
		return []SkillTarget{{Name: SkillName, Path: filepath.Join(home, ".hermes", "skills", "memory", SkillName, "SKILL.md"), Opts: SkillOpts{Agent: agent, Hermes: true}}}, nil
	case "pi":
		if project {
			return one(projAgents, SkillOpts{ToolPrefix: "punk_", Pi: true}), nil
		}
		return one(filepath.Join(home, ".pi", "agent", "skills"), SkillOpts{ToolPrefix: "punk_", Pi: true}), nil
	}
	return nil, fmt.Errorf("no skill location known for agent %q", agent)
}

// sameFile reports whether a and b name the same underlying file:
// literally identical after absolute-path normalization, or the same
// inode via os.SameFile on Stat results (which follows symlinks at any
// path depth, so a CODEX_HOME that is a symlink to ~/.agents, or a
// hardlink, is caught). Unstatable paths yield false - callers treat
// false as "not provably the same file", never as proof of difference.
func sameFile(a, b string) bool {
	aa, errA := filepath.Abs(a)
	ba, errB := filepath.Abs(b)
	if errA == nil && errB == nil && filepath.Clean(aa) == filepath.Clean(ba) {
		return true
	}
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	if errA != nil || errB != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// ReconcileCodexSkillTargets filters Codex skill install targets against
// the OTHER skill root Codex discovers. Codex loads skills from both
// CODEX_HOME/skills and the shared ~/.agents/skills tree, so when another
// agent's connect (cursor, copilot, openclaw - all of which render the
// identical neutral skill text) already installed punk-memory/punk-plan at
// the shared root, installing the same bytes again under CODEX_HOME is
// exactly the duplicate-catalog defect C03 fixes: the session's skill
// catalog then lists every punk skill twice.
//
// Callers MUST populate each target's effective Opts (ServerURL,
// Namespace) BEFORE calling - Render(tg) is the comparison baseline, so
// comparing with blank options against a file written with real ones
// would never recognize the byte-equivalence this function exists to
// find. installSkillFor (cmd/punk) assigns options for all targets first.
//
// Two invariants govern every decision, in this order:
//
//  1. TARGET PRESERVATION, independent of the shared copy's state. A
//     target is returned in kept (i.e. the caller's WriteSkill loop may
//     write it) ONLY when the path is absent, or holds a plain regular
//     file punk wrote whose bytes equal the current rendering. Anything
//     else at the target - marker-bearing bytes that differ from the
//     current rendering (a user customization, or an older render this
//     reconcile cannot PROVE is unmodified generated content), a foreign
//     file, any symlink (to the shared copy, elsewhere, or broken), or a
//     non-regular file - drops out of kept with a diagnostic and is never
//     overwritten, written through, or deleted. SkillMarker proves punk
//     created the file; it does not prove every byte is still punk's, so
//     differing marker-bearing bytes are preserved rather than treated as
//     disposable. The diagnostic names the path and tells the user to
//     delete it by hand if they want the current rendering installed.
//     Consequence: a customized codex skill no longer auto-updates on
//     connect - deliberate, per the preservation contract.
//  2. SHARED-ROOT DEDUPE. When the shared file is punk-managed AND
//     byte-identical to the current rendering, the codex copy is redundant
//     for the catalog: an absent target stays absent (skip note), and a
//     plain-file target holding exactly those bytes is removed (proven
//     generated content, punk may reclaim it). A symlink that resolves to
//     exactly the shared file is reported as already deduplicated. When
//     the shared copy does not satisfy the install - absent, foreign, or
//     byte-different (customized, or another client's rendering such as
//     OpenCode's punk_ tool prefix) - the note says what Codex's catalog
//     will show and why punk cannot resolve it, and the shared bytes are
//     always preserved untouched.
//
// home is the user's home directory; env resolves CODEX_HOME (nil means
// os.Getenv), matching SkillTargets. Reconciliation is idempotent: once
// the catalog is deduped and every survivor is preserved, reruns make no
// filesystem writes.
func ReconcileCodexSkillTargets(targets []SkillTarget, home string, env func(string) string) (kept []SkillTarget, notes []string) {
	for _, tg := range targets {
		render := Render(tg)
		shared := filepath.Join(home, ".agents", "skills", tg.Name, "SKILL.md")
		if sameFile(shared, tg.Path) {
			// The codex root and the shared root name the SAME file
			// (CODEX_HOME == ~/.agents, or a symlinked/hardlinked alias at
			// any depth): there is already exactly one catalog source, and
			// whatever bytes it holds are preserved untouched - writing the
			// current render here would overwrite a customization, and
			// removing it would delete the sole shared copy for every
			// client. Only a genuinely absent file is a fresh install.
			if _, err := os.Lstat(tg.Path); errors.Is(err, os.ErrNotExist) {
				kept = append(kept, tg)
				continue
			}
			notes = append(notes, fmt.Sprintf("skill %s: %s and the codex target are the same file; leaving its current bytes untouched (one catalog source)", tg.Name, tg.Path))
			continue
		}

		// Invariant 1: classify the target before anything else.
		fi, statErr := os.Lstat(tg.Path)
		targetAbsent := errors.Is(statErr, os.ErrNotExist)
		targetWritable := targetAbsent // absent or plain file with current bytes
		var targetNote string
		switch {
		case targetAbsent:
		case statErr != nil:
			targetNote = fmt.Sprintf("skill %s: could not inspect %s (%v); leaving it alone", tg.Name, tg.Path, statErr)
		case fi.Mode()&os.ModeSymlink != 0:
			resolved, evalErr := filepath.EvalSymlinks(tg.Path)
			sharedResolved, sharedEvalErr := filepath.EvalSymlinks(shared)
			switch {
			case evalErr != nil:
				targetNote = fmt.Sprintf("skill %s: %s is a symlink that does not resolve (%v); leaving it untouched - fix or remove the link by hand", tg.Name, tg.Path, evalErr)
			case sharedEvalErr == nil && resolved == sharedResolved:
				targetNote = fmt.Sprintf("skill %s: %s is a symlink to the shared copy; already deduplicated", tg.Name, tg.Path)
			default:
				targetNote = fmt.Sprintf("skill %s: %s is a symlink to %s, not the shared copy %s; leaving the link untouched - resolve any duplicate by hand", tg.Name, tg.Path, resolved, shared)
			}
		case !fi.Mode().IsRegular():
			targetNote = fmt.Sprintf("skill %s: %s exists and is not a regular file; leaving it untouched", tg.Name, tg.Path)
		default:
			existing, readErr := os.ReadFile(tg.Path)
			switch {
			case readErr != nil:
				targetNote = fmt.Sprintf("skill %s: could not read %s (%v); leaving it alone", tg.Name, tg.Path, readErr)
			case !IsManagedSkill(existing):
				targetNote = fmt.Sprintf("skill %s: %s is not managed by punk; leaving the foreign file untouched", tg.Name, tg.Path)
			case string(existing) == render:
				targetWritable = true // up to date; WriteSkill would no-op
			default:
				targetNote = fmt.Sprintf("skill %s: %s is punk-managed but differs from the current rendering (customized, or written by an older punk version); preserved, not overwritten - delete it by hand to install the current rendering", tg.Name, tg.Path)
			}
		}

		// Invariant 2: classify the shared copy.
		sharedBytes, sharedErr := os.ReadFile(shared)
		sharedExists := sharedErr == nil
		sharedEquivalent := sharedExists && IsManagedSkill(sharedBytes) && string(sharedBytes) == render
		var sharedNote string
		switch {
		case errors.Is(sharedErr, os.ErrNotExist):
		case sharedErr != nil:
			sharedNote = fmt.Sprintf("skill %s: could not inspect %s (%v)", tg.Name, shared, sharedErr)
		case !IsManagedSkill(sharedBytes):
			sharedNote = fmt.Sprintf("skill %s: %s is not managed by punk; codex will list two %s entries and punk cannot resolve the foreign one - remove or rename it by hand if unwanted", tg.Name, shared, tg.Name)
		case !sharedEquivalent:
			sharedNote = fmt.Sprintf("skill %s: %s is punk-managed but differs from the codex rendering (customized, or written for another client's tool names); its bytes are preserved so that client keeps its skill - reconcile the duplicate catalog entry by hand if unwanted", tg.Name, shared)
		}

		// Combine: dedupe only ever removes or skips PROVEN-generated
		// content; preservation always wins.
		switch {
		case sharedEquivalent && targetAbsent:
			notes = append(notes, fmt.Sprintf("skill %s: %s already provides the identical punk skill; skipping the redundant codex copy", tg.Name, shared))
		case sharedEquivalent && targetWritable && !targetAbsent:
			// The same-file guard above already dropped every aliased
			// identity, so removal here is only reachable for two
			// genuinely distinct files. Check once more at the destructive
			// step itself: a removal must never delete the shared source.
			if sameFile(tg.Path, shared) {
				notes = append(notes, fmt.Sprintf("skill %s: %s and %s are the same file; nothing removed", tg.Name, tg.Path, shared))
				continue
			}
			if rmErr := os.Remove(tg.Path); rmErr != nil {
				notes = append(notes, fmt.Sprintf("skill %s: could not remove stale duplicate %s (%v); leaving it", tg.Name, tg.Path, rmErr))
			} else {
				notes = append(notes, fmt.Sprintf("skill %s: removed stale duplicate %s (%s already provides the identical punk skill)", tg.Name, tg.Path, shared))
			}
		case sharedEquivalent:
			// Target preserved (customized/foreign/symlink/...): report both.
			notes = append(notes, targetNote)
			if !strings.Contains(targetNote, "symlink to the shared copy") {
				notes = append(notes, fmt.Sprintf("skill %s: %s already provides the identical punk skill; the codex entry above is a duplicate of it", tg.Name, shared))
			}
		default:
			// Shared copy does not satisfy the install.
			if targetWritable {
				kept = append(kept, tg)
			} else if targetNote != "" {
				notes = append(notes, targetNote)
			}
			if sharedNote != "" {
				notes = append(notes, sharedNote)
			}
		}
	}
	return kept, notes
}

// WriteSkill writes content to path unless a file punk did not write is
// already there. Parent directories are created.
func WriteSkill(path, content string) (changed bool, err error) {
	existing, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		if !IsManagedSkill(existing) {
			return false, fmt.Errorf("%s exists and is not managed by punk (missing marker); leaving it alone", path)
		}
		if string(existing) == content {
			return false, nil
		}
	case errors.Is(readErr, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, err
		}
	default:
		return false, readErr
	}
	if err := writePreservingSymlinkAndMode(path, []byte(content), 0o644); err != nil {
		return false, err
	}
	return true, nil
}
