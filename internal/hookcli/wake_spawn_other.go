//go:build !unix

package hookcli

import (
	"errors"
)

// errWakeUnsupported marks platforms without the Unix detachment
// primitives the wake lifecycle needs (session/process-group spawn, a
// config pipe the hook can close, pid liveness). The spec's fail-open
// rule applies: ensure reports one concise line and starts nothing
// rather than falling back to a half-detached child that could keep a
// native hook alive.
var errWakeUnsupported = errors.New("native wake lifecycle is unsupported on this platform (Unix only)")

// wakeSpawnProcess refuses on non-Unix platforms.
func wakeSpawnProcess(argv []string, config []byte, logPath, workDir string) (int, error) {
	return 0, errWakeUnsupported
}

// wakePidAlive reports false on platforms without signal-0 liveness, so
// a matching marker is never mistaken for a live worker.
func wakePidAlive(pid int) bool {
	return false
}
