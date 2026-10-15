// Command trace-diff compares traces from before and after a change and
// reports the structural regressions that tests and latency dashboards
// miss: N+1 queries, retry storms, fan-out growth, new calls the request
// waits for, and latency shifts with where on the critical path they came
// from.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ahmed-hashim-pro/trace-diff/internal/analyze"
	"github.com/ahmed-hashim-pro/trace-diff/internal/bench"
	"github.com/ahmed-hashim-pro/trace-diff/internal/otlp"
	"github.com/ahmed-hashim-pro/trace-diff/internal/report"
	"github.com/ahmed-hashim-pro/trace-diff/internal/stats"
	"github.com/ahmed-hashim-pro/trace-diff/internal/synth"
)

var version = "0.1.0"

const usage = `trace-diff compares traces from before and after a change.

usage:
  trace-diff check BEFORE AFTER [flags]   report; exit 1 if anything should block a merge
  trace-diff diff BEFORE AFTER [flags]    the same report, always exit 0
  trace-diff summary FILE                 endpoints and what each request calls
  trace-diff show FILE TRACE_ID           one trace as a tree
  trace-diff collect --out FILE           receive OTLP/HTTP traces and write them to FILE
  trace-diff synth --scenario NAME        generate a before and an after run of the example shop
  trace-diff bench                        measure trace-diff on every synthetic scenario
  trace-diff version

BEFORE and AFTER are OTLP JSON files: what the OpenTelemetry Collector's
file exporter writes, what trace-diff collect writes, or OTLP/HTTP JSON
request bodies. Gzipped files are read as they are.

check and diff flags:
  --json             write the report as JSON
  --svg FILE         draw the traces behind the top finding
  --svg-kind KIND    ... or behind the first finding of this kind
  --fail-on LEVEL    fail (default) or warn: the lowest severity that fails check
  --q Q              false discovery rate (default 0.001)
  --min-traces N     requests needed per endpoint and side (default 20)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	code := 0
	switch cmd {
	case "check":
		code, err = cmdCompare(rest, stdout, true)
	case "diff":
		code, err = cmdCompare(rest, stdout, false)
	case "summary":
		err = cmdSummary(rest, stdout)
	case "show":
		err = cmdShow(rest, stdout)
	case "collect":
		err = cmdCollect(rest, stdout, stderr)
	case "synth":
		err = cmdSynth(rest, stdout)
	case "bench":
		err = cmdBench(rest, stdout)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "trace-diff", version)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "trace-diff: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "trace-diff:", err)
		return 2
	}
	return code
}

// parse parses flags that may come before, between or after positional
// arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func cmdCompare(args []string, stdout io.Writer, failing bool) (int, error) {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	o := analyze.Defaults()
	asJSON := fs.Bool("json", false, "")
	svg := fs.String("svg", "", "")
	svgKind := fs.String("svg-kind", "", "")
	failOn := fs.String("fail-on", "fail", "")
	fs.Float64Var(&o.Q, "q", o.Q, "")
	fs.IntVar(&o.MinTraces, "min-traces", o.MinTraces, "")
	fs.StringVar(&o.Mutant, "mutant", "", "")
	pos, err := parse(fs, args)
	if err != nil {
		return 2, err
	}
	if len(pos) != 2 {
		return 2, errors.New("need two files: BEFORE AFTER")
	}
	if *failOn != "fail" && *failOn != "warn" {
		return 2, fmt.Errorf("--fail-on must be fail or warn, not %q", *failOn)
	}
	before, err := otlp.ReadFile(pos[0])
	if err != nil {
		return 2, err
	}
	after, err := otlp.ReadFile(pos[1])
	if err != nil {
		return 2, err
	}
	if len(before) == 0 || len(after) == 0 {
		return 2, fmt.Errorf("no spans in %s", map[bool]string{true: pos[0], false: pos[1]}[len(before) == 0])
	}
	r := analyze.Compare(before, after, pos[0], pos[1], o)
	if *asJSON {
		if err := report.JSON(stdout, r); err != nil {
			return 2, err
		}
	} else {
		report.Text(stdout, r)
	}
	if *svg != "" {
		if err := writeSVG(*svg, *svgKind, r, before, after); err != nil {
			return 2, err
		}
	}
	if failing && len(r.Failing(*failOn)) > 0 {
		return 1, nil
	}
	return 0, nil
}

func writeSVG(path, kind string, r *analyze.Report, before, after []otlp.Span) error {
	var f *analyze.Finding
	for i := range r.Findings {
		if len(r.Findings[i].Examples) >= 2 && (kind == "" || r.Findings[i].Kind == kind) {
			f = &r.Findings[i]
			break
		}
	}
	if f == nil {
		return errors.New("--svg: no finding with example traces to draw")
	}
	ta := byID(analyze.Group(before, false))
	tb := byID(analyze.Group(after, false))
	var pa, pb *analyze.Trace
	var la, lb string
	for _, e := range f.Examples {
		switch {
		case e.Side == "after" && pb == nil:
			pb, lb = tb[e.TraceID], "after: "+e.Note
		case e.Side == "before" && pa == nil:
			pa, la = ta[e.TraceID], "before: "+e.Note
		}
	}
	if pa == nil || pb == nil {
		return errors.New("--svg: example traces not found")
	}
	hl := func(s *otlp.Span) bool { return f.Op.Desc != "" && analyze.OpOf(s) == f.Op }
	title := f.Endpoint + ": " + f.Kind
	svg := report.Waterfall(title, f.Title, []report.Panel{
		{Label: la, Trace: pa, Highlight: hl},
		{Label: lb, Trace: pb, Highlight: hl},
	})
	return os.WriteFile(path, []byte(svg), 0o644)
}

func byID(ts []*analyze.Trace) map[string]*analyze.Trace {
	m := make(map[string]*analyze.Trace, len(ts))
	for _, t := range ts {
		m[t.ID] = t
	}
	return m
}

func cmdSummary(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("summary", flag.ContinueOnError)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("need one file")
	}
	spans, err := otlp.ReadFile(pos[0])
	if err != nil {
		return err
	}
	traces := analyze.Group(spans, false)
	byEP := map[string][]*analyze.Trace{}
	var eps []string
	for _, t := range traces {
		if _, ok := byEP[t.Endpoint]; !ok {
			eps = append(eps, t.Endpoint)
		}
		byEP[t.Endpoint] = append(byEP[t.Endpoint], t)
	}
	sort.Strings(eps)
	fmt.Fprintf(stdout, "%s: %d traces, %d spans\n", pos[0], len(traces), len(spans))
	for _, ep := range eps {
		ts := byEP[ep]
		var durs []float64
		counts := map[analyze.Op][]float64{}
		for i, t := range ts {
			durs = append(durs, float64(t.Root.Duration())/1e6)
			for _, s := range t.Spans {
				op := analyze.OpOf(s)
				if counts[op] == nil {
					counts[op] = make([]float64, len(ts))
				}
				counts[op][i]++
			}
		}
		fmt.Fprintf(stdout, "\n%s  %d requests, median %.2f ms, p95 %.2f ms\n", ep, len(ts),
			stats.Quantile(durs, 0.5), stats.Quantile(durs, 0.95))
		ops := make([]analyze.Op, 0, len(counts))
		for op := range counts {
			ops = append(ops, op)
		}
		sort.Slice(ops, func(i, j int) bool { return ops[i].String() < ops[j].String() })
		fmt.Fprintf(stdout, "  %-9s %-6s %-6s %s\n", "requests", "median", "max", "calls per request")
		for _, op := range ops {
			c := counts[op]
			n, mx := 0, 0.0
			for _, v := range c {
				if v > 0 {
					n++
				}
				mx = max(mx, v)
			}
			fmt.Fprintf(stdout, "  %-9s %-6.0f %-6.0f %s\n", fmt.Sprintf("%.0f%%", 100*float64(n)/float64(len(ts))),
				stats.Quantile(c, 0.5), mx, op)
		}
	}
	return nil
}

func cmdShow(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	svg := fs.String("svg", "", "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("need a file and a trace id")
	}
	spans, err := otlp.ReadFile(pos[0])
	if err != nil {
		return err
	}
	id := strings.ToLower(pos[1])
	for _, t := range analyze.Group(spans, false) {
		if t.ID == id || (len(id) >= 8 && strings.HasPrefix(t.ID, id)) {
			report.Tree(stdout, t)
			if *svg != "" {
				out := report.Waterfall(t.Endpoint, "trace "+t.ID, []report.Panel{{Label: t.ID, Trace: t}})
				return os.WriteFile(*svg, []byte(out), 0o644)
			}
			return nil
		}
	}
	return fmt.Errorf("trace %s not in %s", pos[1], pos[0])
}

func cmdCollect(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:4318", "")
	out := fs.String("out", "", "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("--out FILE is required")
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	rcv := otlp.NewReceiver(f)
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: rcv, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	fmt.Fprintf(stderr, "trace-diff: receiving OTLP/HTTP on http://%s/v1/traces, writing %s\n", ln.Addr(), *out)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	fmt.Fprintf(stderr, "trace-diff: wrote %d spans to %s\n", rcv.Spans(), *out)
	return nil
}

func cmdSynth(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("synth", flag.ContinueOnError)
	scenario := fs.String("scenario", "", "")
	seed := fs.Int64("seed", 1, "")
	traces := fs.Int("traces", 300, "")
	out := fs.String("out", ".", "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *scenario == "" {
		fmt.Fprintln(stdout, "scenarios:")
		for _, s := range synth.Scenarios {
			fmt.Fprintf(stdout, "  %-14s %s\n", s.Name, s.What)
		}
		return nil
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	n := synth.Traces(*scenario, *traces)
	for _, side := range []string{"before", "after"} {
		spans, err := synth.Generate(*scenario, side == "after", *seed, n)
		if err != nil {
			return err
		}
		path := filepath.Join(*out, side+".jsonl")
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		// One line per trace, the way a collector batches them.
		start := 0
		for i := 1; i <= len(spans); i++ {
			if i == len(spans) || spans[i].TraceID != spans[start].TraceID {
				if err := otlp.Write(f, spans[start:i]); err != nil {
					f.Close()
					return err
				}
				start = i
			}
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote %s: %d traces, %d spans\n", path, n, len(spans))
	}
	return nil
}

func cmdBench(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	seeds := fs.Int("seeds", 200, "")
	traces := fs.Int("traces", 300, "")
	mutants := fs.Bool("mutants", false, "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	o := analyze.Defaults()
	fmt.Fprintf(stdout, "%d seeds per scenario, %d requests per side (drift: %d)\n\n", *seeds, *traces, synth.Traces("drift", *traces))
	rs := bench.Run(*seeds, *traces, o)
	bench.Table(stdout, rs)
	if *mutants {
		fmt.Fprintln(stdout)
		bench.MutantTable(stdout, bench.Mutants(*seeds, *traces, rs))
	}
	return nil
}
