package hookcli

import (
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// P2-C: OpenCode custom commands. OpenCode's native command format
// (docs.opencode.ai/docs/commands, fetched 2026-10-27) is a markdown
// file under the global ~/.config/opencode/commands/ (XDG_CONFIG_HOME
// honored - the same base the plugin and config files use) or the
// per-project .opencode/commands/. The FILE name is the command name,
// typed as /<name> in the TUI; the YAML frontmatter carries an optional
// description (shown in the TUI's command list) and the body is the
// prompt template.
//
// punk renders the five command bodies from the shared P2-A source
// (CommandBody) with OpenCode's own tool prefix (punk_) and invocation
// syntax (/punk-<name>), so a command file and its skill file always
// carry identical prose - only the wrapper differs.

// openCodeCommandTmpl wraps one shared command body in the OpenCode
// command-file shape. Unlike the SKILL.md wrapper (commandFileTmpl) the
// frontmatter carries only description: in OpenCode the file name IS
// the command name, and keys the docs do not document are not punk's to
// invent.
var openCodeCommandTmpl = template.Must(template.New("opencode-command").Parse(`---
description: {{printf "%q" .Spec.Description}}
---
{{.Marker}}
{{.Body}}
`))

// OpenCodeCommandsDir resolves the native commands directory: global
// ~/.config/opencode/commands (XDG_CONFIG_HOME honored, the same base
// resolution the plugin/config paths in cmd/punk use) or per-project
// .opencode/commands.
func OpenCodeCommandsDir(project bool, xdgConfigHome, home string) string {
	if project {
		return filepath.Join(".opencode", "commands")
	}
	base := xdgConfigHome
	if base == "" {
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "opencode", "commands")
}

// OpenCodeCommandTargets lists the five punk commands as OpenCode
// command files: <commands dir>/punk-<command>.md, invoked as
// /punk-<command>.
func OpenCodeCommandTargets(project bool, home string, env func(string) string) ([]SkillTarget, error) {
	if env == nil {
		env = os.Getenv
	}
	dir := OpenCodeCommandsDir(project, env("XDG_CONFIG_HOME"), home)
	o := SkillOpts{Agent: "opencode", ToolPrefix: "punk_"}
	out := make([]SkillTarget, 0, len(CommandNames))
	for _, name := range CommandNames {
		co := o
		co.Command = name
		out = append(out, SkillTarget{Name: name, Path: filepath.Join(dir, name+".md"), Opts: co})
	}
	return out, nil
}

// RenderOpenCodeCommand renders one punk command as an OpenCode command
// file (see openCodeCommandTmpl): the description and body come from
// the same shared spec and CommandBody source the skill renderer uses;
// only the file wrapper is OpenCode's own.
func RenderOpenCodeCommand(o SkillOpts) string {
	c, ok := commandFor(o.Command)
	if !ok {
		return ""
	}
	body, _ := CommandBody(o.Command, o)
	var b strings.Builder
	_ = openCodeCommandTmpl.Execute(&b, map[string]any{
		"Spec":   c,
		"Opts":   o,
		"Body":   body,
		"Marker": SkillMarker,
	})
	return b.String()
}
