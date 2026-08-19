package spc

import (
	"fmt"
	"math"

	"github.com/waylen888/alarm"
)

// This is the only file in the package that imports alarm. Everything else
// operates on []float64 and knows nothing about windows, engines or events,
// so the statistics stay usable — and extractable — without the alerting
// engine. Keep it that way.

// Defaults substituted for arguments that are out of range in a way that has
// no nearest valid value.
const (
	// DefaultLambda is the smoothing factor used when the caller's lambda is
	// not a finite number above zero. A lambda above 1 is clamped to 1
	// instead, since that is a value in range. 0.2 with an L of 3 is the
	// conventional pairing for detecting shifts of roughly one sigma; the
	// design tables are Lucas and Saccucci, Technometrics 32(1), 1990.
	DefaultLambda = 0.2
	// DefaultL is the control limit width used when the caller's L is not a
	// finite positive number. 3 mirrors the three-sigma limits of a Shewhart
	// chart.
	DefaultL = 3
)

// Argument handling, applied consistently by both constructors:
//
// Constructors never return an error and never panic. An argument outside its
// valid range is clamped to the nearest value inside it; an argument whose
// range is open at the offending end, so that no nearest value exists, is
// replaced by the documented default above. Unknown rule identifiers are
// dropped, and a rule set left empty by that — or empty to begin with — is
// replaced by DefaultRules, rule 1 alone.
//
// The alternative — a condition that is silently and permanently false — is
// the worst failure mode an alerting library has. The engine does have a
// logger (alarm.WithLogf) and does warn at SetRules when a condition's
// MinPoints exceeds the window cap, so an invalid argument could have been
// encoded as an unsatisfiable point count to reach it. That was rejected:
// the resulting warning says a condition needs five thousand observations,
// which is not the same information as "your lambda was out of range", and
// it deliberately installs a rule that can never fire. Clamping keeps the
// condition working, and String reports the effective configuration, so a
// caller who logs the rule they built sees what was changed.

// conditionBase binds a baseline to the split between the reference
// observations and the observations under test. Both conditions embed it, and
// it is the single place the split is performed, which is what keeps the
// points under test out of their own baseline.
type conditionBase struct {
	b    Baseline
	ref  int // reference observations handed to the baseline
	test int // observations judged against the resulting limits
}

func newBase(b Baseline, ref, test int) conditionBase {
	ref = refPointsOf(b, ref)
	if max := alarm.MaxWindowPoints - test; ref > max {
		// The window cannot hold both. Clamping keeps MinPoints satisfiable;
		// a baseline that genuinely needs this many observations is beyond
		// what a window can supply and will report false forever, which is
		// visible in MinPoints reaching the cap.
		ref = max
	}
	if ref < 0 {
		// Reachable only if test alone exceeded the window, which neither
		// constructor allows. Keeping the floor here means the split below
		// can never be asked for a negative slice.
		ref = 0
	}
	return conditionBase{b: b, ref: ref, test: test}
}

// MinPoints is the reference size plus the observations under test. Both
// halves must fit: a window sized for the rules alone leaves the baseline
// unable to estimate, and the condition is then permanently false.
func (c conditionBase) MinPoints() int { return c.ref + c.test }

// split returns the reference observations and the observations under test,
// oldest first, together with the baseline estimated from the former. It
// reports false when the window is too short or the baseline cannot produce a
// usable estimate.
func (c conditionBase) split(w alarm.Window) (test []float64, centre, sigma float64, ok bool) {
	pts := w.LastN(c.ref + c.test)
	if len(pts) < c.ref+c.test {
		return nil, 0, 0, false
	}
	vals := make([]float64, len(pts))
	for i, p := range pts {
		vals[i] = p.Value
	}
	centre, sigma, ok = c.b.Estimate(vals[:c.ref])
	if !ok {
		return nil, 0, 0, false
	}
	return vals[c.ref:], centre, sigma, true
}

