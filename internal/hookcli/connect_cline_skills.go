package hookcli

import (
	"path/filepath"
)

// P2-C: Cline command files. Cline documents BOTH surfaces - Agent Skills
// (a <name>/SKILL.md directory bundle) and workflows (.clinerules/
// workflows/, docs.cline.bot), and v4.1.4 lists both in the slash menu -
// but its current recommended native command surface is Agent Skills,
// which is what punk installs (verified 2026-10-27 against
// docs.cline.bot/customization/skills and the docs index at
// docs.cline.bot/llms.txt). Skills live under ~/.cline/skills/ (global)
// or .cline/skills/ (workspace, the recommended location;
// .clinerules/skills/ and .claude/skills/ also work but are not punk's to
// choose). The frontmatter name must match the directory name, and any
// enabled skill is invocable from chat as /<name> - exactly the
// slash-command behavior the punk commands need. Skills are the
// right choice over workflows here: one file per command, no separate
// workflow format to track, and the same SKILL.md wrapper every other
// client gets.
//
// The commands therefore install as ordinary Cline skills carrying
// the shared P2-A bodies: CommandSkillTargets renders them with the
// same SKILL.md wrapper every other client gets, just rooted at Cline's
// own skill tree. punk's hooks and MCP settings for Cline are wired by
// ConnectCline/ClineMCPSettingsPath (connect_cline.go); this file only
// owns the command-skill surface.

// ClineSkillRoot resolves Cline's native skill root: ~/.cline/skills
// (global) or .cline/skills (workspace). Cline documents no environment
// override for the skill location (unlike its MCP settings path), so
// none is honored here.
func ClineSkillRoot(project bool, home string) string {
	if project {
		return filepath.Join(".cline", "skills")
	}
	return filepath.Join(home, ".cline", "skills")
}

// ClineCommandSkillTargets lists the punk commands as Cline skills:
// <skill root>/punk-<command>/SKILL.md, slash-invocable as
// /punk-<command>.
func ClineCommandSkillTargets(project bool, home string) ([]SkillTarget, error) {
	dir := ClineSkillRoot(project, home)
	o := SkillOpts{Agent: "cline"}
	out := make([]SkillTarget, 0, len(CommandNames))
	for _, name := range CommandNames {
		co := o
		co.Command = name
		out = append(out, SkillTarget{Name: name, Path: filepath.Join(dir, name, "SKILL.md"), Opts: co})
	}
	return out, nil
}
