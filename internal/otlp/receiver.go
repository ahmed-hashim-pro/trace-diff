package otlp

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Receiver is an OTLP/HTTP trace endpoint (POST /v1/traces) that appends
// every request it receives to w as one line of OTLP JSON, the same format
// the Collector's file exporter writes. It accepts protobuf, which the
// OpenTelemetry SDKs send by default, and JSON, either one optionally
// gzipped.
type Receiver struct {
	mu    sync.Mutex
	w     io.Writer
	spans atomic.Int64
}

// NewReceiver writes to w. Writes are serialized; w needs no locking.
func NewReceiver(w io.Writer) *Receiver { return &Receiver{w: w} }

// Spans is how many spans have been written so far.
func (r *Receiver) Spans() int64 { return r.spans.Load() }

func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/v1/traces" {
		http.NotFound(w, req)
		return
	}
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body io.Reader = http.MaxBytesReader(w, req.Body, 64<<20)
	if strings.EqualFold(req.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "bad gzip: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer gz.Close()
		body = gz
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ct, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
	var spans []Span
	switch ct {
	case "application/x-protobuf", "application/protobuf":
		// ExportTraceServiceRequest and TracesData have the same wire
		// format (field 1: repeated ResourceSpans), and decoding into
		// TracesData keeps gRPC out of the dependency graph.
		var msg tracepb.TracesData
		if err := proto.Unmarshal(raw, &msg); err != nil {
			http.Error(w, "bad protobuf: "+err.Error(), http.StatusBadRequest)
			return
		}
		spans = FromProto(&msg)
	case "application/json":
		spans, err = Read(strings.NewReader(string(raw)))
		if err != nil {
			http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, fmt.Sprintf("unsupported Content-Type %q", ct), http.StatusUnsupportedMediaType)
		return
	}
	if len(spans) > 0 {
		r.mu.Lock()
		err = Write(r.w, spans)
		r.mu.Unlock()
		if err != nil {
			http.Error(w, "write: "+err.Error(), http.StatusInternalServerError)
			return
		}
		r.spans.Add(int64(len(spans)))
	}
	if ct == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
		return
	}
	// An empty ExportTraceServiceResponse encodes to zero bytes.
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

// FromProto flattens OTLP protobuf traces.
func FromProto(msg *tracepb.TracesData) []Span {
	var out []Span
	for _, rs := range msg.GetResourceSpans() {
		service := "unknown_service"
		for _, kv := range rs.GetResource().GetAttributes() {
			if kv.GetKey() == "service.name" {
				service = anyString(kv.GetValue())
			}
		}
		for _, ss := range rs.GetScopeSpans() {
			for _, ps := range ss.GetSpans() {
				s := Span{
					TraceID:  zeroToEmpty(hex.EncodeToString(ps.GetTraceId())),
					SpanID:   zeroToEmpty(hex.EncodeToString(ps.GetSpanId())),
					ParentID: zeroToEmpty(hex.EncodeToString(ps.GetParentSpanId())),
					Name:     ps.GetName(),
					Kind:     Kind(ps.GetKind()),
					Service:  service,
					Start:    int64(ps.GetStartTimeUnixNano()),
					End:      int64(ps.GetEndTimeUnixNano()),
					Err:      ps.GetStatus().GetCode() == 2,
					Attrs:    make(map[string]string, len(ps.GetAttributes())),
				}
				for _, kv := range ps.GetAttributes() {
					s.Attrs[kv.GetKey()] = anyString(kv.GetValue())
				}
				out = append(out, s)
			}
		}
	}
	return out
}

func anyString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	case *commonpb.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(x.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		parts := make([]string, len(x.ArrayValue.GetValues()))
		for i, e := range x.ArrayValue.GetValues() {
			parts[i] = strconv.Quote(anyString(e))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case *commonpb.AnyValue_KvlistValue:
		parts := make([]string, len(x.KvlistValue.GetValues()))
		for i, kv := range x.KvlistValue.GetValues() {
			parts[i] = strconv.Quote(kv.GetKey()) + ":" + strconv.Quote(anyString(kv.GetValue()))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	return ""
}
