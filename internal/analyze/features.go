package analyze

import (
	"sort"

	"github.com/ahmed-hashim-pro/trace-diff/internal/otlp"
)

// Trace is the spans of one trace id, with the root that names its endpoint.
type Trace struct {
	ID       string
	Spans    []*otlp.Span
	Root     *otlp.Span
	Endpoint string
}

// Group splits spans into traces, dropping exact duplicate span ids (a
// collector retry can deliver a span twice). The root is the span with no
// parent in the trace; when there are several (a parent was lost), the
// earliest server span wins, then the earliest span.
func Group(spans []otlp.Span, keepLiterals bool) []*Trace {
	byID := map[string]*Trace{}
	seen := map[[2]string]bool{}
	var order []string
	for i := range spans {
		s := &spans[i]
		if s.TraceID == "" || seen[[2]string{s.TraceID, s.SpanID}] {
			continue
		}
		seen[[2]string{s.TraceID, s.SpanID}] = true
		t := byID[s.TraceID]
		if t == nil {
			t = &Trace{ID: s.TraceID}
			byID[s.TraceID] = t
			order = append(order, s.TraceID)
		}
		t.Spans = append(t.Spans, s)
	}
	sort.Strings(order)
	out := make([]*Trace, 0, len(order))
	for _, id := range order {
		t := byID[id]
		ids := make(map[string]bool, len(t.Spans))
		for _, s := range t.Spans {
			ids[s.SpanID] = true
		}
		for _, s := range t.Spans {
			if s.ParentID != "" && ids[s.ParentID] {
				continue
			}
			if t.Root == nil || better(s, t.Root) {
				t.Root = s
			}
		}
		op := opOf(t.Root, keepLiterals)
		t.Endpoint = op.Service + " " + op.Desc
		out = append(out, t)
	}
	return out
}

func better(a, b *otlp.Span) bool {
	as, bs := a.Kind == otlp.KindServer, b.Kind == otlp.KindServer
	if as != bs {
		return as
	}
	if a.Start != b.Start {
		return a.Start < b.Start
	}
	return a.SpanID < b.SpanID
}

// features is what one trace looks like, per operation.
type features struct {
	t   *Trace
	dur float64 // root duration, ms

	count   map[string]int     // spans of the op
	maxSib  map[string]int     // most spans of the op under one parent
	maxConc map[string]int     // most of those running at the same time
	retries map[string]int     // spans of the op that follow a failed one
	crit    map[string]float64 // ms of the critical path spent in the op itself
	critIn  map[string]float64 // ms of the critical path inside the op's spans, children included
	parent  map[string]string  // op -> op of its parent (first seen)
	callee  map[string]string  // client op -> op of the server span it reached
}

const ms = 1e6

func extract(t *Trace, opKey map[*otlp.Span]string, anyRepeatIsRetry bool) *features {
	f := &features{
		t:       t,
		dur:     float64(t.Root.Duration()) / ms,
		count:   map[string]int{},
		maxSib:  map[string]int{},
		maxConc: map[string]int{},
		retries: map[string]int{},
		crit:    map[string]float64{},
		critIn:  map[string]float64{},
		parent:  map[string]string{},
		callee:  map[string]string{},
	}
	byID := make(map[string]*otlp.Span, len(t.Spans))
	for _, s := range t.Spans {
		byID[s.SpanID] = s
	}
	children := map[string][]*otlp.Span{}
	for _, s := range t.Spans {
		k := opKey[s]
		f.count[k]++
		if p := byID[s.ParentID]; p != nil && s != t.Root {
			children[p.SpanID] = append(children[p.SpanID], s)
			if _, ok := f.parent[k]; !ok {
				f.parent[k] = opKey[p]
			}
			if p.Kind == otlp.KindClient && s.Kind == otlp.KindServer {
				if _, ok := f.callee[opKey[p]]; !ok {
					f.callee[opKey[p]] = k
				}
			}
		}
	}
	for _, kids := range children {
		sort.Slice(kids, func(i, j int) bool {
			if kids[i].Start != kids[j].Start {
				return kids[i].Start < kids[j].Start
			}
			return kids[i].SpanID < kids[j].SpanID
		})
		same := map[string][]*otlp.Span{}
		for _, c := range kids {
			same[opKey[c]] = append(same[opKey[c]], c)
		}
		for k, group := range same {
			f.maxSib[k] = max(f.maxSib[k], len(group))
			f.maxConc[k] = max(f.maxConc[k], concurrency(group))
			f.retries[k] += retries(group, anyRepeatIsRetry)
		}
	}
	critical(t.Root, t.Root.End, children, opKey, f.crit, f.critIn)
	return f
}

// concurrency is the most spans of group running at one instant. Spans that
// only touch end to start do not overlap.
func concurrency(group []*otlp.Span) int {
	type ev struct {
		t     int64
		delta int
	}
	evs := make([]ev, 0, 2*len(group))
	for _, s := range group {
		evs = append(evs, ev{s.Start, 1}, ev{max(s.End, s.Start), -1})
	}
	sort.Slice(evs, func(i, j int) bool {
		if evs[i].t != evs[j].t {
			return evs[i].t < evs[j].t
		}
		return evs[i].delta < evs[j].delta // ends before starts
	})
	cur, best := 0, 0
	for _, e := range evs {
		cur += e.delta
		best = max(best, cur)
	}
	return best
}

// retries counts the spans in a group of same-op siblings (sorted by start)
// that are repeats of a failed attempt: they start after an earlier sibling
// that ended with an error, or carry http.request.resend_count.
func retries(group []*otlp.Span, anyRepeat bool) int {
	n := 0
	for i, s := range group {
		if rc := s.Attrs["http.request.resend_count"]; rc != "" && rc != "0" {
			n++
			continue
		}
		for _, prev := range group[:i] {
			if (prev.Err || anyRepeat) && prev.End <= s.Start {
				n++
				break
			}
		}
	}
	return n
}

// critical walks the critical path of s up to the time until: the chain of
// work that the parent was actually waiting on. Starting from the end, it
// repeatedly takes the child that finished last before the current point,
// charges the gap to s itself, and continues from that child's start. The
// self time of every span on the path is added to out by op, so the values
// add up to the root's duration. The time the request waited inside each
// span, children included, is added to incl.
func critical(s *otlp.Span, until int64, children map[string][]*otlp.Span, opKey map[*otlp.Span]string, out, incl map[string]float64) {
	cur := min(s.End, until)
	if cur > s.Start {
		incl[opKey[s]] += float64(cur-s.Start) / ms
	}
	used := map[*otlp.Span]bool{}
	for cur > s.Start {
		var next *otlp.Span
		var nextEnd int64
		for _, c := range children[s.SpanID] {
			if used[c] || c.Start >= cur {
				continue
			}
			e := min(c.End, cur)
			if next == nil || e > nextEnd {
				next, nextEnd = c, e
			}
		}
		if next == nil {
			break
		}
		used[next] = true
		out[opKey[s]] += float64(cur-nextEnd) / ms
		critical(next, nextEnd, children, opKey, out, incl)
		cur = max(next.Start, s.Start)
	}
	if cur > s.Start {
		out[opKey[s]] += float64(cur-s.Start) / ms
	}
}