// Nelson breaches when one of the named Nelson rules completes at the most
// recent observation, judged against the limits b estimates from the ref
// observations preceding the ones under test.
//
// The rules are a required argument, and deliberately so. This constructor
// installs something that can wake a person, no rule set is quiet enough to
// be safe by default — the measured table in the package documentation gives
// the false-alarm rates, and none of them is safe on its own — and which rules a
// metric deserves is a judgement about that metric that the package is not in
// a position to make. Pass AllRules to apply the published procedure. Unknown
// rules are dropped, and an empty or wholly unknown set falls back to
// DefaultRules, because a condition that can never be true is the worse
// failure.
//
// Only rules completing at the newest observation breach. A rule that
// completed six observations ago has already been reported, and leaving it
// breaching would keep the alert up long after the process recovered — which
// is what Rule.ClearFor and Rule.For are for, and not something a condition
// should decide by being sticky.
//
// The statistic is recomputed from the window on every evaluation, so the
// condition holds no state and can be hot-swapped. Its memory is exactly the
// window's length: see the package documentation.
//
// ref is how many observations preceding the ones under test are handed to
// the baseline. A baseline that declares a larger requirement of its own —
// Trailing, TrailingRobust and TrailingRange all declare the n they were
// built with — overrides it, because honouring the smaller number would
// leave Estimate reporting false on every evaluation. So Nelson(Trailing(50),
// 10, ...) reads 50 reference observations, not 10. A baseline that declares
// nothing gets at least MinRefPoints.
//
// MinPoints is the largest Points() among the enabled rules plus the
// reference size, so a Nelson(Trailing(50), 50, []Rule{Rule7}) condition
// declares 65.
// Getting that wrong yields a condition that never breaches, so the two are
// added here rather than left to the caller.
func Nelson(b Baseline, ref int, rules []Rule) alarm.Condition {
	rules = knownRules(rules)
	test := 0
	for _, r := range rules {
		if n := r.Points(); n > test {
			test = n
		}
	}
	return nelson{conditionBase: newBase(b, ref, test), rules: rules}
}

type nelson struct {
	conditionBase
	rules []Rule
}

func (c nelson) Breach(w alarm.Window) bool {
	test, centre, sigma, ok := c.split(w)
	if !ok {
		return false
	}
	last := len(test) - 1
	for _, v := range Check(test, centre, sigma, c.rules...) {
		if v.Index == last {
			return true
		}
	}
	return false
}

// Measure reports the sigma distance of the most recent observation from the
// centre line. The last raw observation, which is the engine's default, says
// almost nothing about a rule that fired on a nine-point run; how far out the
// process is says a good deal.
func (c nelson) Measure(w alarm.Window) float64 {
	test, centre, sigma, ok := c.split(w)
	if !ok {
		return 0
	}
	return (test[len(test)-1] - centre) / sigma
}

// String reports the effective configuration, including any value that was
// clamped at construction. The constructors return an alarm.Condition, so
// this is the only way a caller can see that their lambda of 0.0001 became
// something else; fmt dispatches on the dynamic type, so it costs nothing to
// have and shows up in any log line or test failure that prints the rule.
func (c nelson) String() string {
	return fmt.Sprintf("spc.Nelson(ref=%d, points=%d, rules=%v)", c.ref, c.test, c.rules)
}

