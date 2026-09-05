package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/hypervisor-io/punk-records/internal/authz"
)

// Task A02: namespace enforcement at every external memory boundary.
// A01 covers /v1/namespaces/{ns}/* via the path-prefix hook in
// enforceNamespace; this file covers what that hook cannot see:
// resolved-namespace routes (agent hooks/context), aggregate and
// cross-region surfaces (brain, region enumeration), long-lived
// streams (SSE, task-board waits, MCP subscriptions), and the whole
// HTTP MCP endpoint.
//
// Same trust model everywhere: the credential alone decides (the
// verified subject from the bearer key); namespace-selection inputs -
// query parameters, headers, explicit arguments, client roots - only
// pick the namespace a call targets. No authorizer wired
// (authz.enforcement off) keeps legacy trusted behavior.

// verifiedSubject returns the subject the auth middleware verified for
// this request. authMiddleware deletes any client-supplied
// X-Punk-Subject before verification and re-sets it from the key
// check, so the header reaching handlers is always server-set. The
// bootstrap subject is "": deny-by-default grants it nothing.
func verifiedSubject(r *http.Request) string {
	return r.Header.Get("X-Punk-Subject")
}

// keyIDHeader carries the row ID of the API key that authenticated the
// request - a safe, stable credential identity, never the token itself.
// Like X-Punk-Subject it is server-set only: authMiddleware deletes any
// client-supplied value before verification.
const keyIDHeader = "X-Punk-Key-ID"

// verifiedKeyID returns the row ID of the API key that authenticated
// this request, or 0 when no key did (bootstrap pass-through).
func verifiedKeyID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.Header.Get(keyIDHeader), 10, 64)
	return id
}

// authorizer returns the wired namespace authorizer, or nil when
// enforcement is off.
func (s *Server) authorizer() *authz.Authorizer {
	if s.keys == nil {
		return nil
	}
	return s.keys.az
}

// allowNS reports whether subject may perform op on ns. Without an
// authorizer every call is allowed (legacy trusted behavior).
func (s *Server) allowNS(ctx context.Context, subject, ns string, op authz.Op) bool {
	az := s.authorizer()
	if az == nil {
		return true
	}
	return az.Allow(ctx, subject, ns, op)
}

// credentialActive reports whether the key that authenticated a request
// is still valid. Streams and long-polls outlive the one credential
// check of their opening request, so deliveries revalidate: a key
// revoked mid-stream fails here. Without an authorizer
// (authz.enforcement off) always true - legacy trusted behavior; the
// same for requests authenticated without a key (bootstrap, where
// deny-by-default grants the empty subject nothing anyway).
func (s *Server) credentialActive(ctx context.Context, keyID int64) bool {
	if s.authorizer() == nil || keyID == 0 {
		return true
	}
	return s.keys.KeyActive(ctx, keyID)
}

// streamAllowed revalidates everything a delivery on a long-lived
// stream depends on: the credential that opened the stream is still
// valid, and its verified subject still holds op on ns. Both halves are
// rechecked before EVERY delivery, so revoking either the API key or
// the namespace grant stops subsequent deliveries.
func (s *Server) streamAllowed(ctx context.Context, keyID int64, subject, ns string, op authz.Op) bool {
	return s.credentialActive(ctx, keyID) && s.allowNS(ctx, subject, ns, op)
}

// denyNS writes the 403 body enforceNamespace uses, so every boundary
// speaks with one voice.
func denyNS(w http.ResponseWriter, ns string, op authz.Op) {
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": "namespace grant required: " + string(op) + " on " + ns,
	})
}

// authorizeResolved enforces op on a namespace resolved from request
// input (hook cwd, ns/cwd query parameters, ...): the input only picks
// the namespace, the verified subject decides access. True when
// allowed; on denial writes the 403 and returns false.
func (s *Server) authorizeResolved(w http.ResponseWriter, r *http.Request, ns string, op authz.Op) bool {
	if s.allowNS(r.Context(), verifiedSubject(r), ns, op) {
		return true
	}
	denyNS(w, ns, op)
	return false
}

// readableNamespaces keeps only the namespaces subject may read;
// without an authorizer the input passes through unchanged. Used by
// aggregate surfaces (brain snapshot/events) that enumerate many
// regions at once: unreadable regions are omitted, never denied
// wholesale.
func (s *Server) readableNamespaces(ctx context.Context, subject string, namespaces []string) []string {
	az := s.authorizer()
	if az == nil {
		return namespaces
	}
	kept := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		if az.Allow(ctx, subject, ns, authz.OpRead) {
			kept = append(kept, ns)
		}
	}
	return kept
}

