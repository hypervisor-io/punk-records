package obs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// TracerName is the instrumentation scope for every Punk Records span.
const TracerName = "punk-records"

// Tracer returns the process tracer (noop until SetupTracing installs a
// provider, so call sites never branch).
func Tracer() trace.Tracer { return otel.Tracer(TracerName) }

// SetupTracing installs an OTLP/HTTP trace provider when endpoint is set.
// Empty endpoint leaves the default noop provider: spans cost nothing.
// GenAI semconv attribute names are used at call sites; the agent-span
// conventions are still "Development" status upstream, expect churn.
func SetupTracing(ctx context.Context, endpoint, service string) (func(context.Context) error, error) {
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, err
	}
	res := resource.NewSchemaless(semconv.ServiceName(service))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}
