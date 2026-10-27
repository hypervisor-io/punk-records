package main

import (
	"log/slog"
	"net"
	"strings"
)

// P3-F: a startup warning for an open HTTP boundary. A non-loopback
// http.addr together with zero API keys means bootstrap pass-through:
// until the first key exists, every request passes, so anyone who can
// reach the host can read and write the whole server. The serve path
// warns loudly before the listener starts and keeps starting - never a
// refusal, and authorization behavior is never changed here (enforcement
// stays off unless the user enables it; user decision 2026-10-27, open
// joining is required).

// shouldWarnBind is the pure decision the serve path and the tests
// share: warn only when the configured listen address is non-loopback
// AND zero API keys exist. Loopback binds never warn regardless of
// keys, and a non-loopback bind with at least one active key never
// warns.
func shouldWarnBind(addr string, keyCount int) bool {
	return keyCount == 0 && !isLoopbackBind(addr)
}

// isLoopbackBind reports whether addr listens on loopback only.
//
// The host is net.SplitHostPort's host part: "" (":9090") means all
// interfaces, as do the all-interfaces addresses "0.0.0.0" and "[::]"
// - all non-loopback and therefore warned. "localhost" (case-
// insensitive) counts as loopback, as do 127.0.0.1, ::1 and the
// IPv4-mapped ::ffff:127.0.0.1. Any other hostname is treated as
// non-loopback: no DNS resolution at startup, and the warning errs on
// the loud side rather than silently assuming a name is local.
func isLoopbackBind(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "" {
		return false // ":port" binds every interface
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false // unknown hostname: assume reachable, warn
}

// warnOpenBind logs the P3-F banner when shouldWarnBind says to. One
// slog.Warn record per line, so the whole block carries level=WARN in
// text output and stays greppable in json too; silence otherwise.
func warnOpenBind(log *slog.Logger, addr string, keyCount int) {
	if !shouldWarnBind(addr, keyCount) {
		return
	}
	for _, line := range openBindWarningLines(addr) {
		log.Warn(line)
	}
}

// openBindWarningLines renders the banner. The headline carries the
// configured address and the zero-key fact - the exact line the tests
// capture; the rest explains the exposure and the two ways out.
func openBindWarningLines(addr string) []string {
	loopback := `127.0.0.1:9090`
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		loopback = `127.0.0.1:` + port
	}
	return []string{
		"!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!",
		"OPEN HTTP BIND: " + addr + " has ZERO API keys",
		"The HTTP API will listen on a non-loopback address, reachable from other machines,",
		"and with no API key in the database it runs in bootstrap pass-through:",
		"unauthenticated requests pass - anyone who can reach this host can read and write it.",
		"Create a key now:       punk apikey create --name ops",
		`Or bind loopback only:  http.addr: "` + loopback + `"`,
		"Warning only - punk starts anyway, exactly as configured.",
		"!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!",
	}
}
