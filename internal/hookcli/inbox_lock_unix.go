//go:build unix

package hookcli

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// lockInboxFile takes an exclusive flock on path, retrying a
// non-blocking attempt until timeout so a wedged peer can never hang a
// hook. The kernel drops the lock when the process dies, so a crashed
// hook never leaves the state locked.
func lockInboxFile(path string, timeout time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, errInboxLockTimeout
		}
		time.Sleep(5 * time.Millisecond)
	}
}
