// Package analyze compares two sets of traces, endpoint by endpoint, and
// reports structural changes: calls that are new, repeated per item,
// retried, fanned out, or gone, and shifts in latency with where on the
// critical path they came from.
package analyze

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/ahmed-hashim-pro/trace-diff/internal/otlp"
	"github.com/ahmed-hashim-pro/trace-diff/internal/stats"
)

// Finding kinds.
const (
	NPlusOne     = "n-plus-one"    // a call now repeated, one after another, per request
	FanOut       = "fan-out"       // a call now repeated in parallel
	RetryStorm   = "retry-storm"   // attempts that follow a failed attempt
	NewSyncCall  = "new-sync-call" // a new call the request waits for
	NewCall      = "new-call"      // a new call off the critical path
	MoreFrequent = "more-frequent" // an existing call made in more requests
	MoreCalls    = "more-calls"    // more of an existing call per request
	Slower       = "latency"       // the endpoint got slower
	Removed      = "removed-call"
	Fewer        = "fewer-calls"
	Faster       = "faster"
)

// Severity of a kind: fail means the change should block a merge, warn is
// worth a look, info is reported for context.
func Severity(kind string) string {
	switch kind {
	case NPlusOne, FanOut, RetryStorm, NewSyncCall, MoreCalls, Slower:
		return "fail"
	case NewCall, MoreFrequent:
		return "warn"
	}
	return "info"
}

// Options are the thresholds. A change is reported only when it is both
// statistically significant (Benjamini-Hochberg false discovery rate Q over
// every test in the comparison) and big enough to matter (the Min* floors).
type Options struct {
	MinTraces        int     // per endpoint and side, below which the endpoint is skipped
	Q                float64 // false discovery rate
	MinPresenceDelta float64 // share of requests, e.g. 0.10 = ten points
	MinCountDelta    float64 // extra calls per request, on average
	MinCountRatio    float64 // and at least this many times as many
	MinRetryDelta    float64 // share of requests with a retry
	LatencyRel       float64 // median shift as a share of the old median
	LatencyAbsMs     float64 // and at least this many milliseconds
	Mutant           string  // a deliberate defect, for showing the tests have teeth
}

// Defaults are the thresholds the README's numbers were measured with.
func Defaults() Options {
	return Options{
		MinTraces:        20,
		Q:                0.001,
		MinPresenceDelta: 0.10,
		MinCountDelta:    0.5,
		MinCountRatio:    1.5,
		MinRetryDelta:    0.02,
		LatencyRel:       0.20,
		LatencyAbsMs:     2,
	}
}

// Mutants are deliberately broken variants of the comparison.
var Mutants = []struct{ Name, What string }{
	{"keep-literals", "match spans with their literal values left in"},
	{"pooled", "compare all requests together instead of per endpoint"},
	{"no-correction", "no correction for running many tests"},
	{"no-floors", "report any significant change, however small"},
	{"any-repeat-is-retry", "count every repeated call as a retry, failed or not"},
}

// Report is the result of a comparison.
type Report struct {
	Before, After Side
	Endpoints     []Endpoint
	Findings      []Finding
	Notes         []string
}

// Side describes one input.
type Side struct {
	Name          string
	Traces, Spans int
}

// Endpoint summarizes one endpoint on both sides.
type Endpoint struct {
	Name       string
	NA, NB     int
	MedA, MedB float64 // ms
	P95A, P95B float64
	Compared   bool
}

// Finding is one reported change.
type Finding struct {
	Kind     string
	Severity string
	Endpoint string
	Op       Op
	Title    string
	Lines    []string
	P        float64 // adjusted
	Examples []Example

	key, parent string // op keys, for folding a subtree into one finding
	isNew       bool
}

// Example points at a trace that shows the finding.
type Example struct {
	Side    string // before, after
	TraceID string
	Note    string
}

// Failing returns the findings at or above the given severity.
func (r *Report) Failing(level string) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Severity == "fail" || (level == "warn" && f.Severity == "warn") || level == "info" {
			out = append(out, f)
		}
	}
	return out
}

// side is one input, grouped by endpoint, with features extracted.
type side struct {
	byEP map[string][]*features
	ops  map[string]Op
}

