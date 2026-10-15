package otlp

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Shaped like the Collector's file exporter output: hex ids, 64-bit times
// as strings, int attributes as strings, two objects on two lines.
const collectorLines = `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"orders"}}]},"scopeSpans":[{"scope":{"name":"x"},"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","parentSpanId":"eee19b7ec3c1b173","name":"SELECT order_items","kind":3,"startTimeUnixNano":"1544712660000000000","endTimeUnixNano":"1544712661000000000","attributes":[{"key":"db.query.text","value":{"stringValue":"SELECT * FROM t WHERE id = $1"}},{"key":"db.response.returned_rows","value":{"intValue":"3"}},{"key":"ok","value":{"boolValue":true}}],"status":{"code":2}}]}]}]}
{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"frontend"}}]},"scopeSpans":[{"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b173","name":"GET /orders/{id}","kind":"SPAN_KIND_SERVER","startTimeUnixNano":1544712659000000000,"endTimeUnixNano":1544712662000000000,"status":{}}]}]}]}
`

func TestReadCollectorFile(t *testing.T) {
	spans, err := Read(strings.NewReader(collectorLines))
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 {
		t.Fatalf("got %d spans", len(spans))
	}
	db, root := spans[0], spans[1]
	if db.Service != "orders" || db.Kind != KindClient || !db.Err || db.ParentID != root.SpanID {
		t.Errorf("db span: %+v", db)
	}
	if db.Attrs["db.response.returned_rows"] != "3" || db.Attrs["ok"] != "true" {
		t.Errorf("attrs: %v", db.Attrs)
	}
	if db.Duration() != 1e9 {
		t.Errorf("duration %d", db.Duration())
	}
	if root.Kind != KindServer || root.ParentID != "" || root.Err {
		t.Errorf("root span: %+v", root)
	}
}

func TestBase64IDsAndZeroParent(t *testing.T) {
	// Protobuf-JSON style ids (base64), and an all-zero parent meaning none.
	in := `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"W47/95gDgQPSabYzgT/GDA==","spanId":"7uGbfsPBsXQ=","parentSpanId":"0000000000000000","name":"x","kind":1,"startTimeUnixNano":"1","endTimeUnixNano":"2"}]}]}]}`
	spans, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	s := spans[0]
	if s.TraceID != "5b8efff798038103d269b633813fc60c" || s.SpanID != "eee19b7ec3c1b174" || s.ParentID != "" {
		t.Errorf("ids: %q %q %q", s.TraceID, s.SpanID, s.ParentID)
	}
	if s.Service != "unknown_service" {
		t.Errorf("service %q", s.Service)
	}
}

func TestBadInputSaysWhere(t *testing.T) {
	in := collectorLines + `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"nothex","spanId":"eee19b7ec3c1b174","name":"broken"}]}]}]}`
	_, err := Read(strings.NewReader(in))
	if err == nil || !strings.Contains(err.Error(), "object 3") || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("err = %v", err)
	}
}

func TestRoundTripAndGzip(t *testing.T) {
	in, err := Read(strings.NewReader(collectorLines))
	if err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	if err := Write(&plain, in); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(plain.Bytes())
	zw.Close()
	for name, data := range map[string][]byte{"plain": plain.Bytes(), "gzip": gz.Bytes()} {
		out, err := Read(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(out) != len(in) {
			t.Fatalf("%s: %d spans, want %d", name, len(out), len(in))
		}
		for i := range in {
			a, b := in[i], out[i]
			if a.TraceID != b.TraceID || a.SpanID != b.SpanID || a.ParentID != b.ParentID ||
				a.Name != b.Name || a.Kind != b.Kind || a.Service != b.Service ||
				a.Start != b.Start || a.End != b.End || a.Err != b.Err || len(a.Attrs) != len(b.Attrs) {
				t.Errorf("%s: span %d changed:\n%+v\n%+v", name, i, a, b)
			}
		}
	}
}

func protoRequest() *tracepb.TracesData {
	str := func(k, v string) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
	}
	return &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", "pricing")}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			TraceId:           bytes.Repeat([]byte{0xab}, 16),
			SpanId:            bytes.Repeat([]byte{0xcd}, 8),
			Name:              "GET /quote",
			Kind:              tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: 100,
			EndTimeUnixNano:   250,
			Attributes: []*commonpb.KeyValue{str("http.route", "/quote"),
				{Key: "http.response.status_code", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 200}}}},
			Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR},
		}}}},
	}}}
}

func TestReceiverProtobufAndJSON(t *testing.T) {
	var out bytes.Buffer
	rcv := NewReceiver(&out)
	srv := httptest.NewServer(rcv)
	defer srv.Close()

	body, _ := proto.Marshal(protoRequest())
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(body)
	zw.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/traces", &gz)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-protobuf" {
		t.Fatalf("protobuf: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	resp, err = http.Post(srv.URL+"/v1/traces", "application/json", strings.NewReader(collectorLines))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("json: %d", resp.StatusCode)
	}
	resp, _ = http.Post(srv.URL+"/v1/traces", "text/plain", strings.NewReader("x"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain: %d", resp.StatusCode)
	}
	resp, _ = http.Post(srv.URL+"/v1/traces", "application/x-protobuf", strings.NewReader("\xff\xff"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("garbage protobuf: %d", resp.StatusCode)
	}

	spans, err := Read(&out)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 3 || rcv.Spans() != 3 {
		t.Fatalf("wrote %d spans (counted %d), want 3", len(spans), rcv.Spans())
	}
	p := spans[0]
	if p.Service != "pricing" || p.TraceID != strings.Repeat("ab", 16) || p.SpanID != strings.Repeat("cd", 8) ||
		p.Kind != KindServer || !p.Err || p.Duration() != 150 || p.Attrs["http.response.status_code"] != "200" {
		t.Errorf("protobuf span: %+v", p)
	}
}