// knownRules drops unknown rule identifiers, removes duplicates and returns
// DefaultRules when nothing valid is left.
//
// The fallback used to be all eight, which meant a mistyped rule constant in
// a configuration file selected the noisiest condition the package can build.
// Input that was entirely wrong must never choose the loudest answer.
func knownRules(rules []Rule) []Rule {
	var out []Rule
	for _, r := range rules {
		if r.Valid() && !contains(out, r) {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return DefaultRules()
	}
	return out
}

// EWMA breaches when the exponentially weighted moving average of the most
// recent observations leaves the control limits L·σ·sqrt(λ/(2-λ)·(1-(1-λ)^2ⁿ))
// around the centre line b estimates from the ref observations preceding
// them.
//
// This is the tool for a small sustained shift: one that Nelson rule 1 will
// never see because it is nowhere near three sigma, and that rule 2 sees only
// nine observations after it started.
//
// ref is how many observations preceding the ones under test are handed to
// the baseline. A baseline that declares a larger requirement of its own —
// Trailing, TrailingRobust and TrailingRange all declare the n they were
// built with — overrides it, because honouring the smaller number would
// leave Estimate reporting false on every evaluation. So EWMA(Trailing(50),
// 10, ...) reads 50 reference observations, not 10. A baseline that declares
// nothing gets at least MinRefPoints.
//
// lambda selects how much history the statistic keeps, and MinPoints follows
// from it rather than being chosen: the condition judges EWMAMinPoints(lambda)
// observations, the fewest for which the truncation imposed by a bounded
// window costs less than EWMAResidualWeight. For the default lambda of 0.2
// that is 21 observations, and with a Trailing(50) baseline MinPoints is 71.
//
// lambda above 1 is clamped to 1, where the statistic is the last observation
// and no history is kept. A lambda so small that the derived point count would
// not fit in alarm.MaxWindowPoints alongside the reference observations is
// raised to the smallest one that does. A lambda that is not a finite number
// above zero becomes DefaultLambda, and an L that is not finite and positive
// becomes DefaultL.
func EWMA(b Baseline, ref int, lambda, L float64) alarm.Condition {
	lambda = clampLambda(lambda)
	if !finite(L) || L <= 0 {
		L = DefaultL
	}
	return ewma{
		conditionBase: newBase(b, ref, EWMAMinPoints(lambda)),
		lambda:        lambda,
		l:             L,
	}
}

type ewma struct {
	conditionBase
	lambda float64
	l      float64
}

func (c ewma) Breach(w alarm.Window) bool {
	test, centre, sigma, ok := c.split(w)
	if !ok {
		return false
	}
	half, ok := EWMAControlLimits(sigma, c.lambda, c.l, len(test))
	if !ok {
		return false
	}
	stat, ok := EWMAStat(test, c.lambda)
	if !ok {
		return false
	}
	return math.Abs(stat-centre) > half
}

// Measure reports the EWMA statistic itself, which is the quantity the
// judgement is about. The most recent raw observation is a sample of the noise
// the statistic exists to smooth away.
func (c ewma) Measure(w alarm.Window) float64 {
	test, _, _, ok := c.split(w)
	if !ok {
		return 0
	}
	stat, _ := EWMAStat(test, c.lambda)
	return stat
}

// String reports the effective configuration; see nelson.String.
func (c ewma) String() string {
	return fmt.Sprintf("spc.EWMA(ref=%d, points=%d, lambda=%g, L=%g)", c.ref, c.test, c.lambda, c.l)
}

// clampLambda brings lambda into the range this package can actually serve:
// (0,1], further bounded below by the smallest smoothing factor whose derived
// point count still fits inside a window.
func clampLambda(lambda float64) float64 {
	if !finite(lambda) || lambda <= 0 {
		lambda = DefaultLambda
	}
	if lambda > 1 {
		return 1
	}
	if floor := lambdaFloor(alarm.MaxWindowPoints - MinRefPoints); lambda < floor {
		return floor
	}
	return lambda
}

// lambdaFloor returns the smallest smoothing factor whose EWMAMinPoints is at
// most max, the exact inverse of that function. The correction loop absorbs
// the rounding in Log and Exp, which can leave the closed-form answer one
// point over the limit.
func lambdaFloor(max int) float64 {
	if max < 1 {
		return 1
	}
	l := 1 - math.Exp(math.Log(EWMAResidualWeight)/float64(max))
	for EWMAMinPoints(l) > max {
		l = math.Nextafter(l, 1)
	}
	return l
}
