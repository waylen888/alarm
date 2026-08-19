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
		got, ok := EWMAStat(series[:i+1], 0.5)
		if !ok {
			t.Fatalf("EWMAStat over %v reported false", series[:i+1])
		}
		close(t, got, want, "EWMAStat")
	}
}

func TestEWMAStatLambdaOne(t *testing.T) {
	series := []float64{10, 12, 14, 7}
	got, ok := EWMAStat(series, 1)
	if !ok {
		t.Fatal("EWMAStat reported false")
	}
	close(t, got, 7, "λ=1 is the last observation")
}

func TestEWMAStatSmallLambdaBarelyMoves(t *testing.T) {
	// λ=0.01 over four points: the statistic stays near the seed.
	got, ok := EWMAStat([]float64{10, 20, 20, 20}, 0.01)
	if !ok {
		t.Fatal("EWMAStat reported false")
	}
	if got < 10 || got > 10.4 {
		t.Errorf("EWMAStat = %v, want just above 10", got)
	}
}

func TestEWMAStatRejectsBadInput(t *testing.T) {
	for _, lambda := range []float64{0, -0.5, 1.5, math.NaN()} {
		if _, ok := EWMAStat([]float64{1, 2, 3}, lambda); ok {
			t.Errorf("EWMAStat with lambda %v should report false", lambda)
		}
	}
	if _, ok := EWMAStat(nil, 0.2); ok {
		t.Error("EWMAStat(nil) should report false")
	}
	// A non-finite observation, not just a non-finite lambda. Without the
	// guard the statistic comes back NaN with ok true, and a NaN comparison
	// in Breach is false — the condition goes blind rather than reporting it
	// cannot judge.
	for _, bad := range [][]float64{{1, math.NaN(), 3}, {1, math.Inf(1), 3}, {math.Inf(-1), 2}} {
		if _, ok := EWMAStat(bad, 0.2); ok {
			t.Errorf("EWMAStat over %v should report false", bad)
		}
	}
}

func TestEWMAControlLimits(t *testing.T) {
	// λ=0.5, σ=2, L=3, n large: 3·2·sqrt(0.5/1.5) = 6/sqrt(3).
	steady, ok := EWMAControlLimits(2, 0.5, 3, 1000)
	if !ok {
		t.Fatal("EWMAControlLimits reported false")
	}
	close(t, steady, 6/math.Sqrt(3), "steady-state half-width")

	// n=1: the (1-(1-λ)^{2n}) factor is 1-0.25 = 0.75.
	first, ok := EWMAControlLimits(2, 0.5, 3, 1)
	if !ok {
		t.Fatal("EWMAControlLimits reported false")
	}
	close(t, first, 6*math.Sqrt(0.75/3), "half-width at n=1")
}

func TestEWMAControlLimitsWidenWithN(t *testing.T) {
	prev := 0.0
	for n := 1; n <= 40; n++ {
		w, ok := EWMAControlLimits(1, 0.2, 3, n)
		if !ok {
			t.Fatalf("EWMAControlLimits reported false at n=%d", n)
		}
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
		if _, ok := EWMAControlLimits(c.sigma, c.lambda, c.L, c.n); ok {
			t.Errorf("%s: should report false", c.name)
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
	full, _ := EWMAStat(long, lambda)
	truncated, _ := EWMAStat(long[len(long)-n:], lambda)
	if diff := math.Abs(full - truncated); diff > EWMAResidualWeight*6 {
		t.Errorf("full %v vs truncated over %d points %v: diff %v", full, n, truncated, diff)
	}
}
