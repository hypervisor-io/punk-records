//go:build unix

package nativewake

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireStateLock holds an advisory exclusive lock on the state dir for
// the caller's whole lifetime. Two wake workers sharing one StateDir
// (overlapping lifecycle generations) must never read-check-reserve the
// quota concurrently: the second Run fails immediately instead of
// double-spending. The lock is released by closing the fd, so a crashed
// worker never leaves a stale lock behind.
func acquireStateLock(dir string) (release func() error, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, wakeLockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w by another wake worker: %v", errStateLocked, err)
	}
	return f.Close, nil
}
