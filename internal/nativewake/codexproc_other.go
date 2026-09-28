//go:build !unix

package nativewake

import "os/exec"

// codexSetProcAttr is the non-Unix fallback: no process-group isolation
// exists here, teardown falls back to killing the proxy process itself.
func codexSetProcAttr(*exec.Cmd) {}

// codexKill kills only the proxy process on platforms without process
// groups.
func codexKill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
