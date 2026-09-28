//go:build unix

package hookcli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// errWakeConfigPipe is the generic spawn-failure reason; the underlying
// write error can name the child binary path, which stays out of logs.
var errWakeConfigPipe = errors.New("worker did not accept the config pipe")

// wakeSpawnProcess starts the wake worker fully detached: its own
// session and process group (Setsid), stdin as a short-lived pipe that
// is closed right after the private config write, stdout discarded and
// stderr truncated at spawn into the per-generation log (bounded: one
// worker's concise lines, restarted fresh each generation), and workDir
// as the child's working directory so a Codex proxy subprocess never
// inherits the user's workspace or home. No inherited pipe can keep the
// native hook alive.
//
// On any config-pipe failure - write error, short write, or a child
// that never drains the pipe within wakeConfigWriteTimeout - the child
// is killed AND reaped (Wait), and every pipe is closed: a failed spawn
// leaves no zombie and no fd behind. On success the process is
// released, not waited on: the hook exits promptly and init reaps the
// worker when it eventually ends.
func wakeSpawnProcess(argv []string, config []byte, logPath, workDir string) (int, error) {
	if len(argv) == 0 {
		return 0, errors.New("empty worker command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Dir = workDir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		_ = stdin.Close()
		return 0, err
	}
	defer func() { _ = devnull.Close() }()
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_ = stdin.Close()
		return 0, fmt.Errorf("worker log: %w", err)
	}
	defer func() { _ = logf.Close() }()
	cmd.Stdout = devnull
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return 0, err
	}
	type writeResult struct {
		n   int
		err error
	}
	done := make(chan writeResult, 1)
	go func() {
		n, werr := stdin.Write(config)
		done <- writeResult{n, werr}
	}()
	var w writeResult
	select {
	case w = <-done:
	case <-time.After(wakeConfigWriteTimeout):
		w = writeResult{0, errWakeConfigPipe}
	}
	_ = stdin.Close()
	if w.err != nil || w.n != len(config) {
		// The child may never have read the config: kill it and reap it
		// so a failed spawn leaves neither a zombie nor a live worker
		// running with a truncated config.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 0, errWakeConfigPipe
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// wakePidAlive reports whether pid plausibly exists. It is used ONLY to
// decide whether a matching-fingerprint worker needs respawning; no PID
// is ever signalled for teardown (the control marker does that), so PID
// reuse cannot kill an unrelated process.
func wakePidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
