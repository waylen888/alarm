package spc

import (
	"math"
	"testing"
)

// steady returns n observations of a process centred on 100 with a fixed,
// deterministic jitter of at most ±0.9. Its sample standard deviation is
// ≈0.54 and its MAD·MADScale ≈0.67, close enough that the two baselines agree
// on clean data and any divergence in a test is caused by what that test
// introduced.
func steady(n int) []float64 {
	off := []float64{0.3, -0.5, 0.8, -0.2, 0.1, -0.9, 0.6, -0.4, 0.2, -0.7, 0.5, -0.1, 0.9, -0.3, 0.4, -0.6}
	xs := make([]float64, n)
	for i := range xs {
		xs[i] = 100 + off[i%len(off)]
	}
	return xs
}

// z is the sigma distance of x from the baseline estimated over ref. It
// reports false when the baseline cannot be estimated.
func z(b Baseline, ref []float64, x float64) (float64, bool) {
	centre, sigma, ok := b.Estimate(ref)
	if !ok {
		return 0, false
	}
	return (x - centre) / sigma, true
}

func TestFixed(t *testing.T) {
	c, s, ok := Fixed(10, 2).Estimate(nil)
	if !ok {
		t.Fatal("Fixed reported false")
	}
	close(t, c, 10, "centre")
	close(t, s, 2, "sigma")

	for _, bad := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if _, _, ok := Fixed(10, bad).Estimate(nil); ok {
			t.Errorf("Fixed with sigma %v should report false", bad)
		}
	}
	if _, _, ok := Fixed(math.NaN(), 1).Estimate(nil); ok {
		t.Error("Fixed with a NaN centre should report false")
	}
}

func TestTrailing(t *testing.T) {
	// mean 100; deviations -1,0,1,2; sum of squares 6; /3 = 2.
	ref := []float64{99, 100, 101, 102}
	c, s, ok := Trailing(4).Estimate(ref)
	if !ok {
		t.Fatal("Trailing reported false")
	}
	close(t, c, 100.5, "centre")
	close(t, s, math.Sqrt(5.0/3.0), "sigma")

	if _, _, ok := Trailing(32).Estimate(steady(31)); ok {
		t.Error("Trailing(32) with 31 observations should report false")
	}
	if _, _, ok := Trailing(8).Estimate(make([]float64, 8)); ok {
		t.Error("Trailing over a constant series should report false, not sigma 0")
	}
}

func TestTrailingRobust(t *testing.T) {
	// median 3; deviations 2,1,0,1,2; median of those 1.
	ref := []float64{1, 2, 3, 4, 5}
	c, s, ok := TrailingRobust(5).Estimate(ref)
	if !ok {
		t.Fatal("TrailingRobust reported false")
	}
	close(t, c, 3, "centre")
	close(t, s, MADScale, "sigma")

	if _, _, ok := TrailingRobust(32).Estimate(steady(31)); ok {
		t.Error("TrailingRobust(32) with 31 observations should report false")
	}
}

// A trailing baseline uses the n observations adjacent to the test points,
// not the oldest n: a baseline drawn from an hour ago describes a process
// that may no longer exist.
func TestTrailingUsesTheMostRecentReferenceObservations(t *testing.T) {
	ref := append(steady(16), 199.5, 200.5, 199.5, 200.5)
	c, _, ok := Trailing(4).Estimate(ref)
	if !ok {
		t.Fatal("Trailing reported false")
	}
	close(t, c, 200, "centre")
}

func TestTrailingClampsNBelowTwo(t *testing.T) {
	for _, n := range []int{-5, 0, 1} {
		if got := Trailing(n).(RefSizer).RefPoints(); got != 2 {
			t.Errorf("Trailing(%d).RefPoints() = %d, want 2", n, got)
		}
		if got := TrailingRobust(n).(RefSizer).RefPoints(); got != 2 {
			t.Errorf("TrailingRobust(%d).RefPoints() = %d, want 2", n, got)
		}
	}
}

func TestRefPointsOf(t *testing.T) {
	if got := refPointsOf(Fixed(1, 1), 7); got != 7 {
		t.Errorf("Fixed declares nothing, so the caller's ref stands: got %d, want 7", got)
	}
	if got := refPointsOf(Trailing(50), 10); got != 50 {
		t.Errorf("a baseline's larger requirement must win: got %d, want 50", got)
	}
	if got := refPointsOf(Trailing(10), 50); got != 50 {
		t.Errorf("the caller may ask for more than the baseline needs: got %d, want 50", got)
	}
}

// The bug this design exists to prevent: a sustained shift allowed into its
// own reference period drags the centre after it and inflates sigma, and the
// chart goes blind to the shift.
func TestBaselinePollution(t *testing.T) {
	const shift = 106
	ref := steady(30)
	test := []float64{shift, shift, shift, shift, shift, shift, shift, shift, shift}

	clean, ok := z(Trailing(30), ref, shift)
	if !ok {
		t.Fatal("clean baseline reported false")
	}
	if clean <= 3 {
		t.Errorf("with the test points excluded, z = %v; want > 3 (the shift must be visible)", clean)
	}

	polluted, ok := z(Trailing(39), append(append([]float64(nil), ref...), test...), shift)
	if !ok {
		t.Fatal("polluted baseline reported false")
	}
	if polluted > 3 {
		t.Errorf("with the test points included, z = %v; want <= 3 — this test documents the bug, "+
			"so if it starts passing the exclusion has stopped being load-bearing", polluted)
	}
	t.Logf("z with the shift excluded from its own baseline: %.2f; included: %.2f", clean, polluted)
}

// The reason TrailingRobust exists: one outlier in the reference period
// inflates Trailing's sigma enough to mask a genuine shift.
func TestRobustBaselineSurvivesAnOutlierInTheReference(t *testing.T) {
	ref := steady(32)
	ref[7] = 120 // a single spike in the reference period
	const shift = 103

	plain, ok := z(Trailing(32), ref, shift)
	if !ok {
		t.Fatal("Trailing reported false")
	}
	if plain > 3 {
		t.Errorf("Trailing z = %v; want <= 3 — the outlier is supposed to mask the shift here", plain)
	}

	robust, ok := z(TrailingRobust(32), ref, shift)
	if !ok {
		t.Fatal("TrailingRobust reported false")
	}
	if robust <= 3 {
		t.Errorf("TrailingRobust z = %v; want > 3", robust)
	}
	t.Logf("one outlier in the reference: Trailing z = %.2f, TrailingRobust z = %.2f", plain, robust)
}
