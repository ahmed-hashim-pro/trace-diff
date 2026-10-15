// Package otlpcheck tests trace-diff's OTLP/HTTP receiver against the real
// OpenTelemetry Go SDK and its default exporter (protobuf over HTTP), so the
// receiver is checked against what an instrumented service actually sends.
package otlpcheck

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/ahmed-hashim-pro/trace-diff/internal/analyze"
	"github.com/ahmed-hashim-pro/trace-diff/internal/otlp"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func TestSDKToReceiver(t *testing.T) {
	var out lockedBuffer
	srv := httptest.NewServer(otlp.NewReceiver(&out))
	defer srv.Close()

	ctx := context.Background()
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(srv.URL+"/v1/traces"))
	if err != nil {
		t.Fatal(err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "orders"))),
	)
	tr := tp.Tracer("test")
	var ids []trace.SpanContext
	for i := 0; i < 3; i++ {
		rctx, root := tr.Start(ctx, "GET /orders/{id}", trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.route", "/orders/{id}"), attribute.String("http.request.method", "GET")))
		for j := 0; j <= i; j++ {
			_, q := tr.Start(rctx, "SELECT order_items", trace.WithSpanKind(trace.SpanKindClient),
				trace.WithAttributes(attribute.String("db.query.text", "SELECT * FROM order_items WHERE id = $1"),
					attribute.Int("db.response.returned_rows", 1)))
			if j == 1 {
				q.RecordError(errors.New("boom"))
				q.SetStatus(codes.Error, "boom")
			}
			q.End()
		}
		root.End()
		ids = append(ids, root.SpanContext())
	}
	if err := tp.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	spans, err := otlp.Read(&out.b)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 9 { // 3 roots and 1+2+3 queries
		t.Fatalf("received %d spans, want 9", len(spans))
	}
	traces := analyze.Group(spans, false)
	if len(traces) != 3 {
		t.Fatalf("%d traces, want 3", len(traces))
	}
	errs := 0
	for _, tr := range traces {
		if tr.Endpoint != "orders GET /orders/{id}" {
			t.Errorf("endpoint %q", tr.Endpoint)
		}
		for _, s := range tr.Spans {
			if s != tr.Root && s.ParentID != tr.Root.SpanID {
				t.Errorf("span %s has parent %s, want %s", s.SpanID, s.ParentID, tr.Root.SpanID)
			}
			if s.Err {
				errs++
			}
			if s.Kind == otlp.KindClient && (s.Attrs["db.response.returned_rows"] != "1" || analyze.Describe(s) != "SELECT * FROM order_items WHERE id = ?") {
				t.Errorf("db span: %v / %q", s.Attrs, analyze.Describe(s))
			}
		}
	}
	if errs != 2 {
		t.Errorf("%d error spans, want 2", errs)
	}
	found := map[string]bool{}
	for _, tr := range traces {
		found[tr.ID] = true
	}
	for _, sc := range ids {
		if !found[sc.TraceID().String()] {
			t.Errorf("trace %s missing", sc.TraceID())
		}
	}
}
