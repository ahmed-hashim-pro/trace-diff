// Package otlp reads and writes traces in the OTLP JSON encoding: the format
// of an OTLP/HTTP JSON request body, and of what the OpenTelemetry
// Collector's file exporter writes, one TracesData object per line. It keeps
// only what trace-diff compares: ids, parent links, names, kinds, times,
// error status, the service name and span attributes as strings.
package otlp

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Kind is the OTLP span kind.
type Kind int

const (
	KindUnspecified Kind = iota
	KindInternal
	KindServer
	KindClient
	KindProducer
	KindConsumer
)

var kindNames = []string{"unspecified", "internal", "server", "client", "producer", "consumer"}

func (k Kind) String() string {
	if k >= 0 && int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "kind" + strconv.Itoa(int(k))
}

// Span is one span, flattened.
type Span struct {
	TraceID  string // lowercase hex
	SpanID   string
	ParentID string // "" for a root
	Name     string
	Kind     Kind
	Service  string // the resource's service.name
	Start    int64  // unix nanoseconds
	End      int64
	Err      bool // status code ERROR
	Attrs    map[string]string
}

// Duration in nanoseconds, never negative.
func (s *Span) Duration() int64 { return max(s.End-s.Start, 0) }

// ReadFile reads every span in an OTLP JSON file. The file may hold one
// object or many (JSON lines, as the Collector's file exporter writes), each
// a TracesData or ExportTraceServiceRequest; both have resourceSpans at the
// top. A .gz file, or any gzip stream, is decompressed first.
func ReadFile(path string) ([]Span, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	spans, err := Read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return spans, nil
}

// Read reads OTLP JSON from r, see ReadFile.
func Read(r io.Reader) ([]Span, error) {
	br := bufio.NewReaderSize(r, 1<<16)
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		br = bufio.NewReaderSize(gz, 1<<16)
	}
	dec := json.NewDecoder(br)
	var out []Span
	for n := 1; ; n++ {
		var td jTracesData
		if err := dec.Decode(&td); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("object %d: %w", n, err)
		}
		spans, err := td.flatten()
		if err != nil {
			return nil, fmt.Errorf("object %d: %w", n, err)
		}
		out = append(out, spans...)
	}
	return out, nil
}

// Write writes spans as one TracesData object (one line), grouped into one
// resource per service.
func Write(w io.Writer, spans []Span) error {
	b, err := Marshal(spans)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// Marshal encodes spans as a TracesData JSON object with hex ids.
func Marshal(spans []Span) ([]byte, error) {
	byService := map[string][]jSpan{}
	var services []string
	for i := range spans {
		s := &spans[i]
		if _, ok := byService[s.Service]; !ok {
			services = append(services, s.Service)
		}
		js := jSpan{
			TraceID:      s.TraceID,
			SpanID:       s.SpanID,
			ParentSpanID: s.ParentID,
			Name:         s.Name,
			Kind:         jEnum(s.Kind),
			Start:        jUint(s.Start),
			End:          jUint(s.End),
		}
		if s.Err {
			js.Status.Code = 2
		}
		keys := make([]string, 0, len(s.Attrs))
		for k := range s.Attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := s.Attrs[k]
			js.Attributes = append(js.Attributes, jKV{Key: k, Value: jAny{StringValue: &v}})
		}
		byService[s.Service] = append(byService[s.Service], js)
	}
	var td jTracesData
	for _, svc := range services {
		name := svc
		td.ResourceSpans = append(td.ResourceSpans, jResourceSpans{
			Resource: jResource{Attributes: []jKV{{Key: "service.name", Value: jAny{StringValue: &name}}}},
			ScopeSpans: []jScopeSpans{{
				Scope: jScope{Name: "trace-diff"},
				Spans: byService[svc],
			}},
		})
	}
	return json.Marshal(td)
}

// The OTLP JSON mapping: lowerCamelCase names, ids as hex strings, 64-bit
// integers as strings or numbers, enums as numbers or their names.

type jTracesData struct {
	ResourceSpans []jResourceSpans `json:"resourceSpans"`
}

type jResourceSpans struct {
	Resource   jResource     `json:"resource"`
	ScopeSpans []jScopeSpans `json:"scopeSpans"`
	// Pre-1.0 name for scopeSpans, still written by some old SDKs.
	InstrumentationLibrarySpans []jScopeSpans `json:"instrumentationLibrarySpans,omitempty"`
}

type jResource struct {
	Attributes []jKV `json:"attributes"`
}

type jScope struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

type jScopeSpans struct {
	Scope jScope  `json:"scope"`
	Spans []jSpan `json:"spans"`
}

