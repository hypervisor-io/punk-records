package hookcli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Shell environment: punk connect and punk login export the resolved
// PUNK_URL and PUNK_API_KEY to new shells, so CLI hooks, `punk` commands
// and clients launched from a terminal reach the same server with the
// same key. The values go in a private env file next to the credentials
// file (~/.punk/env, mode 0600); each shell rc file gets a marked block
// that only sources it, so the key itself never lands in .bashrc, which
// is often world-readable or kept in a dotfiles repository.

const (
	shellEnvHeader   = "# managed by punk connect / punk login; rewritten on every run\n"
	shellBlockBegin  = "# >>> punk >>>"
	shellBlockEnd    = "# <<< punk <<<"
	shellBlockSource = `[ -f "%[1]s" ] && . "%[1]s"`
)

// ShellEnvPath is the private env file the rc blocks source: env next to
// the credentials file (PUNK_CREDENTIALS's directory when that is set).
func ShellEnvPath() string {
	return filepath.Join(filepath.Dir(CredentialsPath()), "env")
}

// shellQuote single-quotes s for POSIX sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellEnvContent renders the env file: PUNK_URL always, PUNK_API_KEY
// only when there is a key (an unauthenticated server needs none).
func ShellEnvContent(serverURL, apiKey string) string {
	var b strings.Builder
	b.WriteString(shellEnvHeader)
	fmt.Fprintf(&b, "export PUNK_URL=%s\n", shellQuote(serverURL))
	if apiKey != "" {
		fmt.Fprintf(&b, "export PUNK_API_KEY=%s\n", shellQuote(apiKey))
	}
	return b.String()
}

// ShellRCFiles are the rc files that get the source block: ~/.bashrc
// always (created if missing), ~/.zshrc only when it already exists.
func ShellRCFiles(home string) []string {
	files := []string{filepath.Join(home, ".bashrc")}
	if _, err := os.Stat(filepath.Join(home, ".zshrc")); err == nil {
		files = append(files, filepath.Join(home, ".zshrc"))
	}
	return files
}

// ShellEnvResult says what WriteShellEnv changed.
type ShellEnvResult struct {
	EnvPath    string
	EnvChanged bool
	RCChanged  []string // rc files that gained the source block
}

// WriteShellEnv writes the private env file and makes sure every rc file
// sources it. Idempotent: unchanged content is not rewritten, and an rc
// file that already has the punk block keeps it as is.
func WriteShellEnv(envPath string, rcFiles []string, serverURL, apiKey string) (ShellEnvResult, error) {
	res := ShellEnvResult{EnvPath: envPath}
	content := []byte(ShellEnvContent(serverURL, apiKey))
	existing, err := os.ReadFile(envPath)
	if err != nil && !os.IsNotExist(err) {
		return res, fmt.Errorf("read %s: %w", envPath, err)
	}
	if err == nil && !bytes.HasPrefix(existing, []byte(shellEnvHeader)) {
		return res, fmt.Errorf("%s exists and is not managed by punk; refusing to overwrite", envPath)
	}
	if !bytes.Equal(existing, content) || privateModeNeedsRepair(envPath) {
		if err := writePrivatePreservingSymlinkAndMode(envPath, content); err != nil {
			return res, err
		}
		res.EnvChanged = true
	}
	block := shellBlockBegin + "\n" + fmt.Sprintf(shellBlockSource, envPath) + "\n" + shellBlockEnd + "\n"
	for _, rc := range rcFiles {
		raw, err := os.ReadFile(rc)
		if err != nil && !os.IsNotExist(err) {
			return res, fmt.Errorf("read %s: %w", rc, err)
		}
		if bytes.Contains(raw, []byte(shellBlockBegin)) {
			continue
		}
		var out []byte
		out = append(out, raw...)
		if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
			out = append(out, '\n')
		}
		if len(out) > 0 {
			out = append(out, '\n')
		}
		out = append(out, block...)
		if err := writePreservingSymlinkAndMode(rc, out, 0o644); err != nil {
			return res, err
		}
		res.RCChanged = append(res.RCChanged, rc)
	}
	return res, nil
}
