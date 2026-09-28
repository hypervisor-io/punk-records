//go:build unix

package nativewake

import (
	"os/exec"
	"syscall"
)

// codexSetProcAttr isolates the proxy subprocess in its own process
// group. The proxy may fork helpers that inherit its stdio pipe; a group
// kill then takes the whole tree down instead of leaving pipe holders
// alive.
func codexSetProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// codexKill kills the proxy's whole process group, falling back to the
// direct process when group signaling is unavailable.
func codexKill(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
