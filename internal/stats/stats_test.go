package stats

import (
	"math"
	"testing"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// Fisher's tea-tasting table: 3 of 4 right in one group, 1 of 4 in the
// other. The one-sided p-value is 17/70.
func TestFisherTeaTasting(t *testing.T) {
	if p := FisherGreater(1, 4, 3, 4); !near(p, 17.0/70, 1e-12) {
		t.Fatalf("p = %v, want %v", p, 17.0/70)
	}
	if p := FisherGreater(0, 100, 0, 100); p != 1 {
		t.Fatalf("no successes anywhere: p = %v, want 1", p)
	}
	if p := FisherGreater(0, 200, 200, 200); p > 1e-50 {
		t.Fatalf("0/200 vs 200/200: p = %v", p)
	}
}

// Against scipy.stats.mannwhitneyu(b, a, alternative="greater",
// method="asymptotic", use_continuity=True), which uses the same tie and
// continuity corrections.
func TestMannWhitneyMatchesScipy(t *testing.T) {
	a := []float64{1, 1, 1, 2, 2, 3}
	b := []float64{1, 2, 3, 3, 4, 5, 5}
	g, l := MannWhitney(a, b)
	if !near(g, 0.028222, 1e-5) {
		t.Errorf("greater p = %.6f, want 0.028222", g)
	}
	if !near(l, 0.980028, 1e-5) {
		t.Errorf("less p = %.6f, want 0.980028", l)
	}
	if g, l := MannWhitney([]float64{1, 1, 1}, []float64{1, 1}); g != 1 || l != 1 {
		t.Errorf("all tied: %v %v, want 1 1", g, l)
	}
}

func TestBH(t *testing.T) {
	adj := BH([]float64{0.01, 0.04, 0.03, 0.005})
	want := []float64{0.02, 0.04, 0.04, 0.02}
	for i := range want {
		if !near(adj[i], want[i], 1e-12) {
			t.Fatalf("adj = %v, want %v", adj, want)
		}
	}
}

func TestQuantile(t *testing.T) {
	v := []float64{4, 1, 3, 2}
	if q := Quantile(v, 0.5); q != 2.5 {
		t.Errorf("median = %v", q)
	}
	if q := Quantile(v, 1); q != 4 {
		t.Errorf("max = %v", q)
	}
	if v[0] != 4 {
		t.Errorf("input was sorted in place")
	}
}
