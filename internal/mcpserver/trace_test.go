package mcpserver

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestMCPCallsJoinIncomingTrace(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	cs := session(t)
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_keys",
		Arguments: map[string]any{"namespace": "ns"},
		Meta:      mcp.Meta{"traceparent": "00-" + traceID + "-00f067aa0ba902b7-01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range exp.GetSpans() {
		if s.Name == "mcp.tools/call" && s.SpanContext.TraceID().String() == traceID {
			found = true
		}
	}
	if !found {
		names := []string{}
		for _, s := range exp.GetSpans() {
			names = append(names, s.Name+"/"+s.SpanContext.TraceID().String())
		}
		t.Fatalf("no mcp.tools/call span under the incoming trace; spans: %v", names)
	}
}
