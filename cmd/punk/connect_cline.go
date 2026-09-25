package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

// Cline's native file-hook locations and MCP path were verified 2026-09-25
// in apps/vscode/src/core/hooks/utils.ts and mcp-settings-legacy-migration.ts.
func cmdConnectCline(args []string) error {
	fs := flag.NewFlagSet("connect cline", flag.ContinueOnError)
	project := fs.Bool("project", false, "write .clinerules/hooks in this workspace instead of ~/Documents/Cline/Hooks")
	hooksDir := fs.String("hooks-dir", "", "override native hook directory")
	mcpPath := fs.String("mcp-settings", "", "override Cline MCP settings path (else CLINE_MCP_SETTINGS_PATH / CLINE_DATA_DIR / CLINE_DIR)")
	urlFlag := fs.String("url", "", "punk server URL (else PUNK_URL or saved credentials)")
	messaging := fs.Bool("messaging", false, "inject agent inbox on TaskStart/UserPromptSubmit; no idle wake")
	ns := fs.String("ns", "", "pin hook namespace; default derived from workspace")
	noMCP := fs.Bool("no-mcp", false, "install only file hooks")
	force := fs.Bool("force", false, "replace a foreign MCP punk entry; never overwrites user hook files")
	keyEnv := fs.String("api-key-env", "", "MCP Authorization env name, written as ${env:NAME}")
	agent := fs.String("agent", defaultAgentName(), "MCP identity; inbox address remains cline:<taskId>")
	if err := fs.Parse(args); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := *hooksDir
	if dir == "" {
		if *project {
			dir = filepath.Join(".clinerules", "hooks")
		} else {
			dir = filepath.Join(home, "Documents", "Cline", "Hooks")
		}
	}
	pin := *ns
	if pin == "" && *project {
		pin, _ = hookcli.ProjectNamespace(".")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	server, key := hookcli.ResolveServer(*urlFlag)
	changed, err := hookcli.ConnectCline(dir, hookcli.ClineConnectOpts{PunkPath: exe, ServerURL: server, Namespace: pin, Messaging: *messaging})
	if err != nil {
		return fmt.Errorf("connect cline: %w", err)
	}
	fmt.Printf("punk: Cline file hooks in %s (%s)\n", dir, changedWord(changed))
	if !*noMCP {
		path := *mcpPath
		if path == "" {
			path = hookcli.ClineMCPSettingsPath(home)
		}
		// Cline's settings are global even with workspace hooks. Do not pin
		// a project namespace globally and misroute every other VS Code window.
		changed, err := hookcli.ConnectClineMCP(path, hookcli.MCPEntryOpts{ServerURL: server, APIKey: key, APIKeyEnv: *keyEnv, Agent: *agent}, *force)
		if err != nil {
			return fmt.Errorf("connect cline MCP: %w", err)
		}
		fmt.Printf("punk: Cline MCP entry in %s (%s)\n", path, changedWord(changed))
		if key != "" && *keyEnv == "" {
			fmt.Printf("punk: API key stored in %s; keep file private\n", path)
		}
	}
	fmt.Println("punk: Cline >=4.1.20 required for contextModification; enable hooks in Cline settings. Existing Unix execute permissions preserved.")
	fmt.Println("punk: inbox catch-up only on TaskStart and UserPromptSubmit; no continuation or idle wake. Avoid installing both global and project hooks for the same workspace.")
	return nil
}
