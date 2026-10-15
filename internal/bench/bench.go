// Package bench measures trace-diff on the synthetic shop: for every
// scenario and seed it generates a before and an after run, compares them,
// and checks the findings against the change that was planted.
package bench

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/ahmed-hashim-pro/trace-diff/internal/analyze"
	"github.com/ahmed-hashim-pro/trace-diff/internal/synth"
)

// expectation is what a scenario should produce: one finding of a kind
// about a matching op, and nothing outside the allowed collateral (a new
// query that replaces an old one is also a removed call, and most planted
// changes also move latency).
type expectation struct {
	kind    string
	match   string // substring of the finding's op, title or first line
	allowed []string
}

var expect = map[string]*expectation{
	"none":          nil,
	"n-plus-one":    {analyze.NPlusOne, "order_items WHERE id", []string{analyze.NewSyncCall, analyze.NewCall, analyze.Removed, analyze.Fewer, analyze.Slower}},
	"new-sync-call": {analyze.NewSyncCall, "inventory", []string{analyze.Slower}},
	"new-hop":       {analyze.NewSyncCall, "orders · client · GET users ", []string{analyze.Removed, analyze.Fewer, analyze.Slower, analyze.Faster}},
	"retry-storm":   {analyze.RetryStorm, "pricing", []string{analyze.Slower, analyze.MoreCalls}},
	"fan-out":       {analyze.FanOut, "rates", []string{analyze.Slower, analyze.MoreCalls}},
	"latency":       {analyze.Slower, "FROM orders WHERE id", nil},
	"drift":         nil,
}

// Result is one scenario over many seeds.
type Result struct {
	Scenario   string
	Seeds      int
	Found      int // the planted change, named correctly
	Noisy      int // seeds with any finding that is neither planted nor allowed
	NoisyFail  int // ... of which at least one would fail `check`
	FirstMiss  int // first seed where the planted change was missed, or 0
	FirstNoise int
	Example    string // the first unexpected finding
}

// Run measures every scenario over seeds 1..n with traces per side.
func Run(seeds, traces int, o analyze.Options) []Result {
	var out []Result
	for _, sc := range synth.Scenarios {
		out = append(out, runScenario(sc.Name, seeds, traces, o))
	}
	return out
}

func runScenario(name string, seeds, traces int, o analyze.Options) Result {
	res := Result{Scenario: name, Seeds: seeds}
	type one struct {
		found     bool
		noise     string
		noiseFail bool
	}
	outcomes := make([]one, seeds)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for s := 1; s <= seeds; s++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(seed int) {
			defer wg.Done()
			defer func() { <-sem }()
			found, noise, fail := check(name, int64(seed), traces, o)
			outcomes[seed-1] = one{found, noise, fail}
		}(s)
	}
	wg.Wait()
	for i, oc := range outcomes {
		if oc.found {
			res.Found++
		} else if res.FirstMiss == 0 {
			res.FirstMiss = i + 1
		}
		if oc.noiseFail {
			res.NoisyFail++
		}
		if oc.noise != "" {
			res.Noisy++
			if res.FirstNoise == 0 {
				res.FirstNoise = i + 1
				res.Example = oc.noise
			}
		}
	}
	return res
}

// Check runs one scenario and seed: whether the planted change was found,
// and the first finding that should not be there, if any.
func Check(name string, seed int64, traces int, o analyze.Options) (found bool, noise string) {
	found, noise, _ = check(name, seed, traces, o)
	return found, noise
}

func check(name string, seed int64, traces int, o analyze.Options) (found bool, noise string, noiseFail bool) {
	n := synth.Traces(name, traces)
	before, err := synth.Generate(name, false, seed, n)
	if err != nil {
		panic(err)
	}
	after, _ := synth.Generate(name, true, seed, n)
	r := analyze.Compare(before, after, "before", "after", o)
	e := expect[name]
	for _, f := range r.Findings {
		if e != nil && Matches(f, e.kind, e.match) {
			found = true
			continue
		}
		ok := false
		if e != nil {
			for _, k := range e.allowed {
				ok = ok || f.Kind == k
			}
		}
		if !ok {
			if noise == "" {
				noise = f.String()
			}
			noiseFail = noiseFail || f.Severity == "fail"
		}
	}
	if e == nil {
		found = true
	}
	return found, noise, noiseFail
}

// Matches reports whether f is a finding of kind about something matching.
func Matches(f analyze.Finding, kind, match string) bool {
	if f.Kind != kind {
		return false
	}
	text := f.Op.String() + "\n" + f.Title
	if len(f.Lines) > 1 && kind == analyze.Slower {
		text += "\n" + f.Lines[1] // the top contributor to the shift
	}
	return strings.Contains(text, match)
}

// Table writes results as the README shows them.
func Table(w io.Writer, rs []Result) {
	fmt.Fprintf(w, "%-15s %-9s %-12s %-9s %s\n", "scenario", "found", "unexpected", "failing", "first unexpected finding")
	for _, r := range rs {
		found := fmt.Sprintf("%d/%d", r.Found, r.Seeds)
		if expect[r.Scenario] == nil {
			found = "-"
		}
		first := ""
		if r.Noisy > 0 {
			first = fmt.Sprintf("seed %d: %s", r.FirstNoise, r.Example)
		}
		line := fmt.Sprintf("%-15s %-9s %-12s %-9s %s", r.Scenario, found,
			fmt.Sprintf("%d/%d", r.Noisy, r.Seeds), fmt.Sprintf("%d/%d", r.NoisyFail, r.Seeds), first)
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
}

// MutantRow summarizes what a broken comparison does to the results.
type MutantRow struct {
	Mutant, What string
	Caught       bool
	Effect       string
}

// Mutants runs every scenario against every mutant and reports, for each,
// the clearest way the results got worse than the correct comparison's.
func Mutants(seeds, traces int, base []Result) []MutantRow {
	byName := map[string]Result{}
	for _, r := range base {
		byName[r.Scenario] = r
	}
	var rows []MutantRow
	for _, m := range analyze.Mutants {
		o := analyze.Defaults()
		o.Mutant = m.Name
		rs := Run(seeds, traces, o)
		row := MutantRow{Mutant: m.Name, What: m.What}
		var effects []string
		for _, r := range rs {
			b := byName[r.Scenario]
			if expect[r.Scenario] != nil && r.Found < b.Found {
				effects = append(effects, fmt.Sprintf("%s found %d/%d", r.Scenario, r.Found, r.Seeds))
			}
			if r.Noisy > b.Noisy {
				effects = append(effects, fmt.Sprintf("%s unexpected on %d/%d", r.Scenario, r.Noisy, r.Seeds))
			}
		}
		row.Caught = len(effects) > 0
		if row.Caught {
			if len(effects) > 3 {
				effects = append(effects[:3], fmt.Sprintf("and %d more", len(effects)-3))
			}
			row.Effect = strings.Join(effects, ", ")
		} else {
			row.Effect = "no difference on these scenarios"
		}
		rows = append(rows, row)
	}
	return rows
}

// MutantTable writes the mutant rows.
func MutantTable(w io.Writer, rows []MutantRow) {
	fmt.Fprintf(w, "%-20s %-8s %s\n", "mutant", "result", "what changed")
	for _, r := range rows {
		res := "MISSED"
		if r.Caught {
			res = "caught"
		}
		fmt.Fprintf(w, "%-20s %-8s %s\n", r.Mutant, res, r.Effect)
	}
}