func prepare(spans []otlp.Span, o Options) side {
	keep := o.Mutant == "keep-literals"
	traces := Group(spans, keep)
	sd := side{byEP: map[string][]*features{}, ops: map[string]Op{}}
	opKey := make(map[*otlp.Span]string, len(spans))
	for _, t := range traces {
		for _, s := range t.Spans {
			op := opOf(s, keep)
			k := op.key()
			sd.ops[k] = op
			opKey[s] = k
		}
	}
	for _, t := range traces {
		ep := t.Endpoint
		if o.Mutant == "pooled" {
			ep = "(all requests)"
		}
		sd.byEP[ep] = append(sd.byEP[ep], extract(t, opKey, o.Mutant == "any-repeat-is-retry"))
	}
	return sd
}

// test is one hypothesis test, to be corrected together with all others.
type test struct {
	ep, op string
	what   string // pres-up, pres-down, count-up, count-down, retry-up, slower, faster
	p, adj float64
}

// Compare compares before with after.
func Compare(before, after []otlp.Span, nameA, nameB string, o Options) *Report {
	a, b := prepare(before, o), prepare(after, o)
	r := &Report{
		Before: Side{Name: nameA, Spans: len(before)},
		After:  Side{Name: nameB, Spans: len(after)},
	}
	ops := map[string]Op{}
	for k, v := range a.ops {
		ops[k] = v
	}
	for k, v := range b.ops {
		ops[k] = v
	}
	var eps []string
	for ep, fs := range a.byEP {
		r.Before.Traces += len(fs)
		eps = append(eps, ep)
	}
	for ep, fs := range b.byEP {
		r.After.Traces += len(fs)
		if _, ok := a.byEP[ep]; !ok {
			eps = append(eps, ep)
		}
	}
	sort.Strings(eps)

	var tests []*test
	for _, ep := range eps {
		fa, fb := a.byEP[ep], b.byEP[ep]
		e := Endpoint{Name: ep, NA: len(fa), NB: len(fb)}
		da, db := durations(fa), durations(fb)
		e.MedA, e.P95A = stats.Quantile(da, 0.5), stats.Quantile(da, 0.95)
		e.MedB, e.P95B = stats.Quantile(db, 0.5), stats.Quantile(db, 0.95)
		switch {
		case len(fa) == 0:
			r.Notes = append(r.Notes, fmt.Sprintf("new endpoint %s: %d requests after, none before", ep, len(fb)))
		case len(fb) == 0:
			r.Notes = append(r.Notes, fmt.Sprintf("endpoint %s not seen after (%d requests before)", ep, len(fa)))
		case len(fa) < o.MinTraces || len(fb) < o.MinTraces:
			r.Notes = append(r.Notes, fmt.Sprintf("endpoint %s skipped: %d requests before, %d after, need %d each", ep, len(fa), len(fb), o.MinTraces))
		default:
			e.Compared = true
			tests = append(tests, endpointTests(ep, fa, fb)...)
		}
		r.Endpoints = append(r.Endpoints, e)
	}

	ps := make([]float64, len(tests))
	for i, t := range tests {
		ps[i] = t.p
	}
	adj := stats.BH(ps)
	if o.Mutant == "no-correction" {
		adj = ps
	}
	sig := map[string]map[string]map[string]*test{} // ep -> op -> what
	for i, t := range tests {
		t.adj = adj[i]
		if t.adj > o.Q {
			continue
		}
		if sig[t.ep] == nil {
			sig[t.ep] = map[string]map[string]*test{}
		}
		if sig[t.ep][t.op] == nil {
			sig[t.ep][t.op] = map[string]*test{}
		}
		sig[t.ep][t.op][t.what] = t
	}
	for _, ep := range eps {
		if sig[ep] == nil {
			continue
		}
		r.Findings = append(r.Findings, classify(ep, a.byEP[ep], b.byEP[ep], sig[ep], ops, o)...)
	}
	sort.SliceStable(r.Findings, func(i, j int) bool {
		fi, fj := r.Findings[i], r.Findings[j]
		if rank(fi.Severity) != rank(fj.Severity) {
			return rank(fi.Severity) < rank(fj.Severity)
		}
		if fi.Endpoint != fj.Endpoint {
			return fi.Endpoint < fj.Endpoint
		}
		if kindRank(fi.Kind) != kindRank(fj.Kind) {
			return kindRank(fi.Kind) < kindRank(fj.Kind)
		}
		return fi.P < fj.P
	})
	return r
}

