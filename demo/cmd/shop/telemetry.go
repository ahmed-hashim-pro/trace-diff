package main

import (
	"context"
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// setupTracing exports every span over OTLP/HTTP (protobuf) to
// OTEL_EXPORTER_OTLP_ENDPOINT, the SDK's standard variable.
func setupTracing(ctx context.Context, service string) (func(context.Context) error, error) {
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(attribute.String("service.name", service)))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp.Shutdown, nil
}

// route serves h under a route pattern with a server span named after the
// route ("GET /orders/{id}") and carrying http.route.
func route(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	path := pattern
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		path = pattern[i+1:]
	}
	mux.Handle(pattern, otelhttp.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("http.route", path))
		h(w, r)
	}), pattern))
}

// tracedClient is an HTTP client whose every request gets a client span and
// carries the trace context to the next service.
func tracedClient() *http.Client {
	return &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
}

// dbSpan starts a span for one database statement, with the attributes of
// the OpenTelemetry database conventions.
func dbSpan(ctx context.Context, op, table, stmt string) (context.Context, trace.Span) {
	return otel.Tracer("shop/db").Start(ctx, op+" "+table,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system.name", "postgresql"),
			attribute.String("db.operation.name", op),
			attribute.String("db.collection.name", table),
			attribute.String("db.query.text", stmt),
			attribute.String("server.address", "postgres"),
		))
}

func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}
