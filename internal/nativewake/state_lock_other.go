//go:build !unix

package nativewake

import (
	"fmt"
	"os"
	"path/filepath"
)

// acquireStateLock is the non-Unix fallback: an exclusive-create lock
// file. A crashed worker can leave the file behind; the lifecycle layer
// (W4) owns StateDir cleanup on stop, which removes it. Unix builds use
// the fd-held flock, which cannot go stale.
func acquireStateLock(dir string) (release func() error, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	path := filepath.Join(dir, wakeLockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("%w by another wake worker", errStateLocked)
		}
		return nil, fmt.Errorf("state lock: %w", err)
	}
	_ = f.Close()
	return func() error { return os.Remove(path) }, nil
}
