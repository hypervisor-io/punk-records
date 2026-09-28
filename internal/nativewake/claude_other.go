//go:build !unix

package nativewake

import (
	"context"
	"errors"
	"net"
)

// claudeDial is the non-Unix fallback: Claude Code own-session inbox
// sockets are unix domain sockets, so this platform has no supported
// target. The failure is explicit (fail open, never silently start or
// guess another endpoint) and carries no path.
func claudeDial(context.Context, string) (net.Conn, error) {
	return nil, errors.New("nativewake: claude wake unsupported on this platform")
}
