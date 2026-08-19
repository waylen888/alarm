package spc

// Baseline supplies the centre line and dispersion a control chart is
// judged against.
//
// The observations under test are never passed to Estimate. That exclusion is
// structural: the caller splits the series once, hands the leading part here
// and judges the trailing part against the result. A sustained shift allowed
// into its own centre line drags the centre after it, and the chart goes
// blind to precisely the thing it exists to detect — the most common bug in
// SPC implementations. The interface is shaped so that mistake cannot be made
// locally.
type Baseline interface {
	// Estimate returns the centre line and standard deviation to judge the
	// points under test against, given the reference observations ref. It
	// reports false when it cannot produce a usable estimate — too few
	// observations, or no variance to speak of. A sigma of zero is never
	// returned with ok true, because every caller divides by it.
	Estimate(ref []float64) (centre, sigma float64, ok bool)
}

// MinRefPoints is the fewest reference observations a condition will ask a
// baseline for when the baseline does not declare its own requirement.
// Below two there is no dispersion to estimate, so a smaller number would
// buy a caller nothing but a condition that never breaches.
const MinRefPoints = 2

// RefSizer is an optional interface a Baseline may implement to declare how
// many reference observations it needs. A window sized without counting them
// leaves the condition permanently false, so the conditions in this package
// add the declared size to their MinPoints.
//
// A Baseline that does not implement it is assumed to need a dispersion
// estimate of its own, and so at least MinRefPoints observations. Declaring
// zero — as Fixed does — is how a baseline says it reads none.
type RefSizer interface {
	RefPoints() int
}

// refPointsOf reports how many reference observations to hand b, given the
// caller's requested ref. A baseline's own declared requirement wins when it
// is larger: honouring the smaller number would guarantee Estimate reports
// false on every evaluation. An undeclared baseline gets the MinRefPoints
// floor, since there is no way to tell whether it can work with fewer.
func refPointsOf(b Baseline, ref int) int {
	if ref < 0 {
		ref = 0
	}
	if s, ok := b.(RefSizer); ok {
		if n := s.RefPoints(); n > ref {
			return n
		}
		return ref
	}
	if ref < MinRefPoints {
		return MinRefPoints
	}
	return ref
}

// Fixed is a baseline of caller-supplied constants, from prior analysis of
// the metric. It ignores the reference observations entirely, which makes it
// deterministic and the easiest kind to reason about: the limits are the same
// today as they were last week, and a shift cannot erode them.
//
// It is the right default for a metric whose normal range is genuinely known.
// A non-positive or non-finite sigma yields a baseline that reports false on
// every evaluation, so no condition built on it can ever breach.
func Fixed(centre, sigma float64) Baseline { return fixed{centre: centre, sigma: sigma} }

type fixed struct {
	centre, sigma float64
}

// RefPoints is zero: Fixed reads no reference observations, so a condition
// built on it need not wait for any before it can judge.
func (b fixed) RefPoints() int { return 0 }

func (b fixed) Estimate([]float64) (float64, float64, bool) {
	if !finite(b.centre) || !finite(b.sigma) || b.sigma <= 0 {
		return 0, 0, false
	}
	return b.centre, b.sigma, true
}

// Trailing estimates the centre line and dispersion from the n observations
// preceding the points under test, as their mean and sample standard
// deviation. n below 2 is raised to 2, the fewest observations a sample
// standard deviation is defined over.
//
// The metric supplies its own normal range, which is what makes this usable
// on a series whose level nobody wrote down. The cost is that the estimate is
// only as clean as the reference period, and it is dirty in two different
// ways:
//
//   - One spike inflates sigma enough to hide a real shift. Use
//     TrailingRobust when that is a realistic worry, which for monitoring
//     data it usually is.
//   - Any drift, ramp or cycle inside the reference period is absorbed into
//     sigma as if it were noise, because a sample standard deviation is a
//     global measure of dispersion and cannot tell level variation from
//     scatter. On a metric with a daily cycle — the case this package exists
//     for — that inflation is present on essentially every evaluation, and it
//     desensitises every rule. Use TrailingRange, whose estimator is local
//     and does not see the level. TrailingRobust does not fix this: median
//     and MAD are equally global, and resist outliers rather than drift.
//
// The two failure modes want opposite estimators, and no baseline here
// survives both. Pick by which one the metric actually has: outliers in the
// reference period, or drift through it.
func Trailing(n int) Baseline { return trailing{n: atLeast(n, 2)} }

type trailing struct{ n int }

func (b trailing) RefPoints() int { return b.n }

func (b trailing) Estimate(ref []float64) (float64, float64, bool) {
	ref, ok := tail(ref, b.n)
	if !ok {
		return 0, 0, false
	}
	centre, ok := Mean(ref)
	if !ok {
		return 0, 0, false
	}
	sigma, ok := StdDev(ref)
	if !ok {
		return 0, 0, false
	}
	return centre, sigma, true
}

