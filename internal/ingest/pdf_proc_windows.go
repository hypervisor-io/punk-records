//go:build windows

package ingest

import "os/exec"

// configureAdapterCancel cancels the adapter by killing the direct process
// only. Windows has no process-group construct used here, so there is no
// group to signal (no Setpgid / syscall.Kill): an adapter that spawns a
// grandchild cannot be reaped via a group. Instead cmd.WaitDelay - installed
// portably in pdf.go - is what bounds cmd.Wait on an inherited pipe still
// held open by a descendant that the direct-process kill does not reach.
func configureAdapterCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		p := cmd.Process
		if p == nil {
			return nil
		}
		return p.Kill()
	}
}
