//go:build !unix

package hookcli

import (
	"errors"
	"os"
	"time"
)

// inboxLockStale is how old a lock file must be before a waiter treats
// its holder as crashed. Hook state updates take milliseconds, so this
// is far past any live holder.
const inboxLockStale = 10 * time.Second

// lockInboxFile is the portable fallback (Windows, plan9, wasm): an
// exclusively created lock file. O_EXCL creation is atomic on every
// supported filesystem, so exactly one process holds it; a lock file
// older than inboxLockStale is presumed abandoned by a crashed hook and
// removed.
func lockInboxFile(path string, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > inboxLockStale {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, errInboxLockTimeout
		}
		time.Sleep(5 * time.Millisecond)
	}
}
