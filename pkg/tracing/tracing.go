package tracing

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.7.0"
	"google.golang.org/grpc"
)

// SetupTracer is a helper method for registering and starting a new TracerProvider and Propagator.
// The returned TracerProvider should be shutdown with .Shutdown()
func SetupTracer(ctx context.Context, opts ...sdktrace.TracerProviderOption) (*sdktrace.TracerProvider, error) {
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)

	prp := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
	otel.SetTextMapPropagator(prp)

	return tp, nil
}

// NewOTLPExporter returns an OTEL compatible exporter given a grpc endpoint
func NewOTLPExporter(ctx context.Context, endpoint string, dialOpts ...grpc.DialOption) (*otlptrace.Exporter, error) {
	conn, err := grpc.DialContext(ctx, endpoint, dialOpts...)
	if err != nil {
		return nil, err
	}

	return otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
}

// DefaultResources returns a set of best practices tracing resources
func DefaultResources(name string, attrs ...attribute.KeyValue) *resource.Resource {
	tracingIdKey, err := os.Hostname()
	if err != nil {
		tracingIdKey = "unknown"
	}

	attrs = append(attrs,
		semconv.ServiceNameKey.String(name),
		semconv.ServiceInstanceIDKey.String(tracingIdKey))

	return resource.NewWithAttributes(
		semconv.SchemaURL,
		attrs...,
	)
}
