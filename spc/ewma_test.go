package spc

import (
	"math"
	"testing"
)

// The recursion is pinned against a hand-computed series rather than assumed:
// λ=0.5, seeded at 10; z₁ = 0.5·12 + 0.5·10 = 11; z₂ = 0.5·14 + 0.5·11 = 12.5;
// z₃ = 0.5·10 + 0.5·12.5 = 11.25.
func TestEWMAStatAgainstAHandComputedSeries(t *testing.T) {
	series := []float64{10, 12, 14, 10}
	for i, want := range []float64{10, 11, 12.5, 11.25} {
		close(t, EWMAStat(series[:i+1], 0.5), want, "EWMAStat")
	}
}

func TestEWMAStatLambdaOne(t *testing.T) {
	series := []float64{10, 12, 14, 7}
	close(t, EWMAStat(series, 1), 7, "λ=1 is the last observation")
}

func TestEWMAStatSmallLambdaBarelyMoves(t *testing.T) {
	// λ=0.01 over four points: the statistic stays near the seed.
	got := EWMAStat([]float64{10, 20, 20, 20}, 0.01)
	if got < 10 || got > 10.4 {
		t.Errorf("EWMAStat = %v, want just above 10", got)
	}
}

func TestEWMAStatRejectsBadInput(t *testing.T) {
	for _, lambda := range []float64{0, -0.5, 1.5, math.NaN()} {
		if got := EWMAStat([]float64{1, 2, 3}, lambda); got != 0 {
			t.Errorf("EWMAStat with lambda %v = %v, want 0", lambda, got)
		}
	}
	if got := EWMAStat(nil, 0.2); got != 0 {
		t.Errorf("EWMAStat(nil) = %v, want 0", got)
	}
}

func TestEWMAControlLimits(t *testing.T) {
	// λ=0.5, σ=2, L=3, n large: 3·2·sqrt(0.5/1.5) = 6/sqrt(3).
	close(t, EWMAControlLimits(2, 0.5, 3, 1000), 6/math.Sqrt(3), "steady-state half-width")

	// n=1: the (1-(1-λ)^{2n}) factor is 1-0.25 = 0.75.
	close(t, EWMAControlLimits(2, 0.5, 3, 1), 6*math.Sqrt(0.75/3), "half-width at n=1")
}

func TestEWMAControlLimitsWidenWithN(t *testing.T) {
	prev := 0.0
	for n := 1; n <= 40; n++ {
		w := EWMAControlLimits(1, 0.2, 3, n)
		if w <= prev {
			t.Fatalf("half-width at n=%d is %v, not wider than %v", n, w, prev)
		}
		prev = w
	}
	steady := 3 * math.Sqrt(0.2/1.8)
	if math.Abs(prev-steady) > 0.01 {
		t.Errorf("half-width at n=40 is %v, should have converged on %v", prev, steady)
	}
}

func TestEWMAControlLimitsRejectBadInput(t *testing.T) {
	cases := []struct {
		name             string
		sigma, lambda, L float64
		n                int
	}{
		{"sigma zero", 0, 0.2, 3, 20},
		{"sigma negative", -1, 0.2, 3, 20},
		{"lambda zero", 1, 0, 3, 20},
		{"lambda above one", 1, 1.5, 3, 20},
		{"L zero", 1, 0.2, 0, 20},
		{"n zero", 1, 0.2, 3, 0},
		{"sigma NaN", math.NaN(), 0.2, 3, 20},
	}
	for _, c := range cases {
		if got := EWMAControlLimits(c.sigma, c.lambda, c.L, c.n); got != 0 {
			t.Errorf("%s: half-width = %v, want 0", c.name, got)
		}
	}
}

// MinPoints is derived from lambda, not chosen. These are the arithmetic, not
// a table someone typed in.
func TestEWMAMinPointsIsDerived(t *testing.T) {
	for _, c := range []struct {
		lambda float64
		want   int
	}{
		{1, 1},
		{0.5, 7},   // ceil(ln0.01/ln0.5)  = ceil(6.64)
		{0.2, 21},  // ceil(ln0.01/ln0.8)  = ceil(20.6)
		{0.1, 44},  // ceil(ln0.01/ln0.9)  = ceil(43.7)
		{0.05, 90}, // ceil(ln0.01/ln0.95) = ceil(89.8)
	} {
		if got := EWMAMinPoints(c.lambda); got != c.want {
			t.Errorf("EWMAMinPoints(%v) = %d, want %d", c.lambda, got, c.want)
		}
	}
	for _, bad := range []float64{0, -1, 1.5, math.NaN()} {
		if got := EWMAMinPoints(bad); got != 0 {
			t.Errorf("EWMAMinPoints(%v) = %d, want 0", bad, got)
		}
	}
}

// The property MinPoints exists to guarantee: at n points the discarded
// history is worth less than EWMAResidualWeight, and one point fewer is not
// enough.
func TestEWMAMinPointsBoundsTheDiscardedWeight(t *testing.T) {
	for _, lambda := range []float64{0.05, 0.1, 0.2, 0.3, 0.5, 0.8} {
		n := EWMAMinPoints(lambda)
		if w := math.Pow(1-lambda, float64(n)); w >= EWMAResidualWeight {
			t.Errorf("lambda %v: weight at n=%d is %v, not below %v", lambda, n, w, EWMAResidualWeight)
		}
		if w := math.Pow(1-lambda, float64(n-1)); w < EWMAResidualWeight {
			t.Errorf("lambda %v: n=%d is larger than it needs to be", lambda, n)
		}
	}
}

// The truncated statistic and one carried forward from a much longer history
// must agree to within the residual weight's worth of the series range.
func TestEWMAStatTruncationIsNegligible(t *testing.T) {
	const lambda = 0.2
	long := make([]float64, 500)
	for i := range long {
		long[i] = 100 + float64(i%7)
	}
	n := EWMAMinPoints(lambda)
	full := EWMAStat(long, lambda)
	truncated := EWMAStat(long[len(long)-n:], lambda)
	if diff := math.Abs(full - truncated); diff > EWMAResidualWeight*6 {
		t.Errorf("full %v vs truncated over %d points %v: diff %v", full, n, truncated, diff)
	}
}
