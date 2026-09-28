//go:build unix

package nativewake

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// claudeDial validates the socket path and opens one unix connection. It
// never leaks the path: validation and dial failures collapse to
// genericUnavailable (definite no target) or a static generic error;
// context errors (path-free) pass through.
//
// Validation insists on an absolute path that Lstat identifies as an
// actual socket. Lstat (not Stat) is deliberate: a symlink is refused
// rather than followed, because the token is written to whatever the path
// resolves to and a swapped symlink would hand it to an attacker endpoint.
func claudeDial(ctx context.Context, socket string) (net.Conn, error) {
	if socket == "" || !filepath.IsAbs(socket) {
		return nil, genericUnavailable()
	}
	fi, err := os.Lstat(socket)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// Missing (or unreadable) path: no live target.
		return nil, genericUnavailable()
	}
	if fi.Mode()&os.ModeSymlink != 0 || fi.Mode()&os.ModeSocket == 0 {
		return nil, genericUnavailable()
	}
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Err != nil {
			switch {
			case errors.Is(opErr.Err, syscall.ECONNREFUSED), errors.Is(opErr.Err, syscall.ENOENT):
				// Stale socket file or vanished listener: definite
				// no target.
				return nil, genericUnavailable()
			}
		}
		// Timeouts and everything else are ambiguous (the session may be
		// alive but congested): generic retryable failure, no path leak.
		return nil, errors.New("nativewake: claude wake dial failed")
	}
	return conn, nil
}