// NamespaceGate decides namespace operations for one MCP session's
// verified credential. The HTTP boundary (mcpAuthz below) injects a gate
// bound to the API-key credential its auth middleware verified when the
// session connected into every /mcp request context when
// authz.enforcement is deny; the MCP server (internal/mcpserver)
// reauthorizes every namespaced tool, resource and subscription
// delivery through it.
//
// Absence of a gate is meaningful: the stdio server (punk mcp) and
// in-memory transports carry none and remain trusted local transports
// with the same authority as direct database access (see the
// internal/authz package docs). A gate is a server-set context value;
// clients cannot forge one, and namespace-selection inputs (explicit
// tool arguments, the X-Punk-Namespace header, client roots, the
// server default) only SELECT the namespace a call targets - they
// never grant access to it.
type NamespaceGate interface {
	Allow(ctx context.Context, namespace string, op authz.Op) bool
}

// SubjectGate is the optional interface for gates bound to a verified
// credential: it reports the subject the boundary verified. The MCP
// server uses it to bind a session's subscriptions to one verified
// subject, so a credential-changed reuse of the session cannot attach
// to (or rebind) another subject's subscriptions.
type SubjectGate interface {
	Subject() string
}

type namespaceGateKey struct{}

// ContextWithNamespaceGate binds a request's authorization gate. It is
// called by the HTTP boundary after credential verification, never by
// MCP clients.
func ContextWithNamespaceGate(ctx context.Context, g NamespaceGate) context.Context {
	return context.WithValue(ctx, namespaceGateKey{}, g)
}

// NamespaceGateFrom returns the gate bound to ctx, or nil when the
// transport is trusted (no gate injected).
func NamespaceGateFrom(ctx context.Context) NamespaceGate {
	g, _ := ctx.Value(namespaceGateKey{}).(NamespaceGate)
	return g
}

// mcpGate adapts the HTTP boundary to the MCP session gate: bound to
// the credential (key row ID + subject) the auth middleware verified
// when the session connected - the SDK derives every later request
// handler context from that connect request. Allow revalidates BOTH
// halves at every decision: a key revoked since the session connected
// fails here even when the subject's grants are unchanged (credential
// revocation stops subsequent stream deliveries), and a revoked grant
// fails as before.
type mcpGate struct {
	az      *authz.Authorizer
	keys    *Keys
	keyID   int64
	subject string
}

func (g mcpGate) Allow(ctx context.Context, namespace string, op authz.Op) bool {
	if g.keyID != 0 && !g.keys.KeyActive(ctx, g.keyID) {
		return false // credential revoked since the session connected
	}
	return g.az.Allow(ctx, g.subject, namespace, op)
}

// Subject reports the verified subject this gate is bound to.
func (g mcpGate) Subject() string { return g.subject }

// mcpTokenInfo is the SDK bearer verifier for the /mcp boundary. Punk
// keys carry no expiration; validity is exactly the key check, so an
// unknown or revoked key fails with auth.ErrInvalidToken. UserID is the
// verified subject: the streamable-HTTP transport records it with the
// session it creates and rejects any later request that reuses the
// session ID under a different UserID, which is what stops a
// credential-changed reuse of an MCP session from attaching to another
// subject's subscriptions. The raw token is never stored or forwarded;
// Keys.Check sees it only as its SHA-256 hash.
func (s *Server) mcpTokenInfo(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	valid, keyID, subject, err := s.keys.Check(ctx, token)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, fmt.Errorf("%w: unknown or revoked api key", auth.ErrInvalidToken)
	}
	return &auth.TokenInfo{UserID: subject, Extra: map[string]any{"punk_key_id": keyID}}, nil
}

// mcpAuthz injects the gate into every /mcp request context after
// authMiddleware has verified the bearer key, and binds MCP sessions to
// their creating credential: credential-carrying requests additionally
// pass the SDK bearer middleware, whose TokenInfo.UserID makes the
// streamable-HTTP transport answer 403 to any request reusing a session
// ID under a different verified subject (session-hijacking protection).
// Mounted only on the HTTP transport; the stdio server (punk mcp)
// carries no gate and stays a trusted local transport.
func (s *Server) mcpAuthz(next http.Handler) http.Handler {
	bound := auth.RequireBearerToken(s.mcpTokenInfo, &auth.RequireBearerTokenOptions{
		AllowMissingExpiration: true, // punk keys do not expire; revocation is the validity signal
	})(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		az := s.authorizer()
		if az == nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx := ContextWithNamespaceGate(r.Context(), mcpGate{
			az:      az,
			keys:    s.keys,
			keyID:   verifiedKeyID(r),
			subject: verifiedSubject(r),
		})
		if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && token != "" {
			bound.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		// Bootstrap pass-through (zero active keys): no credential to
		// bind a session to; deny-by-default grants the empty subject
		// nothing.
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
