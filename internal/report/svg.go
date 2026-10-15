package report

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/ahmed-hashim-pro/trace-diff/internal/analyze"
	"github.com/ahmed-hashim-pro/trace-diff/internal/otlp"
)

// Panel is one trace in a waterfall, with the spans to call out.
type Panel struct {
	Label     string
	Trace     *analyze.Trace
	Highlight func(*otlp.Span) bool
}

const (
	ink   = "#24292f"
	muted = "#57606a"
	grid  = "#d8dee4"
	red   = "#cf222e"
	font  = "-apple-system, 'Segoe UI', Helvetica, Arial, sans-serif"
)

var palette = []string{"#2a78d6", "#1f9e89", "#8a63d2", "#d4a72c", "#6e7781", "#cf6a87", "#0b7285"}

// svgRow is a span, or a run of same-op siblings drawn as one bar.
type svgRow struct {
	span      *otlp.Span
	depth     int
	more      int   // > 0: this row stands for this many more spans
	end       int64 // for a run: the end of its last span
	highlight bool
}

// Waterfall draws traces one above the other on a shared time scale, so a
// request that got longer looks longer. Runs of more than eight
// consecutive same-op siblings are drawn as six bars and one for the rest.
func Waterfall(title, subtitle string, panels []Panel) string {
	const (
		width, left, right = 1000.0, 380.0, 24.0
		rowH, barH         = 15.0, 10.0
	)
	plotW := width - left - right
	var span int64
	var all [][]svgRow
	services := map[string]int{}
	for _, p := range panels {
		// Work can outlive the request (an abandoned attempt keeps running),
		// so the scale runs to the last span's end, not the root's.
		for _, s := range p.Trace.Spans {
			span = max(span, s.End-p.Trace.Root.Start)
		}
		rows := compress(layout(p.Trace), p.Highlight)
		all = append(all, rows)
		for _, r := range rows {
			if _, ok := services[r.span.Service]; !ok {
				services[r.span.Service] = len(services)
			}
		}
	}
	if span <= 0 {
		span = 1
	}
	x := func(ns int64) float64 { return left + float64(ns)/float64(span)*plotW }

	var b strings.Builder
	f := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	y := 76.0
	var body strings.Builder
	g := func(format string, a ...any) { fmt.Fprintf(&body, format, a...) }
	stepMs := niceStep(float64(span)/1e6, 8)
	for pi, p := range panels {
		rows := all[pi]
		t0 := p.Trace.Root.Start
		g(`<text x="24" y="%.1f" font-size="13" font-weight="600" fill="%s">%s</text>`+"\n", y, ink, esc(p.Label))
		g(`<text x="%.1f" y="%.1f" font-size="11" text-anchor="end" fill="%s">%s ms, %s</text>`+"\n", width-right, y, muted,
			msf(float64(p.Trace.Root.Duration())/1e6), plural(len(p.Trace.Spans), "span"))
		y += 10
		top := y
		bottom := top + float64(len(rows))*rowH
		for at := 0.0; at <= float64(span)/1e6+1e-9; at += stepMs {
			xx := x(int64(at * 1e6))
			g(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s"/>`+"\n", xx, top, xx, bottom, grid)
			g(`<text x="%.1f" y="%.1f" font-size="10" text-anchor="middle" fill="%s">%s</text>`+"\n", xx, bottom+12, muted, trimMs(at))
		}
		for i, r := range rows {
			ry := top + float64(i)*rowH
			color := palette[services[r.span.Service]%len(palette)]
			if r.highlight {
				color = red
			}
			label := r.span.Service + " · " + analyze.Describe(r.span)
			if r.more > 0 {
				label = fmt.Sprintf("… %d more like the one above", r.more)
			}
			indent := 24 + 10*float64(r.depth)
			maxChars := int((left - 12 - indent) / 5.9)
			weight := "400"
			if r.highlight {
				weight = "600"
			}
			g(`<text x="%.1f" y="%.1f" font-size="10.5" font-weight="%s" fill="%s">%s</text>`+"\n", indent, ry+barH, weight, ink, esc(clipText(label, maxChars)))
			start, end := r.span.Start-t0, r.span.End-t0
			if r.more > 0 {
				end = r.end - t0
			}
			w := math.Max(x(end)-x(start), 1)
			if r.more > 0 {
				g(`<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="url(#run)" stroke="%s" stroke-width="0.8"/>`+"\n", x(start), ry+2, w, barH-1, color)
				continue
			}
			g(`<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"/>`+"\n", x(start), ry+2, w, barH-1, color)
			if r.span.Err {
				g(`<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="url(#err)" stroke="%s" stroke-width="1"/>`+"\n", x(start), ry+2, w, barH-1, red)
			}
		}
		y = bottom + 34
	}

	height := y + 20
	f(`<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" font-family="%s">`+"\n", width, height, width, height, font)
	f(`<defs><pattern id="err" width="5" height="5" patternUnits="userSpaceOnUse" patternTransform="rotate(45)"><rect width="5" height="5" fill="#ffffff"/><line x1="0" y1="0" x2="0" y2="5" stroke="%s" stroke-width="2"/></pattern>`, red)
	f(`<pattern id="run" width="4" height="4" patternUnits="userSpaceOnUse"><rect width="4" height="4" fill="#ffffff"/><circle cx="2" cy="2" r="1" fill="%s"/></pattern></defs>`+"\n", muted)
	f(`<rect width="100%%" height="100%%" fill="#ffffff"/>` + "\n")
	f(`<text x="24" y="30" font-size="17" font-weight="600" fill="%s">%s</text>`+"\n", ink, esc(title))
	f(`<text x="24" y="50" font-size="12.5" fill="%s">%s</text>`+"\n", muted, esc(subtitle))
	b.WriteString(body.String())
	// Legend: one swatch per service, then the call-outs.
	lx, ly := 24.0, height-14
	names := make([]string, 0, len(services))
	for s := range services {
		names = append(names, s)
	}
	sort.Slice(names, func(i, j int) bool { return services[names[i]] < services[names[j]] })
	item := func(sw, label string) {
		f(`<g transform="translate(%.1f,%.1f)">%s</g>`, lx, ly-9, sw)
		f(`<text x="%.1f" y="%.1f" font-size="11" fill="%s">%s</text>`+"\n", lx+18, ly, muted, esc(label))
		lx += 18 + 6.2*float64(len(label)) + 16
	}
	for _, s := range names {
		item(fmt.Sprintf(`<rect width="12" height="10" fill="%s"/>`, palette[services[s]%len(palette)]), s)
	}
	item(fmt.Sprintf(`<rect width="12" height="10" fill="%s"/>`, red), "the finding")
	item(fmt.Sprintf(`<rect width="12" height="10" fill="url(#err)" stroke="%s"/>`, red), "error")
	b.WriteString("</svg>\n")
	return b.String()
}

// compress turns runs of more than eight consecutive siblings with the same
// op (and their subtrees) into six rows and one summary row.
func compress(rows []row, hl func(*otlp.Span) bool) []svgRow {
	var out []svgRow
	for i := 0; i < len(rows); {
		r := rows[i]
		// The run: following rows at the same depth with the same op, each
		// with its subtree.
		op := analyze.OpOf(r.span)
		type item struct{ from, to int }
		var run []item
		j := i
		for j < len(rows) && rows[j].depth == r.depth && analyze.OpOf(rows[j].span) == op && rows[j].span.ParentID == r.span.ParentID {
			k := j + 1
			for k < len(rows) && rows[k].depth > r.depth {
				k++
			}
			run = append(run, item{j, k})
			j = k
		}
		if len(run) <= 8 {
			out = append(out, svgRow{span: r.span, depth: r.depth, highlight: hl != nil && hl(r.span)})
			i++
			continue
		}
		for _, it := range run[:6] {
			for k := it.from; k < it.to; k++ {
				out = append(out, svgRow{span: rows[k].span, depth: rows[k].depth, highlight: hl != nil && hl(rows[k].span)})
			}
		}
		rest := run[6:]
		first, last := rows[rest[0].from].span, rows[rest[len(rest)-1].from].span
		out = append(out, svgRow{span: first, depth: r.depth, more: len(rest), end: last.End, highlight: hl != nil && hl(first)})
		i = j
	}
	return out
}

func niceStep(span, target float64) float64 {
	raw := span / target
	if raw <= 0 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 5, 10} {
		if m*p >= raw {
			return m * p
		}
	}
	return 10 * p
}

func trimMs(v float64) string {
	s := fmt.Sprintf("%.3f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s + " ms"
}

// clipText shortens s to n runes by cutting from the middle: the end of a
// statement or path (WHERE id = ?) is often what tells two spans apart.
func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n || n < 8 {
		return s
	}
	head := (n - 1) * 2 / 5
	tail := n - 1 - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
