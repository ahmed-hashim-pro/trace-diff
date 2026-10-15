// Package stats has the three tests trace-diff decides with: Mann-Whitney U
// for "per-request numbers moved", Fisher's exact test for "this happens in
// more requests than before", and Benjamini-Hochberg to keep the false
// discovery rate down when a comparison runs hundreds of tests at once.
package stats

import (
	"math"
	"sort"
)

// MannWhitney compares two samples without assuming a distribution. It
// returns one-sided p-values for "b tends to be larger than a" and "b tends
// to be smaller", using the normal approximation with a tie correction and a
// continuity correction. Counts per request are full of ties (most are 0 or
// 1), so the tie correction matters. With no variation at all, both p-values
// are 1.
func MannWhitney(a, b []float64) (pGreater, pLess float64) {
	na, nb := len(a), len(b)
	if na == 0 || nb == 0 {
		return 1, 1
	}
	type obs struct {
		v float64
		b bool
	}
	all := make([]obs, 0, na+nb)
	for _, v := range a {
		all = append(all, obs{v, false})
	}
	for _, v := range b {
		all = append(all, obs{v, true})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v < all[j].v })
	n := float64(na + nb)
	var rankB, tieSum float64
	for i := 0; i < len(all); {
		j := i
		for j < len(all) && all[j].v == all[i].v {
			j++
		}
		rank := float64(i+j+1) / 2 // average of ranks i+1..j
		t := float64(j - i)
		tieSum += t*t*t - t
		for k := i; k < j; k++ {
			if all[k].b {
				rankB += rank
			}
		}
		i = j
	}
	fa, fb := float64(na), float64(nb)
	u := rankB - fb*(fb+1)/2
	mean := fa * fb / 2
	variance := fa * fb / 12 * ((n + 1) - tieSum/(n*(n-1)))
	if variance <= 0 {
		return 1, 1
	}
	sd := math.Sqrt(variance)
	pGreater = upperTail((u - mean - 0.5) / sd)
	pLess = upperTail((mean - u - 0.5) / sd)
	return pGreater, pLess
}

// upperTail is P(Z >= z) for a standard normal Z.
func upperTail(z float64) float64 {
	return 0.5 * math.Erfc(z/math.Sqrt2)
}

// FisherGreater is the one-sided Fisher exact p-value for "the rate in b
// (kb of nb) is higher than the rate in a (ka of na)": the probability, with
// the margins fixed, of b holding kb or more of the successes.
func FisherGreater(ka, na, kb, nb int) float64 {
	k := ka + kb
	n := na + nb
	hi := min(k, nb)
	var p float64
	for x := kb; x <= hi; x++ {
		p += math.Exp(logHyper(x, nb, k, n))
	}
	return min(p, 1)
}

// logHyper is log P(X = x) for X ~ Hypergeometric(population n, k successes,
// draws m).
func logHyper(x, m, k, n int) float64 {
	return logChoose(k, x) + logChoose(n-k, m-x) - logChoose(n, m)
}

func logChoose(n, k int) float64 {
	if k < 0 || k > n {
		return math.Inf(-1)
	}
	a, _ := math.Lgamma(float64(n + 1))
	b, _ := math.Lgamma(float64(k + 1))
	c, _ := math.Lgamma(float64(n - k + 1))
	return a - b - c
}

// BH returns Benjamini-Hochberg adjusted p-values, in the input order. A test
// is significant at false discovery rate q when its adjusted value is <= q.
func BH(p []float64) []float64 {
	m := len(p)
	idx := make([]int, m)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool { return p[idx[i]] < p[idx[j]] })
	adj := make([]float64, m)
	running := 1.0
	for r := m - 1; r >= 0; r-- {
		v := p[idx[r]] * float64(m) / float64(r+1)
		running = min(running, v)
		adj[idx[r]] = running
	}
	return adj
}

// Quantile is the q-th quantile (0..1) of v by linear interpolation. It
// sorts a copy, so v is left alone.
func Quantile(v []float64, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	pos := q * float64(len(s)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return s[lo] + (s[hi]-s[lo])*(pos-float64(lo))
}

// Mean of v, 0 when empty.
func Mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}