func kindRank(k string) int {
	for i, x := range []string{NPlusOne, RetryStorm, FanOut, NewSyncCall, MoreCalls, Slower, NewCall, MoreFrequent, Removed, Fewer, Faster} {
		if x == k {
			return i
		}
	}
	return 99
}

func rank(sev string) int {
	return map[string]int{"fail": 0, "warn": 1, "info": 2}[sev]
}

func durations(fs []*features) []float64 {
	out := make([]float64, len(fs))
	for i, f := range fs {
		out[i] = f.dur
	}
	return out
}

func endpointTests(ep string, fa, fb []*features) []*test {
	var tests []*test
	keys := map[string]bool{}
	for _, f := range fa {
		for k := range f.count {
			keys[k] = true
		}
	}
	for _, f := range fb {
		for k := range f.count {
			keys[k] = true
		}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	na, nb := len(fa), len(fb)
	for _, k := range sorted {
		ca, cb := countsOf(fa, k), countsOf(fb, k)
		ka, kb := present(ca), present(cb)
		switch {
		case kb*na > ka*nb:
			tests = append(tests, &test{ep: ep, op: k, what: "pres-up", p: stats.FisherGreater(ka, na, kb, nb)})
		case kb*na < ka*nb:
			tests = append(tests, &test{ep: ep, op: k, what: "pres-down", p: stats.FisherGreater(kb, nb, ka, na)})
		}
		up, down := stats.MannWhitney(ca, cb)
		switch ma, mb := stats.Mean(ca), stats.Mean(cb); {
		case mb > ma:
			tests = append(tests, &test{ep: ep, op: k, what: "count-up", p: up})
		case mb < ma:
			tests = append(tests, &test{ep: ep, op: k, what: "count-down", p: down})
		}
		ra, rb := withRetries(fa, k), withRetries(fb, k)
		if rb*na > ra*nb {
			tests = append(tests, &test{ep: ep, op: k, what: "retry-up", p: stats.FisherGreater(ra, na, rb, nb)})
		}
	}
	da, db := durations(fa), durations(fb)
	// The tail: how many requests are slower than the old p95. A retry
	// storm or a rare slow path moves the tail and leaves the median alone.
	thr := stats.Quantile(da, 0.95)
	ka, kb := above(da, thr), above(db, thr)
	if kb*na > ka*nb {
		tests = append(tests, &test{ep: ep, what: "tail", p: stats.FisherGreater(ka, na, kb, nb)})
	}
	slower, faster := stats.MannWhitney(da, db)
	if stats.Quantile(db, 0.5) >= stats.Quantile(da, 0.5) {
		tests = append(tests, &test{ep: ep, what: "slower", p: slower})
	} else {
		tests = append(tests, &test{ep: ep, what: "faster", p: faster})
	}
	return tests
}

func above(v []float64, thr float64) int {
	n := 0
	for _, x := range v {
		if x > thr {
			n++
		}
	}
	return n
}

func countsOf(fs []*features, k string) []float64 {
	out := make([]float64, len(fs))
	for i, f := range fs {
		out[i] = float64(f.count[k])
	}
	return out
}

func present(c []float64) int {
	n := 0
	for _, v := range c {
		if v > 0 {
			n++
		}
	}
	return n
}

func withRetries(fs []*features, k string) int {
	n := 0
	for _, f := range fs {
		if f.retries[k] > 0 {
			n++
		}
	}
	return n
}

// opStats summarizes one op on one side of one endpoint.
type opStats struct {
	n, present         int
	pres, mean         float64
	sibMed, concMed    float64 // over requests where present
	countMed, countP95 float64 // over requests where present
	countMax           int
	retryReqs          int
	retryMean          float64 // retries per request where present
	critShare          float64 // share of present requests where it is on the critical path
	critMed, critP95   float64 // ms on the critical path, where present
	critMean           float64 // ms on the critical path per request, all requests
	parent, callee     string
}

func summarize(fs []*features, k string) opStats {
	s := opStats{n: len(fs)}
	var sib, conc, cnt, crit, critAll []float64
	var retries float64
	parents, callees := map[string]int{}, map[string]int{}
	for _, f := range fs {
		c := f.count[k]
		critAll = append(critAll, f.crit[k])
		if c == 0 {
			continue
		}
		s.present++
		sib = append(sib, float64(f.maxSib[k]))
		conc = append(conc, float64(f.maxConc[k]))
		cnt = append(cnt, float64(c))
		s.countMax = max(s.countMax, c)
		if f.retries[k] > 0 {
			s.retryReqs++
		}
		retries += float64(f.retries[k])
		if f.critIn[k] > 0 {
			s.critShare++
			crit = append(crit, f.critIn[k])
		}
		if p, ok := f.parent[k]; ok {
			parents[p]++
		}
		if c, ok := f.callee[k]; ok {
			callees[c]++
		}
	}
	if s.n > 0 {
		s.pres = float64(s.present) / float64(s.n)
		all := countsOf(fs, k)
		s.mean = stats.Mean(all)
	}
	if s.present > 0 {
		s.sibMed = stats.Quantile(sib, 0.5)
		s.concMed = stats.Quantile(conc, 0.5)
		s.countMed = stats.Quantile(cnt, 0.5)
		s.countP95 = stats.Quantile(cnt, 0.95)
		s.retryMean = retries / float64(s.present)
		s.critShare /= float64(s.present)
	}
	if len(crit) > 0 {
		s.critMed = stats.Quantile(crit, 0.5)
		s.critP95 = stats.Quantile(crit, 0.95)
	}
	s.critMean = stats.Mean(critAll)
	s.parent = mostCommon(parents)
	s.callee = mostCommon(callees)
	return s
}

func mostCommon(m map[string]int) string {
	best, bestN := "", 0
	for k, n := range m {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best
}

// classify turns the significant tests of one endpoint into findings,
// applying the size floors and naming each change by its shape.
func classify(ep string, fa, fb []*features, sig map[string]map[string]*test, ops map[string]Op, o Options) []Finding {
	floors := o.Mutant != "no-floors"
	var out []Finding
	keys := make([]string, 0, len(sig))
	for k := range sig {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	type gone struct {
		op string
		st opStats
	}
	var removed []gone
	for _, k := range keys {
		if k == "" {
			continue
		}
		t := sig[k]
		sa, sb := summarize(fa, k), summarize(fb, k)
		if t["pres-down"] != nil || t["count-down"] != nil {
			if !floors || sa.pres-sb.pres >= o.MinPresenceDelta || (sa.mean-sb.mean >= o.MinCountDelta && sa.mean >= o.MinCountRatio*sb.mean) {
				removed = append(removed, gone{k, sa})
			}
		}
	}
	isRemoved := map[string]bool{}
	for _, g := range removed {
		isRemoved[g.op] = true
	}
	replaced := map[string]bool{}

	for _, k := range keys {
		if k == "" {
			continue
		}
		t := sig[k]
		op := ops[k]
		sa, sb := summarize(fa, k), summarize(fb, k)
		presUp := t["pres-up"] != nil && (!floors || sb.pres-sa.pres >= o.MinPresenceDelta)
		countUp := t["count-up"] != nil && (!floors || (sb.mean-sa.mean >= o.MinCountDelta && sb.mean >= o.MinCountRatio*sa.mean))
		retryUp := t["retry-up"] != nil && (!floors || (float64(sb.retryReqs)/float64(sb.n)-float64(sa.retryReqs)/float64(sa.n) >= o.MinRetryDelta && sb.retryReqs >= 3))
		if !presUp && !countUp && !retryUp {
			continue
		}
		p := 1.0
		for _, w := range []string{"pres-up", "count-up", "retry-up"} {
			if t[w] != nil {
				p = math.Min(p, t[w].adj)
			}
		}
		isNew := sa.present == 0 || sa.pres < 0.01
		var kind string
		switch {
		case retryUp:
			kind = RetryStorm
		case sb.sibMed >= 2 && sb.sibMed > sa.sibMed:
			if sb.concMed >= 2 {
				kind = FanOut
			} else {
				kind = NPlusOne
			}
		case isNew && sb.critShare >= 0.5:
			kind = NewSyncCall
		case isNew:
			kind = NewCall
		case presUp:
			kind = MoreFrequent
		default:
			kind = MoreCalls
		}
		f := Finding{Kind: kind, Severity: Severity(kind), Endpoint: ep, Op: op, P: p}
		target := describeOp(op, sb.callee, ops)
		switch kind {
		case RetryStorm:
			f.Title = "retries: " + target
			f.Lines = append(f.Lines,
				fmt.Sprintf("requests with a retry: %s before, %s after", pct(sa.retryReqs, sa.n), pct(sb.retryReqs, sb.n)),
				fmt.Sprintf("attempts per request where called: mean %.2f before, %.2f after (max %d)", 1+sa.retryMean, 1+sb.retryMean, sb.countMax))
		case NPlusOne, FanOut:
			how := "one after another"
			if kind == FanOut {
				how = fmt.Sprintf("in parallel, %s at once (median)", num(sb.concMed))
			}
			f.Title = "repeated per request: " + target
			if isNew {
				f.Lines = append(f.Lines, fmt.Sprintf("new: in %s of requests, up to %d calls each (median %s, p95 %s), %s",
					pct(sb.present, sb.n), sb.countMax, cnt(sb.countMed), cnt(sb.countP95), how))
			} else {
				f.Lines = append(f.Lines, fmt.Sprintf("calls per request: median %s → %s, p95 %s → %s, max %d → %d, %s",
					cnt(sa.countMed), cnt(sb.countMed), cnt(sa.countP95), cnt(sb.countP95), sa.countMax, sb.countMax, how))
			}
		case NewSyncCall, NewCall:
			f.Title = "new call: " + target
			f.Lines = append(f.Lines, fmt.Sprintf("in %s of requests, %s each (median)", pct(sb.present, sb.n), calls(int(math.Round(sb.countMed)))))
		case MoreFrequent:
			f.Title = "called in more requests: " + target
			f.Lines = append(f.Lines, fmt.Sprintf("in %s of requests before, %s after", pct(sa.present, sa.n), pct(sb.present, sb.n)))
		case MoreCalls:
			f.Title = "more calls per request: " + target
			f.Lines = append(f.Lines, fmt.Sprintf("mean %.2f → %.2f per request, max %d → %d", sa.mean, sb.mean, sa.countMax, sb.countMax))
		}
		if sb.parent != "" {
			f.Lines = append(f.Lines, "called from "+ops[sb.parent].String())
		}
		if sb.critShare > 0 {
			f.Lines = append(f.Lines, fmt.Sprintf("the request waits on it in %s of those requests: %s ms median, %s ms p95",
				pct(int(math.Round(sb.critShare*float64(sb.present))), sb.present), ms2(sb.critMed), ms2(sb.critP95)))
		} else {
			f.Lines = append(f.Lines, "never on the critical path: the request does not wait for it")
		}
		f.Examples = examples(fa, fb, k, kind)
		f.key, f.parent, f.isNew = k, sb.parent, isNew
		out = append(out, f)
	}
	// A removed call and a new one under the same parent are usually one
	// rewritten into the other; pair each removed call with the most similar
	// new one.
	for _, g := range removed {
		best, bestSim := -1, -1.0
		for i, f := range out {
			if !f.isNew || f.parent != g.st.parent || f.parent == "" {
				continue
			}
			if sim := similarity(ops[f.key].Desc, ops[g.op].Desc); sim > bestSim {
				best, bestSim = i, sim
			}
		}
		if best >= 0 {
			f := &out[best]
			line := fmt.Sprintf("replaces %s (%s per request before, gone after)", ops[g.op].Desc, calls(int(math.Round(g.st.countMed))))
			f.Lines = append(f.Lines[:1], append([]string{line}, f.Lines[1:]...)...)
			replaced[g.op] = true
		}
	}
	out = fold(out, ops, "with")

	var lost []Finding
	for _, g := range removed {
		if replaced[g.op] {
			continue
		}
		sb := summarize(fb, g.op)
		op := ops[g.op]
		kind := Removed
		line := fmt.Sprintf("in %s of requests before, %s after", pct(g.st.present, g.st.n), pct(sb.present, sb.n))
		if sb.present > 0 && g.st.pres-sb.pres < o.MinPresenceDelta {
			kind = Fewer
			line = fmt.Sprintf("mean %.2f → %.2f per request", g.st.mean, sb.mean)
		}
		lost = append(lost, Finding{
			Kind: kind, Severity: Severity(kind), Endpoint: ep, Op: op,
			Title: "called less: " + describeOp(op, g.st.callee, ops),
			Lines: []string{line},
			P:     minAdj(sig[g.op], "pres-down", "count-down"),
			key:   g.op, parent: g.st.parent,
		})
	}
	out = append(out, fold(lost, ops, "with")...)

	if t := sig[""]; t != nil {
		da, db := durations(fa), durations(fb)
		ma, mb := stats.Quantile(da, 0.5), stats.Quantile(db, 0.5)
		shift := mb - ma
		big := math.Abs(shift) >= math.Max(o.LatencyRel*ma, o.LatencyAbsMs)
		pa, pb := stats.Quantile(da, 0.95), stats.Quantile(db, 0.95)
		tailBig := pb-pa >= math.Max(o.LatencyRel*pa, o.LatencyAbsMs) && float64(above(db, pa)) >= 0.10*float64(len(db))
		if !floors {
			big, tailBig = true, true
		}
		w, kind := (*test)(nil), Slower
		switch {
		case t["slower"] != nil && big:
			w = t["slower"]
		case t["faster"] != nil && big:
			w, kind = t["faster"], Faster
		case t["tail"] != nil && tailBig:
			w = t["tail"]
		}
		if w != nil {
			f := Finding{Kind: kind, Severity: Severity(kind), Endpoint: ep, P: w.adj,
				Title: "request latency",
				Lines: []string{fmt.Sprintf("median %s → %s ms (%+.0f%%), p95 %s → %s ms (%+.0f%%); %s of requests now slower than the old p95",
					ms2(ma), ms2(mb), 100*shift/ma, ms2(pa), ms2(pb), 100*(pb-pa)/pa, pct(above(db, pa), len(db)))},
			}
			f.Lines = append(f.Lines, attribution(fa, fb, ops, kind == Slower)...)
			q, label := 0.5, "median request"
			if w == t["tail"] {
				q, label = 0.95, "p95 request"
			}
			f.Examples = []Example{
				{"before", quantileTrace(fa, q), label},
				{"after", quantileTrace(fb, q), label},
			}
			out = append(out, f)
		}
	}
	return out
}

// fold merges findings about a subtree into the finding about its top: a
// new client call brings a new server span and its queries with it, and a
// retried call repeats everything beneath it. Those are one change, so they
// are reported once, with the rest listed under it.
func fold(fs []Finding, ops map[string]Op, word string) []Finding {
	byKey := map[string]int{}
	for i, f := range fs {
		byKey[f.key] = i
	}
	top := func(i int) int {
		for depth := 0; depth < 64; depth++ {
			j, ok := byKey[fs[i].parent]
			if !ok || j == i {
				return i
			}
			i = j
		}
		return i
	}
	var out []Finding
	idx := map[int]int{}
	for i := range fs {
		if top(i) == i {
			idx[i] = len(out)
			out = append(out, fs[i])
		}
	}
	for i := range fs {
		if t := top(i); t != i {
			o := &out[idx[t]]
			o.Lines = append(o.Lines, fmt.Sprintf("%s %s (%s)", word, ops[fs[i].key].String(), fs[i].Kind))
		}
	}
	return out
}

// similarity is the Jaccard index of the words of two descriptions.
func similarity(a, b string) float64 {
	wa, wb := map[string]bool{}, map[string]bool{}
	for _, w := range strings.Fields(a) {
		wa[w] = true
	}
	for _, w := range strings.Fields(b) {
		wb[w] = true
	}
	inter, union := 0, len(wa)
	for w := range wb {
		if wa[w] {
			inter++
		} else {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func calls(n int) string {
	switch n {
	case 0:
		return "no such call"
	case 1:
		return "1 call"
	}
	return fmt.Sprintf("%d calls", n)
}

func minAdj(t map[string]*test, whats ...string) float64 {
	p := 1.0
	for _, w := range whats {
		if t[w] != nil {
			p = math.Min(p, t[w].adj)
		}
	}
	return p
}

// attribution lists the ops whose time on the critical path moved most, in
// the direction of the latency change.
func attribution(fa, fb []*features, ops map[string]Op, slower bool) []string {
	keys := map[string]bool{}
	for _, f := range fa {
		for k := range f.crit {
			keys[k] = true
		}
	}
	for _, f := range fb {
		for k := range f.crit {
			keys[k] = true
		}
	}
	type moved struct {
		k string
		d float64
	}
	var ms []moved
	var total float64
	for k := range keys {
		var sa, sb float64
		for _, f := range fa {
			sa += f.crit[k]
		}
		for _, f := range fb {
			sb += f.crit[k]
		}
		d := sb/float64(len(fb)) - sa/float64(len(fa))
		if !slower {
			d = -d
		}
		if d > 0 {
			ms = append(ms, moved{k, d})
			total += d
		}
	}
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].d != ms[j].d {
			return ms[i].d > ms[j].d
		}
		return ms[i].k < ms[j].k
	})
	var out []string
	for i, m := range ms {
		if i == 3 || m.d < 0.1*total {
			break
		}
		sign := "+"
		if !slower {
			sign = "-"
		}
		out = append(out, fmt.Sprintf("%s%s ms mean on the critical path: %s", sign, ms2(m.d), ops[m.k].String()))
	}
	return out
}

func describeOp(op Op, callee string, ops map[string]Op) string {
	s := op.String()
	if callee != "" {
		c := ops[callee]
		s += " → " + c.Service + " " + c.Desc
	}
	return s
}

// examples picks traces that show a finding: the worst after, a typical
// after, and a typical before.
func examples(fa, fb []*features, k, kind string) []Example {
	score := func(f *features) int {
		if kind == RetryStorm {
			return f.retries[k]
		}
		return f.count[k]
	}
	var worst *features
	for _, f := range fb {
		if worst == nil || score(f) > score(worst) {
			worst = f
		}
	}
	var ex []Example
	if worst != nil && score(worst) > 0 {
		note := calls(worst.count[k])
		if kind == RetryStorm {
			note = fmt.Sprintf("%d attempts, %d retries", worst.count[k], worst.retries[k])
		}
		ex = append(ex, Example{"after", worst.t.ID, note})
	}
	var hits []*features
	for _, f := range fb {
		if score(f) > 0 {
			hits = append(hits, f)
		}
	}
	if len(hits) > 0 {
		sort.SliceStable(hits, func(i, j int) bool { return score(hits[i]) < score(hits[j]) })
		typ := hits[len(hits)/2]
		if worst == nil || score(typ) != score(worst) {
			note := "typical, " + calls(typ.count[k])
			if kind == RetryStorm {
				note = fmt.Sprintf("typical, %d attempts", typ.count[k])
			}
			ex = append(ex, Example{"after", typ.t.ID, note})
		}
	}
	if id := medianTrace(fa); id != "" {
		ex = append(ex, Example{"before", id, "median request, " + calls(countIn(fa, id, k))})
	}
	return ex
}

func countIn(fs []*features, id, k string) int {
	for _, f := range fs {
		if f.t.ID == id {
			return f.count[k]
		}
	}
	return 0
}

func medianTrace(fs []*features) string { return quantileTrace(fs, 0.5) }

// quantileTrace is the id of the request at quantile q of duration.
func quantileTrace(fs []*features, q float64) string {
	if len(fs) == 0 {
		return ""
	}
	s := append([]*features(nil), fs...)
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].dur != s[j].dur {
			return s[i].dur < s[j].dur
		}
		return s[i].t.ID < s[j].t.ID
	})
	return s[int(q*float64(len(s)-1))].t.ID
}

func pct(k, n int) string {
	if n == 0 {
		return "0%"
	}
	v := 100 * float64(k) / float64(n)
	if v > 0 && v < 1 {
		return fmt.Sprintf("%.1f%%", v)
	}
	return fmt.Sprintf("%.0f%%", v)
}

func num(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

func ms2(v float64) string {
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	case v >= 10:
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

// cnt formats a quantile of a call count: whole numbers, rounded.
func cnt(v float64) string { return fmt.Sprintf("%.0f", math.Round(v)) }

// String is a one-line summary of the finding.
func (f Finding) String() string {
	return strings.ToUpper(f.Severity) + " " + f.Kind + " " + f.Endpoint + ": " + f.Title
}
