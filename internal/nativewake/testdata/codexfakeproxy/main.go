// Command codexfakeproxy is a test double for `codex app-server proxy`
// (codex-cli 0.157.1, stdio-to-uds/src/lib.rs): a DUMB raw byte relay
// between its stdio and a Unix socket. It adds no protocol, so anything
// a client speaks on its stdio arrives at the socket byte for byte -
// which is exactly why the production transport must carry WebSocket
// framing itself.
//
// Behavior contract shared with the real proxy:
//
//   - argv: app-server proxy [--sock <path>]; with no --sock it uses the
//     default control socket path, which tests supply through
//     PUNK_FAKE_PROXY_SOCK.
//   - a missing socket makes it exit 1 with NO stdout, exactly like the
//     real proxy ("failed to connect to socket ... No such file or
//     directory").
//
// Bookkeeping (for assertions) goes to $PUNK_FAKE_PROXY_STATE when set:
// pids (appended), argv (last start), starts (count). It lives under
// testdata so the go tool ignores it during normal builds.
package main

import (
	"io"
	"net"
	"os"
	"strconv"
	"strings"
)

func main() {
	args := os.Args[1:]
	sock := ""
	if len(args) >= 2 && args[0] == "app-server" && args[1] == "proxy" {
		for i := 2; i+1 < len(args); i++ {
			if args[i] == "--sock" {
				sock = args[i+1]
			}
		}
	}
	if sock == "" {
		sock = os.Getenv("PUNK_FAKE_PROXY_SOCK")
	}
	recordState(args)
	if sock == "" {
		os.Exit(2)
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		// Real-proxy parity: no stdout, non-zero exit.
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()
	go func() {
		_, _ = io.Copy(conn, os.Stdin)
		_ = conn.Close()
	}()
	_, _ = io.Copy(os.Stdout, conn)
}

func recordState(args []string) {
	dir := os.Getenv("PUNK_FAKE_PROXY_STATE")
	if dir == "" {
		return
	}
	appendFile(dir+"/pids", strconv.Itoa(os.Getpid())+"\n")
	writeFile(dir+"/argv", strings.Join(args, " ")+"\n")
	n := 0
	if b, err := os.ReadFile(dir + "/starts"); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	writeFile(dir+"/starts", strconv.Itoa(n+1)+"\n")
}

func appendFile(path, body string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(body)
}

func writeFile(path, body string) {
	_ = os.WriteFile(path, []byte(body), 0o644)
}