type jStatus struct {
	Code    jEnum  `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type jSpan struct {
	TraceID      string  `json:"traceId"`
	SpanID       string  `json:"spanId"`
	ParentSpanID string  `json:"parentSpanId,omitempty"`
	Name         string  `json:"name"`
	Kind         jEnum   `json:"kind"`
	Start        jUint   `json:"startTimeUnixNano"`
	End          jUint   `json:"endTimeUnixNano"`
	Attributes   []jKV   `json:"attributes,omitempty"`
	Status       jStatus `json:"status"`
}

type jKV struct {
	Key   string `json:"key"`
	Value jAny   `json:"value"`
}

type jAny struct {
	StringValue *string    `json:"stringValue,omitempty"`
	BoolValue   *bool      `json:"boolValue,omitempty"`
	IntValue    *jUint     `json:"intValue,omitempty"`
	DoubleValue *float64   `json:"doubleValue,omitempty"`
	BytesValue  *string    `json:"bytesValue,omitempty"`
	ArrayValue  *jArray    `json:"arrayValue,omitempty"`
	KvlistValue *jKeyValue `json:"kvlistValue,omitempty"`
}

type jArray struct {
	Values []jAny `json:"values"`
}

type jKeyValue struct {
	Values []jKV `json:"values"`
}

// jUint accepts a JSON number or a decimal string, as the mapping allows.
type jUint int64

func (u *jUint) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*u = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return fmt.Errorf("not an integer: %s", b)
		}
		v = int64(f)
	}
	*u = jUint(v)
	return nil
}

func (u jUint) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strconv.FormatInt(int64(u), 10) + `"`), nil
}

// jEnum accepts a number or an enum value name (SPAN_KIND_SERVER,
// STATUS_CODE_ERROR).
type jEnum int

var enumNames = map[string]int{
	"SPAN_KIND_UNSPECIFIED": 0, "SPAN_KIND_INTERNAL": 1, "SPAN_KIND_SERVER": 2,
	"SPAN_KIND_CLIENT": 3, "SPAN_KIND_PRODUCER": 4, "SPAN_KIND_CONSUMER": 5,
	"STATUS_CODE_UNSET": 0, "STATUS_CODE_OK": 1, "STATUS_CODE_ERROR": 2,
}

func (e *jEnum) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		name := strings.Trim(string(b), `"`)
		v, ok := enumNames[name]
		if !ok {
			if n, err := strconv.Atoi(name); err == nil {
				v = n
			} else {
				return fmt.Errorf("unknown enum value %s", b)
			}
		}
		*e = jEnum(v)
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*e = jEnum(n)
	return nil
}

func (td *jTracesData) flatten() ([]Span, error) {
	var out []Span
	for _, rs := range td.ResourceSpans {
		service := "unknown_service"
		for _, kv := range rs.Resource.Attributes {
			if kv.Key == "service.name" {
				service = kv.Value.String()
			}
		}
		for _, ss := range append(rs.ScopeSpans, rs.InstrumentationLibrarySpans...) {
			for _, js := range ss.Spans {
				tid, err := normID(js.TraceID, 16)
				if err != nil {
					return nil, fmt.Errorf("span %q: traceId: %w", js.Name, err)
				}
				sid, err := normID(js.SpanID, 8)
				if err != nil {
					return nil, fmt.Errorf("span %q: spanId: %w", js.Name, err)
				}
				pid, err := normID(js.ParentSpanID, 8)
				if err != nil {
					return nil, fmt.Errorf("span %q: parentSpanId: %w", js.Name, err)
				}
				s := Span{
					TraceID: tid, SpanID: sid, ParentID: pid,
					Name: js.Name, Kind: Kind(js.Kind), Service: service,
					Start: int64(js.Start), End: int64(js.End),
					Err:   js.Status.Code == 2,
					Attrs: make(map[string]string, len(js.Attributes)),
				}
				for _, kv := range js.Attributes {
					s.Attrs[kv.Key] = kv.Value.String()
				}
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// normID returns an id as lowercase hex. The OTLP JSON mapping says hex;
// some producers write protobuf-JSON base64 instead, which is accepted too.
// An all-zero id means "none".
func normID(s string, size int) (string, error) {
	if s == "" {
		return "", nil
	}
	if len(s) == 2*size {
		if b, err := hex.DecodeString(s); err == nil {
			return zeroToEmpty(hex.EncodeToString(b)), nil
		}
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == size {
		return zeroToEmpty(hex.EncodeToString(b)), nil
	}
	return "", fmt.Errorf("%q is not a %d-byte id in hex or base64", s, size)
}

func zeroToEmpty(h string) string {
	if strings.Trim(h, "0") == "" {
		return ""
	}
	return h
}

// String renders an attribute value as text: strings as they are, numbers
// and booleans in their usual form, arrays and maps as JSON.
func (a jAny) String() string {
	switch {
	case a.StringValue != nil:
		return *a.StringValue
	case a.BoolValue != nil:
		return strconv.FormatBool(*a.BoolValue)
	case a.IntValue != nil:
		return strconv.FormatInt(int64(*a.IntValue), 10)
	case a.DoubleValue != nil:
		return strconv.FormatFloat(*a.DoubleValue, 'g', -1, 64)
	case a.BytesValue != nil:
		return *a.BytesValue
	case a.ArrayValue != nil:
		parts := make([]string, len(a.ArrayValue.Values))
		for i, v := range a.ArrayValue.Values {
			parts[i] = strconv.Quote(v.String())
		}
		return "[" + strings.Join(parts, ",") + "]"
	case a.KvlistValue != nil:
		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, kv := range a.KvlistValue.Values {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(strconv.Quote(kv.Key) + ":" + strconv.Quote(kv.Value.String()))
		}
		buf.WriteByte('}')
		return buf.String()
	}
	return ""
}
