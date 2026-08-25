package obs

import (
	"context"
	"testing"
)

func TestSetupTracingNoop(t *testing.T) {
	shutdown, err := SetupTracing(context.Background(), "", "punk-records")
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// tracer is usable either way
	_, span := Tracer().Start(context.Background(), "test.span")
	span.End()
}

func TestSetupTracingWithEndpoint(t *testing.T) {
	shutdown, err := SetupTracing(context.Background(), "http://localhost:4318", "punk-records")
	if err != nil {
		t.Fatal(err)
	}
	// no collector running: shutdown may flush-fail; only lifecycle matters
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = shutdown(ctx)
}
