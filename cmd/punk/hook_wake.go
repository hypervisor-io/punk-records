package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

// cmdHookWake runs "punk hook wake": the lifecycle of the opt-in native
// wake listener (see hookcli.Wake and internal/hookcli/wake_connect.go
// for the generated entries). --action ensure rechecks the session's
// native endpoint and guarantees one detached listener, --action stop
// (SessionEnd) tears it down, and --action run is the detached listener
// itself, reading its private config JSON from stdin. Like cmdHook and
// cmdHookInbox it always exits 0 and prints nothing to stdout: a hook
// must never break or hang the host session.
func cmdHookWake(args []string) error {
	fs := flag.NewFlagSet("hook wake", flag.ContinueOnError)
	urlFlag := fs.String("url", "", "punk-records base URL (default $PUNK_URL, saved credentials, or http://localhost:9090)")
	client := fs.String("client", "", "client whose native hook payload is on stdin (claude-code, codex)")
	action := fs.String("action", "", "ensure | stop | run")
	nsFlag := fs.String("ns", "", "namespace override (else $PUNK_NAMESPACE, else derived from the payload cwd)")
	messaging := fs.Bool("messaging", false, "enable wake without PUNK_MESSAGING=1 (PUNK_MESSAGING=0 still disables and tears down)")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		// Fail open: a mistyped hook entry must never break the session.
		fmt.Fprintln(os.Stderr, "punk hook wake:", err)
		return nil
	}
	if *nsFlag != "" {
		hookcli.SetNamespaceOverride(*nsFlag)
	}
	baseURL, apiKey, err := resolveServerForCommand(*urlFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "punk hook wake:", err)
		return nil
	}
	return hookcli.Wake(hookcli.WakeOpts{
		Client: *client, Action: *action, BaseURL: baseURL, APIKey: apiKey,
		Namespace: *nsFlag, Enabled: *messaging,
	}, os.Stdin, os.Stderr)
}
