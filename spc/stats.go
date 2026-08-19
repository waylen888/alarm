package spc

import (
	"math"
	"sort"
)

// MADScale scales a median absolute deviation into an estimate of the
// standard deviation. For normally distributed data the MAD converges to
// 0.6745σ, so σ ≈ MAD/0.6745 = 1.4826·MAD. The constant is what makes MAD
// comparable to StdDev, and therefore usable as the dispersion of a control
// chart whose limits are expressed in multiples of σ.
const MADScale = 1.4826

// MovingRangeScale converts a mean moving range into an estimate of the
// standard deviation. It is the d2 constant for a subgroup of two — the
// expected range of two observations from a standard normal distribution,
// E|X₁-X₂| = 2/√π = 1.12838 — carried to the four significant figures the
// published control-chart tables give, so that a reader checking against a
// table sees the same number. Sigma is estimated as MR/1.128. The constant is what makes
// MeanMovingRange comparable to StdDev, and therefore usable as the
// dispersion of a control chart whose limits are multiples of sigma.
const MovingRangeScale = 1.128

// Mean returns the arithmetic mean of xs. It reports false for an empty
// slice or a non-finite result.
func Mean(xs []float64) (float64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	m := sum / float64(len(xs))
	if !finite(m) {
		return 0, false
	}
	return m, true
}

// StdDev returns the sample standard deviation of xs, the square root of the
// sum of squared deviations divided by n-1. It reports false for fewer than
// two observations, a non-finite result, or zero variance — a zero dispersion
// is never a usable answer, because every caller divides by it.
//
// The n-1 divisor is deliberate: the observations are a sample of the
// process, not the whole of it, and the population form would understate the
// dispersion and so overstate every z it feeds.
func StdDev(xs []float64) (float64, bool) {
	if len(xs) < 2 {
		return 0, false
	}
	mean, ok := Mean(xs)
	if !ok {
		return 0, false
	}
	var sum float64
	for _, x := range xs {
		d := x - mean
		sum += d * d
	}
	sd := math.Sqrt(sum / float64(len(xs)-1))
	if !finite(sd) || sd <= 0 {
		return 0, false
	}
	return sd, true
}

// Median returns the median of xs, the mean of the two central values for an
// even count. It does not modify xs. It reports false for an empty slice or a
// non-finite result.
func Median(xs []float64) (float64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	s := make([]float64, len(xs))
	copy(s, xs)
	sort.Float64s(s)
	var m float64
	if n := len(s); n%2 == 1 {
		m = s[n/2]
	} else {
		m = (s[n/2-1] + s[n/2]) / 2
	}
	if !finite(m) {
		return 0, false
	}
	return m, true
}

// MAD returns the median absolute deviation of xs: the median of the absolute
// deviations from the median. It does not modify xs. It reports false for an
// empty slice, a non-finite result, or a zero deviation.
//
// MAD has a breakdown point of 50%: up to half the observations can be
// arbitrarily corrupted without driving it arbitrarily far. It still moves,
// by a bounded amount. A standard deviation has a breakdown point of zero —
// one outlier moves it as far as you like — which is why a reference period
// containing spikes needs this instead.
func MAD(xs []float64) (float64, bool) {
	med, ok := Median(xs)
	if !ok {
		return 0, false
	}
	dev := make([]float64, len(xs))
	for i, x := range xs {
		dev[i] = math.Abs(x - med)
	}
	mad, ok := Median(dev)
	if !ok || mad <= 0 {
		return 0, false
	}
	return mad, true
}

// MeanMovingRange returns the mean of the absolute differences between
// adjacent observations. It reports false for fewer than two observations, a
// non-finite result, or a zero result.
//
// Unlike StdDev and MAD this is a local measure of dispersion: it looks only
// at how far the series moves from one observation to the next, and so is
// blind to where the level of the series has been. That is the whole point.
// A sample standard deviation over a reference period containing a drift, a
// ramp or part of a cycle absorbs that level variation as though it were
// scatter, and returns a sigma several times larger than the noise the
// process actually has. A mean moving range does not see the level, so a
// ramp enters it only as the per-step increment rather than as the whole
// excursion.
//
// The price is the mirror image: a mean moving range cannot tell a genuinely
// noisy process from a smoothly drifting one, and on an autocorrelated series
// it underestimates the dispersion, because consecutive observations resemble
// each other more than independent ones would. Under-estimating sigma makes a
// chart more sensitive, and a chart on autocorrelated data is already too
// sensitive.
func MeanMovingRange(xs []float64) (float64, bool) {
	if len(xs) < 2 {
		return 0, false
	}
	var sum float64
	for i := 1; i < len(xs); i++ {
		sum += math.Abs(xs[i] - xs[i-1])
	}
	mr := sum / float64(len(xs)-1)
	if !finite(mr) || mr <= 0 {
		return 0, false
	}
	return mr, true
}

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
