package spc

import "math"

// EWMAResidualWeight is the largest weight the discarded history is allowed
// to carry in an EWMA computed over a bounded window.
//
// An EWMA is recursive and in principle remembers the whole process history.
// Recomputed over the last n observations it remembers only those. Because
// the recursion is seeded at the oldest retained observation rather than
// started from zero, the difference from a carried-forward statistic works
// out to exactly (1-λ)^n times the gap between that seed and the value the
// statistic would have held before it — so (1-λ)^n is the weight of the
// history that was dropped, not the weight of any observation that was kept
// (the oldest retained one carries (1-λ)^{n-1}).
//
// One percent is small enough that the truncated statistic and the
// carried-forward one differ by far less than the control limits they are
// judged against, and it is a round number a reader can argue with. See
// EWMAMinPoints.
const EWMAResidualWeight = 0.01

// EWMAStat returns the exponentially weighted moving average of series with
// smoothing factor lambda, seeded at the first observation:
//
//	z₀ = series[0]
//	zᵢ = λ·series[i] + (1-λ)·zᵢ₋₁
//
// λ near 1 discards history and the statistic approaches the last raw
// observation; λ near 0 smooths heavily and reacts slowly. It reports false
// for an empty series or a lambda outside (0,1]; a bare zero would be
// indistinguishable from a perfectly ordinary EWMA.
//
// Seeding at series[0] rather than at the centre line keeps the function
// independent of any baseline, which is what lets it live in this layer. The
// seed is the oldest retained observation and carries (1-λ)^{n-1} of the
// result, which at λ=0.2 and n=21 is 1.15% — above EWMAResidualWeight, which
// bounds something else: the weight of the history a bounded window discards.
// See EWMAResidualWeight.
func EWMAStat(series []float64, lambda float64) (float64, bool) {
	if len(series) == 0 || !finite(lambda) || lambda <= 0 || lambda > 1 {
		return 0, false
	}
	z := series[0]
	for _, x := range series[1:] {
		z = lambda*x + (1-lambda)*z
	}
	if !finite(z) {
		return 0, false
	}
	return z, true
}

// EWMAControlLimits returns the half-width of the EWMA control limits at
// observation n:
//
//	L·σ·sqrt( λ/(2-λ) · (1-(1-λ)^{2n}) )
//
// The statistic is in control while it stays within halfWidth of the centre
// line. The steady-state form is the same expression without the
// (1-(1-λ)^{2n}) factor, which only matters for the first few observations,
// where the limits are genuinely narrower because the statistic has not yet
// accumulated its full variance. Using the steady-state width from the start
// makes the chart insensitive exactly when a process is most likely to be
// out of control.
//
// L trades sensitivity against false alarms in the same way three-sigma
// limits do on a Shewhart chart; 3 is the conventional value, and lowering it
// detects smaller shifts sooner at the cost of more false alarms.
//
// It reports false when any argument is out of range, meaning no judgement
// is possible — which is not the same as a limit of zero, and every caller
// divides the difference by it or compares against it.
func EWMAControlLimits(sigma, lambda, L float64, n int) (halfWidth float64, ok bool) {
	if !finite(sigma) || sigma <= 0 || !finite(lambda) || lambda <= 0 || lambda > 1 ||
		!finite(L) || L <= 0 || n < 1 {
		return 0, false
	}
	steady := lambda / (2 - lambda)
	startup := 1 - math.Pow(1-lambda, 2*float64(n))
	w := L * sigma * math.Sqrt(steady*startup)
	if !finite(w) || w <= 0 {
		return 0, false
	}
	return w, true
}

// EWMAMinPoints returns how many observations an EWMA with smoothing factor
// lambda must be computed over before window truncation is negligible: the
// smallest n for which the discarded history's weight (1-λ)^n falls below
// EWMAResidualWeight.
//
//	n = ceil( ln(EWMAResidualWeight) / ln(1-λ) )
//
// For λ=0.2 that is 21 observations, for λ=0.5 it is 7, and for λ=0.05 it is
// 90. A condition sized below this number is not computing the EWMA it claims
// to; a condition sized well above it is paying for observations that carry
// no weight.
//
// It returns 1 for λ=1, where the statistic is the last observation and no
// history is retained, and 0 for a lambda outside (0,1].
func EWMAMinPoints(lambda float64) int {
	if !finite(lambda) || lambda <= 0 || lambda > 1 {
		return 0
	}
	if lambda == 1 {
		return 1
	}
	n := int(math.Ceil(math.Log(EWMAResidualWeight) / math.Log(1-lambda)))
	if n < 1 {
		return 1
	}
	return n
}
