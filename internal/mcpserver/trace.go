package mcpserver

import (
	"context"
	"reflect"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/propagation"

	"github.com/hypervisor-io/punk-records/internal/obs"
)

// traceMetaKeys are the W3C trace-context headers a client may put in a
// request's _meta so a punk span joins the caller's trace.
var traceMetaKeys = []string{"traceparent", "tracestate", "baggage"}

// metaOf returns the request's _meta, or nil. Methods called without
// params (tools/list, ping) reach the middleware with a typed-nil
// pointer inside the Params interface, so the nil check has to look
// through the interface before GetMeta may be called.
func metaOf(req mcp.Request) map[string]any {
	if req == nil {
		return nil
	}
	p := req.GetParams()
	if p == nil {
		return nil
	}
	if rv := reflect.ValueOf(p); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return nil
	}
	return p.GetMeta()
}

// traceMiddleware opens one span per incoming MCP method. When _meta
// carries a valid traceparent the span is a child of it; otherwise it is
// a root. Invalid headers are ignored by the propagator, so a bad client
// value never fails the call.
func traceMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if meta := metaOf(req); len(meta) > 0 {
			carrier := propagation.MapCarrier{}
			for _, k := range traceMetaKeys {
				if v, ok := meta[k].(string); ok && v != "" {
					carrier[k] = v
				}
			}
			if len(carrier) > 0 {
				ctx = propagation.TraceContext{}.Extract(ctx, carrier)
			}
		}
		ctx, span := obs.Tracer().Start(ctx, "mcp."+method)
		defer span.End()
		return next(ctx, method, req)
	}
}
