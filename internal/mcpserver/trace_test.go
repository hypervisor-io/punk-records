package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// Over HTTP the SDK hands the middleware a typed-nil params pointer for
// methods called without params (tools/list from a real client); the
// middleware must not dereference it.
func TestTraceMiddlewareSurvivesNilParamsOverHTTP(t *testing.T) {
	srv := newTestServerForHTTP(t)
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer ts.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list with nil params: %v", err)
	}
	if len(res.Tools) == 0 {
		t.Fatal("no tools listed")
	}
}
