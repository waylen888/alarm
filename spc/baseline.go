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
// only as clean as the reference period: one spike in it inflates sigma
// enough to hide a real shift. Use TrailingRobust when that is a realistic
// worry, which for monitoring data it usually is.
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