// TrailingRobust estimates the centre line and dispersion from the n
// observations preceding the points under test, as their median and their
// median absolute deviation scaled by MADScale. n below 2 is raised to 2.
//
// Both estimators have a 50% breakdown point, so up to half the reference
// period can be arbitrarily corrupted without moving the limits. That is the
// difference that matters: a reference period containing a single large
// outlier gives Trailing a sigma several times too wide, and a shift that
// should have been three sigma out lands comfortably inside the limits.
//
// The price is efficiency. On genuinely clean normal data MAD·MADScale is a
// noisier estimator of σ than the sample standard deviation, so Trailing
// detects marginally sooner. Reference periods drawn from production
// monitoring data are rarely clean.
//
// Two cautions. MAD·MADScale is biased low at small n — by roughly 10% at
// n=10 and 2% at n=50 — and no finite-sample correction is applied here, so a
// small n gives limits that are slightly too tight and a false-alarm rate
// slightly above nominal. Prefer n of 50 or more. And MAD is zero whenever
// more than half the reference observations share a value, which for a
// low-cardinality integer metric is the normal case rather than a corner one;
// Estimate then reports false and the condition is never true. See the
// package documentation on metrics these conditions do not work on.
func TrailingRobust(n int) Baseline { return trailingRobust{n: atLeast(n, 2)} }

type trailingRobust struct{ n int }

func (b trailingRobust) RefPoints() int { return b.n }

func (b trailingRobust) Estimate(ref []float64) (float64, float64, bool) {
	ref, ok := tail(ref, b.n)
	if !ok {
		return 0, 0, false
	}
	centre, ok := Median(ref)
	if !ok {
		return 0, 0, false
	}
	mad, ok := MAD(ref)
	if !ok {
		return 0, 0, false
	}
	sigma := mad * MADScale
	if !finite(sigma) || sigma <= 0 {
		return 0, 0, false
	}
	return centre, sigma, true
}

// TrailingRange estimates the centre line from the mean of the n
// observations preceding the points under test, and the dispersion from
// their mean moving range divided by MovingRangeScale. n below 2 is raised
// to 2.
//
// This is the classic individuals-chart estimator, and the reason to prefer
// it over Trailing is drift. A sample standard deviation is a global measure
// of dispersion: any drift, ramp or partial cycle inside the reference period
// is absorbed into it as though it were noise, and the resulting sigma can be
// several times the process's actual scatter, which desensitises every rule
// judged against it. On a metric with a daily cycle — the case this package
// exists for — that inflation is present on essentially every evaluation. A
// mean moving range looks only at consecutive differences and does not see
// the level, so it estimates the noise rather than the noise plus the cycle.
//
// It also survives two cases the other baselines do not. A single outlier
// contributes two large moving ranges out of n-1, where it contributes its
// whole squared deviation to a sample standard deviation, so this is
// considerably more resistant than Trailing though less so than
// TrailingRobust. And a low-cardinality integer metric — an error count that
// is mostly zero — has a moving range whenever any two adjacent observations
// differ, where its MAD is zero as soon as more than half the reference
// shares a value, which leaves TrailingRobust unable to estimate at all.
//
// The cost is sensitivity to autocorrelation. Consecutive observations of a
// monitoring time series resemble each other more than independent ones
// would, so the moving ranges are smaller than the process's dispersion
// warrants and the limits come out too narrow. Trailing has the opposite
// bias. Sampling at an interval longer than the metric's autocorrelation time
// mitigates it; there is no estimator here that does.
func TrailingRange(n int) Baseline { return trailingRange{n: atLeast(n, 2)} }

type trailingRange struct{ n int }

func (b trailingRange) RefPoints() int { return b.n }

func (b trailingRange) Estimate(ref []float64) (float64, float64, bool) {
	ref, ok := tail(ref, b.n)
	if !ok {
		return 0, 0, false
	}
	centre, ok := Mean(ref)
	if !ok {
		return 0, 0, false
	}
	mr, ok := MeanMovingRange(ref)
	if !ok {
		return 0, 0, false
	}
	sigma := mr / MovingRangeScale
	if !finite(sigma) || sigma <= 0 {
		return 0, 0, false
	}
	return centre, sigma, true
}

// tail returns the last n values of xs, reporting false when there are fewer
// than n. Taking the tail rather than the whole slice keeps the reference
// period adjacent to the points under test: a baseline drawn from an hour ago
// describes a process that may no longer exist.
func tail(xs []float64, n int) ([]float64, bool) {
	if len(xs) < n {
		return nil, false
	}
	return xs[len(xs)-n:], true
}

func atLeast(n, min int) int {
	if n < min {
		return min
	}
	return n
}
