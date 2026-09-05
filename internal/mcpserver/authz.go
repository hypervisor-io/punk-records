package mcpserver

import (
	"context"
	"fmt"

	"github.com/hypervisor-io/punk-records/internal/api"
	"github.com/hypervisor-io/punk-records/internal/authz"
)

// Task A02: namespace enforcement for the MCP surface. The gate type
// and its context key live in internal/api - the HTTP boundary that
// verifies the bearer key and injects a per-request gate bound to the
// verified subject (see internal/api/authz_boundary.go). This package
// reads that gate and enforces it at every namespaced tool, resource
// and subscription delivery.
//
// Trust model: a missing gate means a trusted local transport (the
// stdio server started by punk mcp, in-memory sessions) or
// enforcement off - both allow. With a gate the policy is
// deny-by-default: only an active grant for the verified subject
// passes. Namespace-selection inputs (explicit tool arguments, the
// X-Punk-Namespace header, client roots, the server default) only
// SELECT the namespace a call targets; the grant decision comes from
// the credential alone.

// NamespaceGate is the per-request authorization gate the HTTP
// boundary injects; alias of the internal/api type so this package's
// handlers speak the boundary's interface.
type NamespaceGate = api.NamespaceGate

// namespaceGateFrom returns the gate bound to the request context, or
// nil on trusted transports.
func namespaceGateFrom(ctx context.Context) NamespaceGate {
	return api.NamespaceGateFrom(ctx)
}

// gateSubject returns the verified subject a gate is bound to, or ""
// when the gate carries none. The subscription registry uses it to bind
// a session's subscriptions to one verified subject (see
// api.SubjectGate).
func gateSubject(g NamespaceGate) string {
	if g == nil {
		return ""
	}
	if sg, ok := g.(api.SubjectGate); ok {
		return sg.Subject()
	}
	return ""
}

// authorizeNS enforces op on the final resolved namespace ns. A
// missing gate (trusted transport, or enforcement off) allows; with a
// gate the policy is deny-by-default: only an active grant for the
// verified subject passes.
func authorizeNS(ctx context.Context, ns string, op authz.Op) error {
	g := namespaceGateFrom(ctx)
	if g == nil {
		return nil
	}
	if g.Allow(ctx, ns, op) {
		return nil
	}
	return fmt.Errorf("namespace grant required: %s on %s", op, ns)
}
