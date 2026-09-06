//go:build unix

package ingest

import (
	"os/exec"
	"syscall"
)

// configureAdapterCancel runs the adapter in its own process group and makes
// context cancellation signal the whole group with SIGKILL, covering both the
// group leader and any descendant that reparented out of the group (the
// group signal, then the pid as fallback).
//
// Group SIGKILL signals every member still in the group, but it does not
// reap the process nor kill descendants that already escaped the group by
// reparenting. cmd.WaitDelay - installed portably in pdf.go - is the
// mechanism that bounds cmd.Wait on an inherited pipe still held open by an
// escaped descendant.
func configureAdapterCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		p := cmd.Process
		if p == nil {
			return nil
		}
		if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err == nil {
			return nil
		}
		return p.Kill()
	}
}
