package analyze

import (
	"strings"
	"testing"

	"github.com/ahmed-hashim-pro/trace-diff/internal/otlp"
)

func TestNormalizeSQL(t *testing.T) {
	cases := map[string]string{
		"SELECT * FROM users WHERE id = 42":                        "SELECT * FROM users WHERE id = ?",
		"SELECT * FROM users WHERE id = $1":                        "SELECT * FROM users WHERE id = ?",
		"select a from t where name = 'O''Brien' and x > 1.5e3;":   "select a from t where name = ? and x > ?",
		"SELECT * FROM t WHERE id IN (1, 2, 3)":                    "SELECT * FROM t WHERE id IN (?)",
		"SELECT * FROM t WHERE id IN ($1,$2)":                      "SELECT * FROM t WHERE id IN (?)",
		"INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y'), (3, 'z')": "INSERT INTO t (a, b) VALUES (?)",
		"SELECT col1 FROM table2 WHERE c = :name AND d = @p1":      "SELECT col1 FROM table2 WHERE c = ? AND d = ?",
		"SELECT  *\n  FROM t\tWHERE x = 0xFF":                      "SELECT * FROM t WHERE x = ?",
	}
	for in, want := range cases {
		if got := NormalizeSQL(in); got != want {
			t.Errorf("NormalizeSQL(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"/orders/42": "/orders/{n}",
		"/users/3f2b8c1e-9d4a-4c1b-8e2f-1a2b3c4d5e6f/x": "/users/{id}/x",
		"/stock/SKU-01234?full=1":                       "/stock/{id}",
		"/blobs/deadbeefdeadbeef":                       "/blobs/{id}",
		"/v2/health":                                    "/v2/health",
	}
	for in, want := range cases {
		if got := NormalizePath(in); got != want {
			t.Errorf("NormalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpOf(t *testing.T) {
	cases := []struct {
		s    otlp.Span
		want string
	}{
		{otlp.Span{Service: "orders", Kind: otlp.KindClient, Name: "SELECT", Attrs: map[string]string{"db.statement": "SELECT 1 FROM t WHERE id = 7"}},
			"orders · db · SELECT ? FROM t WHERE id = ?"},
		{otlp.Span{Service: "orders", Kind: otlp.KindServer, Name: "GET /orders/7", Attrs: map[string]string{"http.request.method": "GET", "http.route": "/orders/{id}"}},
			"orders · server · GET /orders/{id}"},
		{otlp.Span{Service: "web", Kind: otlp.KindClient, Name: "GET", Attrs: map[string]string{"http.request.method": "GET", "url.full": "http://orders:8081/orders/7?x=1"}},
			"web · client · GET orders:8081 /orders/{n}"},
		{otlp.Span{Service: "web", Kind: otlp.KindInternal, Name: "render page 3"},
			"web · internal · render page {n}"},
		{otlp.Span{Service: "cache", Kind: otlp.KindClient, Name: "GET", Attrs: map[string]string{"db.system.name": "redis"}},
			"cache · db · GET"},
	}
	for _, c := range cases {
		s := c.s
		if s.Attrs == nil {
			s.Attrs = map[string]string{}
		}
		if got := OpOf(&s).String(); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

// span is a test helper: times in ms from 0.
func span(trace, id, parent, svc, name string, kind otlp.Kind, start, end float64, attrs ...string) otlp.Span {
	a := map[string]string{}
	for i := 0; i+1 < len(attrs); i += 2 {
		a[attrs[i]] = attrs[i+1]
	}
	return otlp.Span{TraceID: trace, SpanID: id, ParentID: parent, Service: svc, Name: name, Kind: kind,
		Start: int64(start * 1e6), End: int64(end * 1e6), Attrs: a}
}

// A root of 100 ms with two overlapping children, a (10-60) and b
// (20-80), and c (30-50) under b. Walking back from the end: the root
// waits on b from 80 down to 20 (b itself 40 ms, c 20 ms), then on a from
// 20 down to 10, then on nothing. a's work from 20 to 60 overlaps b, so the
// request was never waiting on it then.
func TestCriticalPathAddsUp(t *testing.T) {
	spans := []otlp.Span{
		span("t1", "r", "", "gw", "root", otlp.KindServer, 0, 100),
		span("t1", "a", "r", "gw", "a", otlp.KindInternal, 10, 60),
		span("t1", "b", "r", "gw", "b", otlp.KindInternal, 20, 80),
		span("t1", "c", "b", "gw", "c", otlp.KindInternal, 30, 50),
	}
	traces := Group(spans, false)
	keys := map[*otlp.Span]string{}
	for _, s := range traces[0].Spans {
		keys[s] = opOf(s, false).key()
	}
	f := extract(traces[0], keys, false)
	got := map[string]float64{}
	var total float64
	for k, v := range f.crit {
		got[strings.Split(k, "\x00")[2]] = v
		total += v
	}
	want := map[string]float64{"root": 30, "a": 10, "b": 40, "c": 20}
	for k, v := range want {
		if d := got[k] - v; d > 1e-9 || d < -1e-9 {
			t.Errorf("%s: %.3f ms, want %.3f (all %v)", k, got[k], v, got)
		}
	}
	if total != 100 {
		t.Errorf("critical path adds to %v ms, want the root's 100", total)
	}
}

func TestConcurrencyAndRetries(t *testing.T) {
	seq := []*otlp.Span{
		{Start: 0, End: 10}, {Start: 10, End: 20}, {Start: 20, End: 30},
	}
	if c := concurrency(seq); c != 1 {
		t.Errorf("back to back: concurrency %d, want 1", c)
	}
	par := []*otlp.Span{{Start: 0, End: 10}, {Start: 1, End: 10}, {Start: 2, End: 12}, {Start: 11, End: 13}}
	if c := concurrency(par); c != 3 {
		t.Errorf("concurrency %d, want 3", c)
	}
	attempts := []*otlp.Span{
		{Start: 0, End: 30, Err: true}, {Start: 30, End: 60, Err: true}, {Start: 60, End: 70, Attrs: map[string]string{}},
	}
	for _, s := range attempts {
		if s.Attrs == nil {
			s.Attrs = map[string]string{}
		}
	}
	if r := retries(attempts, false); r != 2 {
		t.Errorf("retries %d, want 2", r)
	}
	if r := retries(seq2(), false); r != 0 {
		t.Errorf("repeats without errors are not retries: %d", r)
	}
	if r := retries(seq2(), true); r != 2 {
		t.Errorf("the any-repeat mutant counts them: %d", r)
	}
}

func seq2() []*otlp.Span {
	return []*otlp.Span{
		{Start: 0, End: 1, Attrs: map[string]string{}},
		{Start: 1, End: 2, Attrs: map[string]string{}},
		{Start: 2, End: 3, Attrs: map[string]string{}},
	}
}

func TestGroupDropsDuplicatesAndPicksServerRoot(t *testing.T) {
	spans := []otlp.Span{
		span("t", "x", "lost", "gw", "orphan", otlp.KindInternal, 0, 5),
		span("t", "r", "", "gw", "GET /a", otlp.KindServer, 1, 9, "http.route", "/a", "http.request.method", "GET"),
		span("t", "r", "", "gw", "GET /a", otlp.KindServer, 1, 9, "http.route", "/a", "http.request.method", "GET"),
	}
	ts := Group(spans, false)
	if len(ts) != 1 || len(ts[0].Spans) != 2 {
		t.Fatalf("got %d traces / %d spans", len(ts), len(ts[0].Spans))
	}
	if ts[0].Endpoint != "gw GET /a" {
		t.Errorf("endpoint %q", ts[0].Endpoint)
	}
}

func TestFewTracesAreSkippedNotGuessed(t *testing.T) {
	var a, b []otlp.Span
	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		a = append(a, span("A"+id, "r", "", "gw", "GET /x", otlp.KindServer, 0, 10, "http.route", "/x"))
		b = append(b, span("B"+id, "r", "", "gw", "GET /x", otlp.KindServer, 0, 10, "http.route", "/x"))
		for j := 0; j < 30; j++ {
			b = append(b, span("B"+id, string(rune('A'+j)), "r", "gw", "SELECT 1", otlp.KindClient, 1, 2, "db.query.text", "SELECT 1"))
		}
	}
	r := Compare(a, b, "a", "b", Defaults())
	if len(r.Findings) != 0 {
		t.Errorf("5 requests a side should not be judged: %v", r.Findings)
	}
	if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "need 20 each") {
		t.Errorf("notes: %v", r.Notes)
	}
}
