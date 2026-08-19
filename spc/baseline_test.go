package spc

import (
	"math"
	"testing"
)

// quoted pins the sigma distances the package documentation and both READMEs
// print for this fixture, in the order they are printed. The assertions above
// only check which side of three each lands on, so without this the quoted
// figures could drift silently — which is the failure the documentation's own
// reproducibility rule exists to prevent.
func quoted(t *testing.T, what string, plain, robust, local, wantPlain, wantRobust, wantLocal float64) {
	t.Helper()
	for _, c := range []struct {
		name      string
		got, want float64
	}{
		{"Trailing", plain, wantPlain},
		{"TrailingRobust", robust, wantRobust},
		{"TrailingRange", local, wantLocal},
	} {
		// The documentation quotes one decimal, so compare at one decimal
		// rather than with a tolerance — 4.05 against a documented 4.1 is
		// correct and a tolerance of 0.05 rejects it by a rounding error.
		if math.Round(c.got*10)/10 != c.want {
			t.Errorf("%s, %s: z = %.2f, documented as %.1f", what, c.name, c.got, c.want)
		}
	}
}

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
	// Asymmetric on purpose: mean 105, median 100.5, so a centre estimated
	// with the wrong statistic is visible here. Deviations from the mean are
	// -6, -5, -4, 15; sum of squares 302; /3.
	ref := []float64{99, 100, 101, 120}
	c, s, ok := Trailing(4).Estimate(ref)
	if !ok {
		t.Fatal("Trailing reported false")
	}
	close(t, c, 105, "centre")
	close(t, s, math.Sqrt(302.0/3.0), "sigma")

	if _, _, ok := Trailing(32).Estimate(steady(31)); ok {
		t.Error("Trailing(32) with 31 observations should report false")
	}
	if _, _, ok := Trailing(8).Estimate(make([]float64, 8)); ok {
		t.Error("Trailing over a constant series should report false, not sigma 0")
	}
}

