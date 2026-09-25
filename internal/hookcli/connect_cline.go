package hookcli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const clineHookMarker = "# punk-managed: cline-hook v1\n"

// Only capture-supported events are installed. No pre-tool veto hook, no
// notification hook (Cline ignores its context), and no synthetic Stop event.
var clineHookEvents = []string{"TaskStart", "TaskResume", "UserPromptSubmit", "PostToolUse", "TaskComplete", "TaskCancel"}

type ClineConnectOpts struct {
	PunkPath  string
	ServerURL string
	Namespace string
	Messaging bool
	GOOS      string // empty = current platform; explicit for cross-platform generation tests
}

// ConnectCline writes native file hooks, not JSON hook config. Every target
// is preflighted before any write; a foreign file/symlink is never replaced.
// Existing modes are preserved because chmod is Cline's Unix enable switch.
func ConnectCline(hooksDir string, o ClineConnectOpts) (bool, error) {
	if o.PunkPath == "" {
		return false, fmt.Errorf("cline: punk executable required")
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	body := []byte(clineHookScript(o))
	type pending struct {
		path string
		mode os.FileMode
		data []byte
	}
	var writes []pending
	for _, ev := range clineHookEvents {
		name := ev
		if o.GOOS == "windows" {
			name += ".ps1"
		}
		path := filepath.Join(hooksDir, name)
		mode := os.FileMode(0755)
		if o.GOOS == "windows" {
			mode = 0644
		}
		info, err := os.Lstat(path)
		if err != nil && !os.IsNotExist(err) {
			return false, err
		}
		var existing []byte
		if err == nil {
			if !info.Mode().IsRegular() {
				return false, fmt.Errorf("cline: refusing non-regular hook %s", path)
			}
			existing, err = os.ReadFile(path)
			if err != nil {
				return false, err
			}
			prefix := clineHookMarker
			if o.GOOS != "windows" {
				prefix = "#!/bin/sh\n" + prefix
			}
			if !bytes.HasPrefix(existing, []byte(prefix)) {
				return false, fmt.Errorf("cline: refusing to overwrite user hook %s; compose manually or choose another hook scope", path)
			}
			mode = info.Mode().Perm()
		}
		if !bytes.Equal(existing, body) {
			writes = append(writes, pending{path, mode, body})
		}
	}
	for _, w := range writes {
		if err := writeAtomic(w.path, w.data, w.mode); err != nil {
			return false, err
		}
	}
	return len(writes) > 0, nil
}

func clineHookScript(o ClineConnectOpts) string {
	args := []string{"hook", "--from", "cline", "--url", o.ServerURL}
	if o.Namespace != "" {
		args = append(args, "--ns", o.Namespace)
	}
	if o.Messaging {
		args = append(args, "--messaging")
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	if o.GOOS == "windows" {
		quote = func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
		for i := range args {
			args[i] = quote(args[i])
		}
		// Explicit UTF-8 for native stdin preserves non-ASCII task text in
		// Windows PowerShell 5.1; direct invocation yields one JSON reply.
		return clineHookMarker + "$OutputEncoding = [Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)\n$raw = [Console]::In.ReadToEnd()\n$raw | & " + quote(o.PunkPath) + " " + strings.Join(args, " ") + "\nexit 0\n"
	}
	// Static words stay readable; operator-supplied strings always quoted.
	cmd := quote(o.PunkPath) + " hook --from cline --url " + quote(o.ServerURL)
	if o.Namespace != "" {
		cmd += " --ns " + quote(o.Namespace)
	}
	if o.Messaging {
		cmd += " --messaging"
	}
	return "#!/bin/sh\n" + clineHookMarker + "exec " + cmd + "\n"
}

// ClineMCPSettingsPath follows getSharedMcpSettingsPath in current upstream.
func ClineMCPSettingsPath(home string) string {
	if p := strings.TrimSpace(os.Getenv("CLINE_MCP_SETTINGS_PATH")); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("CLINE_DATA_DIR")); p != "" {
		return filepath.Join(p, "settings", "cline_mcp_settings.json")
	}
	dir := strings.TrimSpace(os.Getenv("CLINE_DIR"))
	if dir == "" {
		dir = filepath.Join(home, ".cline")
	}
	return filepath.Join(dir, "data", "settings", "cline_mcp_settings.json")
}
