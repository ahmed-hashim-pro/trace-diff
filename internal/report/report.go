// Package report renders a comparison as text, JSON, or an SVG waterfall of
// the traces behind the top finding.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/ahmed-hashim-pro/trace-diff/internal/analyze"
	"github.com/ahmed-hashim-pro/trace-diff/internal/otlp"
)

// Text writes the human-readable report.
func Text(w io.Writer, r *analyze.Report) {
	fmt.Fprintf(w, "before: %s, %s, %s\n", r.Before.Name, plural(r.Before.Traces, "trace"), plural(r.Before.Spans, "span"))
	fmt.Fprintf(w, "after:  %s, %s, %s\n\n", r.After.Name, plural(r.After.Traces, "trace"), plural(r.After.Spans, "span"))

	width := len("endpoint")
	for _, e := range r.Endpoints {
		width = max(width, len(e.Name))
	}
	fmt.Fprintf(w, "%-*s  %-11s  %-15s  %s\n", width, "endpoint", "requests", "median ms", "p95 ms")
	for _, e := range r.Endpoints {
		fmt.Fprintf(w, "%-*s  %-11s  %-15s  %s\n", width, e.Name,
			fmt.Sprintf("%d → %d", e.NA, e.NB),
			fmt.Sprintf("%s → %s", msf(e.MedA), msf(e.MedB)),
			fmt.Sprintf("%s → %s", msf(e.P95A), msf(e.P95B)))
	}
	fmt.Fprintln(w)

	if len(r.Findings) == 0 {
		fmt.Fprintln(w, "no structural changes found")
	}
	for _, f := range r.Findings {
		fmt.Fprintf(w, "%-4s  %s  %s\n", strings.ToUpper(f.Severity), f.Kind, f.Endpoint)
		fmt.Fprintf(w, "      %s\n", f.Title)
		for _, l := range f.Lines {
			fmt.Fprintf(w, "      %s\n", l)
		}
		for _, e := range f.Examples {
			fmt.Fprintf(w, "      trace %-6s %s  %s\n", e.Side, e.TraceID, e.Note)
		}
		fmt.Fprintln(w)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
	if len(r.Notes) > 0 {
		fmt.Fprintln(w)
	}
	count := map[string]int{}
	for _, f := range r.Findings {
		count[f.Severity]++
	}
	fmt.Fprintf(w, "%d failing, %d to look at, %d for context\n", count["fail"], count["warn"], count["info"])
}

// JSON writes the report as JSON.
func JSON(w io.Writer, r *analyze.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func plural(n int, word string) string {
	s := fmt.Sprintf("%d %s", n, word)
	if n != 1 {
		s += "s"
	}
	return s
}

func msf(v float64) string {
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	case v >= 10:
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

// Tree writes one trace as an indented tree: offset and duration in ms,
// service, kind, and what each span did.
func Tree(w io.Writer, t *analyze.Trace) {
	rows := layout(t)
	fmt.Fprintf(w, "trace %s  %s  %s ms, %s\n", t.ID, t.Endpoint, msf(float64(t.Root.Duration())/1e6), plural(len(t.Spans), "span"))
	for _, r := range rows {
		mark := ""
		if r.span.Err {
			mark = "  ERROR"
		}
		fmt.Fprintf(w, "%8s %8s  %s%s · %s · %s%s\n",
			msf(float64(r.span.Start-t.Root.Start)/1e6), msf(float64(r.span.Duration())/1e6),
			strings.Repeat("  ", r.depth), r.span.Service, kindWord(r.span), analyze.Describe(r.span), mark)
	}
}

func kindWord(s *otlp.Span) string {
	if analyze.IsDB(s) {
		return "db"
	}
	return s.Kind.String()
}

type row struct {
	span  *otlp.Span
	depth int
}

// layout orders a trace's spans depth first, children by start time. Spans
// whose parent is missing are placed under the root.
func layout(t *analyze.Trace) []row {
	ids := map[string]bool{}
	for _, s := range t.Spans {
		ids[s.SpanID] = true
	}
	kids := map[string][]*otlp.Span{}
	for _, s := range t.Spans {
		if s == t.Root {
			continue
		}
		p := s.ParentID
		if !ids[p] {
			p = t.Root.SpanID
		}
		kids[p] = append(kids[p], s)
	}
	for _, k := range kids {
		sort.Slice(k, func(i, j int) bool {
			if k[i].Start != k[j].Start {
				return k[i].Start < k[j].Start
			}
			return k[i].SpanID < k[j].SpanID
		})
	}
	var out []row
	var walk func(s *otlp.Span, d int)
	walk = func(s *otlp.Span, d int) {
		out = append(out, row{s, d})
		for _, c := range kids[s.SpanID] {
			walk(c, d+1)
		}
	}
	walk(t.Root, 0)
	return out
}
