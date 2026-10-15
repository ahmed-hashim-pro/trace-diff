package bench

import (
	"testing"

	"github.com/ahmed-hashim-pro/trace-diff/internal/analyze"
	"github.com/ahmed-hashim-pro/trace-diff/internal/synth"
)

// Every planted change is found, and nothing else is reported, on the
// first seeds. The README's table runs 200 seeds per scenario.
func TestScenarios(t *testing.T) {
	seeds := 5
	if testing.Short() {
		seeds = 2
	}
	for _, sc := range synth.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			t.Parallel()
			for seed := int64(1); seed <= int64(seeds); seed++ {
				found, noise := Check(sc.Name, seed, 300, analyze.Defaults())
				if !found {
					t.Errorf("seed %d: planted change not found", seed)
				}
				if noise != "" {
					t.Errorf("seed %d: unexpected finding: %s", seed, noise)
				}
			}
		})
	}
}

// Each broken comparison gets something visibly wrong within a few seeds.
func TestMutantsHaveTeeth(t *testing.T) {
	cases := []struct {
		mutant, scenario string
		wantMiss         bool // else: an unexpected finding
	}{
		{"keep-literals", "retry-storm", true},
		{"pooled", "none", false},
		{"no-floors", "drift", false},
		{"any-repeat-is-retry", "n-plus-one", true},
	}
	for _, c := range cases {
		t.Run(c.mutant, func(t *testing.T) {
			t.Parallel()
			o := analyze.Defaults()
			o.Mutant = c.mutant
			for seed := int64(1); seed <= 10; seed++ {
				found, noise := Check(c.scenario, seed, 300, o)
				if (c.wantMiss && !found) || (!c.wantMiss && noise != "") {
					return
				}
			}
			t.Errorf("%s on %s: no visible effect in 10 seeds", c.mutant, c.scenario)
		})
	}
}