func TestTrailingRobust(t *testing.T) {
	// Asymmetric on purpose: median 3, mean 5, so a centre estimated with the
	// wrong statistic is visible here. Deviations from the median are
	// 2,1,0,1,9; median of those 1.
	ref := []float64{1, 2, 3, 4, 13}
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

func TestTrailingRange(t *testing.T) {
	// mean 3.75; moving ranges 3, 2, 6, mean 11/3.
	ref := []float64{1, 4, 2, 8}
	c, sig, ok := TrailingRange(4).Estimate(ref)
	if !ok {
		t.Fatal("TrailingRange reported false")
	}
	close(t, c, 3.75, "centre")
	close(t, sig, (11.0/3.0)/MovingRangeScale, "sigma")

	if _, _, ok := TrailingRange(32).Estimate(steady(31)); ok {
		t.Error("TrailingRange(32) with 31 observations should report false")
	}
	if _, _, ok := TrailingRange(8).Estimate(make([]float64, 8)); ok {
		t.Error("TrailingRange over a constant series should report false, not sigma 0")
	}
}

// TrailingRobust's selling point is a centre line with a 50% breakdown point,
// and it goes untested by any symmetric fixture, where the mean and the median
// coincide by construction.
func TestTrailingRobustCentreResistsAnOutlier(t *testing.T) {
	ref := steady(32)
	ref[7] = 300 // one corrupted observation, two hundred units out

	plain, _, ok := Trailing(32).Estimate(ref)
	if !ok {
		t.Fatal("Trailing reported false")
	}
	robust, _, ok := TrailingRobust(32).Estimate(ref)
	if !ok {
		t.Fatal("TrailingRobust reported false")
	}

	if math.Abs(robust-100) > 1 {
		t.Errorf("TrailingRobust centre = %v; a median must barely move, want about 100", robust)
	}
	if math.Abs(plain-100) < 3 {
		t.Errorf("Trailing centre = %v; a mean must have been dragged by the outlier", plain)
	}
	t.Logf("one outlier in the reference: mean centre %.2f, median centre %.2f", plain, robust)
}

// The reason TrailingRange exists: drift inside the reference period is
// absorbed into a sample standard deviation as though it were noise, and the
// inflated sigma hides a real shift. A moving range does not see the level,
// so it estimates the scatter the process actually has.
func TestTrailingRangeSeesThroughDriftInTheReference(t *testing.T) {
	// The level climbs 100 -> 125 across the reference period while the
	// scatter stays around half a unit. The test point sits four units above
	// where the reference left off.
	ref := make([]float64, 50)
	for i := range ref {
		ref[i] = 100 + 0.5*float64(i) + steady(50)[i] - 100
	}
	shift := ref[len(ref)-1] + 4

	plain, ok := z(Trailing(50), ref, shift)
	if !ok {
		t.Fatal("Trailing reported false")
	}
	if plain > 3 {
		t.Errorf("Trailing z = %v; want <= 3 — the drift is supposed to mask the shift here", plain)
	}

	robust, ok := z(TrailingRobust(50), ref, shift)
	if !ok {
		t.Fatal("TrailingRobust reported false")
	}
	if robust > 3 {
		t.Errorf("TrailingRobust z = %v; want <= 3 — a median and MAD are equally blind to drift", robust)
	}

	local, ok := z(TrailingRange(50), ref, shift)
	if !ok {
		t.Fatal("TrailingRange reported false")
	}
	if local <= 3 {
		t.Errorf("TrailingRange z = %v; want > 3", local)
	}
	t.Logf("drift in the reference: Trailing z = %.2f, TrailingRobust z = %.2f, TrailingRange z = %.2f",
		plain, robust, local)
	quoted(t, "drift in the reference", plain, robust, local, 2.2, 1.8, 19.2)
}

// The honest ordering on the other failure mode. A single outlier contributes
// two moving ranges out of n-1, so TrailingRange resists it far better than
// Trailing and nothing like as well as TrailingRobust. Neither of the two
// baselines that survive drift survives outliers, and vice versa.
func TestTrailingRangeIsOnlyPartlyResistantToAnOutlier(t *testing.T) {
	ref := steady(50)
	ref[20] = 130
	const shift = 103

	plain, _ := z(Trailing(50), ref, shift)
	local, _ := z(TrailingRange(50), ref, shift)
	robust, _ := z(TrailingRobust(50), ref, shift)

	if !(plain < local && local < robust) {
		t.Errorf("expected Trailing < TrailingRange < TrailingRobust, got %.2f, %.2f, %.2f", plain, local, robust)
	}
	if local > 3 {
		t.Errorf("TrailingRange z = %v; a single large outlier still masks the shift for it", local)
	}
	// Logged in the order the documentation quotes them — Trailing,
	// TrailingRobust, TrailingRange — so that anyone updating the prose from
	// this line cannot transpose the last two, which has happened once.
	t.Logf("one outlier: Trailing z = %.2f, TrailingRobust z = %.2f, TrailingRange z = %.2f", plain, robust, local)
	quoted(t, "one outlier", plain, robust, local, 0.6, 4.0, 1.3)
}

// A low-cardinality integer metric has a MAD of zero as soon as more than half
// the reference shares a value, which leaves TrailingRobust unable to estimate
// and any condition built on it permanently false. A moving range exists as
// long as any two adjacent observations differ.
func TestTrailingRangeEstimatesWhereMADCannot(t *testing.T) {
	counter := make([]float64, 50)
	for i := range counter {
		if i%7 == 0 {
			counter[i] = 1
		}
	}
	if _, _, ok := TrailingRobust(50).Estimate(counter); ok {
		t.Error("TrailingRobust should not be able to estimate a mostly-zero counter")
	}
	if _, _, ok := TrailingRange(50).Estimate(counter); !ok {
		t.Error("TrailingRange should be able to estimate a mostly-zero counter")
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
		if got := TrailingRange(n).(RefSizer).RefPoints(); got != 2 {
			t.Errorf("TrailingRange(%d).RefPoints() = %d, want 2", n, got)
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
